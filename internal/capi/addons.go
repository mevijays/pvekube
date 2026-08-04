// Post-provision addon installers: metrics-server, Istio, MetalLB. All three
// share the same shape as Cilium's CNI install (see EnsureCNIStep) — none of
// them can run until the workload cluster's API server is actually
// reachable, which is well after "kubectl apply" returns, so each waits on
// waitForWorkloadKubeconfig before doing anything.
package capi

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pvekube/internal/bootstrap"
	"pvekube/internal/jobs"
	"pvekube/internal/runner"
	"pvekube/internal/versions"
)

// AddonSelection is which post-provision addons to install, collected from
// the cluster creation form alongside the CNI choice.
type AddonSelection struct {
	MetricsServer bool
	Istio         bool
	MetalLB       bool
	MetalLBIPPool string // e.g. "10.10.10.90-10.10.10.99" or CIDR, required if MetalLB is true
}

type nodeStatusJSON struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// WaitForNodesReadyStep polls until all expected control plane and worker nodes
// are present and in Ready status on the workload cluster. This ensures CNI,
// cloud-init, and node bootstrapping are 100% complete before any post-provision
// addons are applied or the apply job finishes.
func WaitForNodesReadyStep(dataDir, binDir, clusterName string) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		kcPath, cleanup, err := waitForWorkloadKubeconfig(c, dataDir, binDir, clusterName, "node provisioning & CNI readiness")
		if err != nil {
			return err
		}
		defer cleanup()

		kubectlBin := filepath.Join(binDir, "kubectl")
		kcPathMgmt := bootstrap.KubeconfigPath(dataDir)

		// Get expected replica counts from management cluster
		expectedCP := 1
		if out, err := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPathMgmt,
			"get", "kubeadmcontrolplane", clusterName+"-control-plane",
			"-o", "jsonpath={.spec.replicas}").Output(); err == nil {
			fmt.Sscanf(string(out), "%d", &expectedCP)
		}
		if expectedCP <= 0 {
			expectedCP = 1
		}

		expectedWorkers := 0
		if out, err := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPathMgmt,
			"get", "machinedeployment", clusterName+"-workers",
			"-o", "jsonpath={.spec.replicas}").Output(); err == nil {
			fmt.Sscanf(string(out), "%d", &expectedWorkers)
		}
		if expectedWorkers < 0 {
			expectedWorkers = 0
		}

		totalExpected := expectedCP + expectedWorkers
		c.Logf("Waiting for %s nodes to be provisioned and Ready (expected: %d control-plane, %d worker(s))...",
			clusterName, expectedCP, expectedWorkers)

		deadline := time.Now().Add(30 * time.Minute)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			out, err := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath,
				"get", "nodes", "-o", "json", "--request-timeout=10s").Output()
			if err == nil {
				var nlist nodeStatusJSON
				if json.Unmarshal(out, &nlist) == nil {
					readyCount := 0
					var nodeSummaries []string

					for _, item := range nlist.Items {
						isReady := false
						for _, cond := range item.Status.Conditions {
							if cond.Type == "Ready" && cond.Status == "True" {
								isReady = true
								break
							}
						}
						if isReady {
							readyCount++
							nodeSummaries = append(nodeSummaries, fmt.Sprintf("%s (Ready)", item.Metadata.Name))
						} else {
							nodeSummaries = append(nodeSummaries, fmt.Sprintf("%s (NotReady/initializing)", item.Metadata.Name))
						}
					}

					c.Logf("Nodes status (%d/%d Ready): %s", readyCount, totalExpected, strings.Join(nodeSummaries, ", "))

					if len(nlist.Items) >= totalExpected && readyCount >= totalExpected {
						c.Logf("✓ All %d node(s) are online and Ready! Network & CNI fully operational.", totalExpected)
						return nil
					}
				}
			} else {
				c.Logf("Querying workload nodes... (API server re-establishing connection)")
			}

			if time.Now().After(deadline) {
				return fmt.Errorf("timed out after 30 minutes waiting for all %d node(s) to become Ready on %s", totalExpected, clusterName)
			}

			select {
			case <-c.Done():
				return c.Err()
			case <-ticker.C:
			}
		}
	}
}

