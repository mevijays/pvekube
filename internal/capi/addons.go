// Post-provision addon installers: metrics-server, Istio, MetalLB. All three
// share the same shape as Cilium's CNI install (see EnsureCNIStep) — none of
// them can run until the workload cluster's API server is actually
// reachable, which is well after "kubectl apply" returns, so each waits on
// waitForWorkloadKubeconfig before doing anything.
package capi

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

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

// waitForWorkloadKubeconfig polls (bounded, 15 minutes) until the workload
// cluster's kubeconfig Secret exists — CAPI only publishes it once the
// control plane's API server is actually reachable — then writes it to a
// temp file for callers to pass as --kubeconfig. The caller must call the
// returned cleanup func once done with it.
func waitForWorkloadKubeconfig(c *jobs.Ctx, dataDir, binDir, clusterName, activity string) (path string, cleanup func(), err error) {
	deadline := time.Now().Add(15 * time.Minute)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	var kubeconfig []byte
	c.Logf("Waiting for %s's control plane API to become reachable before %s...", clusterName, activity)
	for {
		kc, err := GetWorkloadKubeconfig(c, dataDir, binDir, clusterName)
		if err == nil {
			kubeconfig = kc
			break
		}
		if time.Now().After(deadline) {
			return "", nil, fmt.Errorf("cluster %s's API never became reachable after 15 minutes — cannot proceed with %s", clusterName, activity)
		}
		select {
		case <-c.Done():
			return "", nil, c.Err()
		case <-ticker.C:
		}
	}

	return writeTempKubeconfig(dataDir, kubeconfig)
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