// waitForWorkloadKubeconfig polls (bounded, 30 minutes) until the workload
// cluster's kubeconfig Secret exists AND the cluster's API server is stably
// accepting connections (5 consecutive successful checks 3s apart).
func waitForWorkloadKubeconfig(c *jobs.Ctx, dataDir, binDir, clusterName, activity string) (path string, cleanup func(), err error) {
	kubectlBin := filepath.Join(binDir, "kubectl")
	deadline := time.Now().Add(30 * time.Minute)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	// Gate 1: wait for the kubeconfig Secret to be published by CAPI.
	c.Logf("Waiting for %s's kubeconfig Secret to appear (control plane initializing)...", clusterName)
	var kubeconfig []byte
	for {
		kc, err := GetWorkloadKubeconfig(c, dataDir, binDir, clusterName)
		if err == nil {
			kubeconfig = kc
			c.Logf("Kubeconfig Secret is available — verifying API server stability...")
			break
		}
		if time.Now().After(deadline) {
			return "", nil, fmt.Errorf("cluster %s's kubeconfig Secret never appeared after 30 minutes — check that VMs booted and joined the management cluster", clusterName)
		}
		select {
		case <-c.Done():
			return "", nil, c.Err()
		case <-ticker.C:
			c.Logf("Still waiting for %s kubeconfig... (VMs may still be booting)", clusterName)
		}
	}

	// Write the kubeconfig to a temp file so kubectl can use it.
	kcPath, cleanupFn, err := writeTempKubeconfig(dataDir, kubeconfig)
	if err != nil {
		return "", nil, err
	}

	// Gate 2: wait until API server is consistently reachable (5 consecutive successful checks 3s apart)
	c.Logf("Waiting for %s's API server to stabilize before %s...", clusterName, activity)
	apiTicker := time.NewTicker(15 * time.Second)
	defer apiTicker.Stop()
	consecutiveSuccesses := 0
	for {
		out, err := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath,
			"get", "nodes", "--request-timeout=10s").CombinedOutput()
		if err == nil {
			consecutiveSuccesses++
			if consecutiveSuccesses >= 5 {
				c.Logf("API server is stable — proceeding with %s", activity)
				return kcPath, cleanupFn, nil
			}
			time.Sleep(3 * time.Second)
			continue
		}

		consecutiveSuccesses = 0
		if time.Now().After(deadline) {
			cleanupFn()
			return "", nil, fmt.Errorf("cluster %s's API server never stabilized after 30 minutes — cannot proceed with %s. Last error: %s", clusterName, activity, strings.TrimSpace(string(out)))
		}

		errMsg := strings.TrimSpace(string(out))
		if len(errMsg) > 120 {
			errMsg = errMsg[:120] + "..."
		}
		c.Logf("API not stable yet (%s) — retrying in 15s...", errMsg)
		select {
		case <-c.Done():
			cleanupFn()
			return "", nil, c.Err()
		case <-apiTicker.C:
		}
	}
}

// writeTempKubeconfig writes kubeconfig bytes to a temp file under dataDir
// and returns its path plus a cleanup func to remove it.
func writeTempKubeconfig(dataDir string, kubeconfig []byte) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp(dataDir, "kubeconfig-*.yaml")
	if err != nil {
		return "", nil, err
	}
	if _, err := f.Write(kubeconfig); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, err
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// InstallMetricsServerStep applies the pinned metrics-server manifest, then
// patches in --kubelet-insecure-tls: kubeadm-bootstrapped nodes (like every
// one PVEKube creates) use self-signed kubelet serving certs, which
// metrics-server rejects by default — without this patch it runs but every
// `kubectl top` call fails with a TLS error, a well-known kubeadm+
// metrics-server gotcha, not something specific to this cluster.
func InstallMetricsServerStep(dataDir, binDir, clusterName string) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		kcPath, cleanup, err := waitForWorkloadKubeconfig(c, dataDir, binDir, clusterName, "installing metrics-server")
		if err != nil {
			return err
		}
		defer cleanup()

		kubectlBin := filepath.Join(binDir, "kubectl")
		c.Logf("Applying metrics-server %s", versions.MetricsServerVersion)
		if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
			"apply", "-f", versions.MetricsServerManifestURL()); err != nil {
			return fmt.Errorf("applying metrics-server: %w", err)
		}

		c.Logf("Patching --kubelet-insecure-tls (required for kubeadm-issued self-signed kubelet certs)")
		patch := `[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]`
		if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
			"patch", "deployment", "metrics-server", "-n", "kube-system", "--type=json", "-p", patch); err != nil {
			return fmt.Errorf("patching metrics-server for insecure kubelet TLS: %w", err)
		}
		c.Logf("metrics-server installed")
		return nil
	}
}

// InstallIstioStep runs `istioctl install` with the default profile via
// istioctl (bundled Helm-driven install, no separate system Helm needed).
func InstallIstioStep(dataDir, binDir, clusterName string) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		kcPath, cleanup, err := waitForWorkloadKubeconfig(c, dataDir, binDir, clusterName, "installing Istio")
		if err != nil {
			return err
		}
		defer cleanup()

		istioctlBin := filepath.Join(binDir, "istioctl")
		c.Logf("Installing Istio %s (default profile) via istioctl", versions.IstioctlVersion)
		return runner.Run(c, c, "", nil, istioctlBin, "install", "--set", "profile=default", "-y", "--kubeconfig", kcPath)
	}
}

// InstallMetalLBStep applies the pinned MetalLB manifest, waits for its
// controller to be ready (required before its CRDs are usable — creating an
// IPAddressPool too early errors with "no matches for kind"), then creates
// an IPAddressPool over ipPool and a matching L2Advertisement so the pool is
// actually usable, not just installed.
func InstallMetalLBStep(dataDir, binDir, clusterName, ipPool string) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		kcPath, cleanup, err := waitForWorkloadKubeconfig(c, dataDir, binDir, clusterName, "installing MetalLB")
		if err != nil {
			return err
		}
		defer cleanup()

		kubectlBin := filepath.Join(binDir, "kubectl")
		c.Logf("Applying MetalLB %s", versions.MetalLBVersion)
		if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
			"apply", "-f", versions.MetalLBManifestURL()); err != nil {
			return fmt.Errorf("applying MetalLB: %w", err)
		}

		c.Logf("Waiting for MetalLB's controller to be ready before configuring its IP pool...")
		if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
			"wait", "--for=condition=Available", "deployment/controller", "-n", "metallb-system", "--timeout=180s"); err != nil {
			return fmt.Errorf("waiting for MetalLB controller: %w", err)
		}

		poolYAML := fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: pvekube-pool
  namespace: metallb-system
spec:
  addresses:
  - %s
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: pvekube-l2
  namespace: metallb-system
spec:
  ipAddressPools:
  - pvekube-pool
`, ipPool)

		f, err := os.CreateTemp(dataDir, "metallb-pool-*.yaml")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		if _, err := f.WriteString(poolYAML); err != nil {
			f.Close()
			return err
		}
		f.Close()

		c.Logf("Creating IPAddressPool over %s", ipPool)
		if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath, "apply", "-f", f.Name()); err != nil {
			return fmt.Errorf("creating MetalLB IPAddressPool: %w", err)
		}
		c.Logf("MetalLB installed with pool %s", ipPool)
		return nil
	}
}

// AddonSteps appends one job Step per selected addon onto spec, each running
// only after the CNI step (Kubernetes services generally need networking to
// come up first, and this keeps installs from racing each other over the
// same API server before it's warmed up).
func AddonSteps(spec *jobs.Spec, dataDir, binDir, clusterName string, addons AddonSelection) *jobs.Spec {
	if addons.MetricsServer {
		spec.Step("Install metrics-server", InstallMetricsServerStep(dataDir, binDir, clusterName))
	}
	if addons.Istio {
		spec.Step("Install Istio", InstallIstioStep(dataDir, binDir, clusterName))
	}
	if addons.MetalLB {
		spec.Step("Install MetalLB", InstallMetalLBStep(dataDir, binDir, clusterName, addons.MetalLBIPPool))
	}
	return spec
}
