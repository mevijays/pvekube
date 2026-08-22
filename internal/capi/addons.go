// Post-provision addon installers: metrics-server, Istio, MetalLB. All three
// share the same shape as Cilium's CNI install (see EnsureCNIStep) — none of
// them can run until the workload cluster's API server is actually
// reachable, which is well after "kubectl apply" returns, so each waits on
// waitForWorkloadKubeconfig before doing anything.
package capi

import (
	"bytes"
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

	// GitOps installs Flux and points it at a Git repository, so everything
	// else the cluster should run can be declared in that repo rather than
	// applied by hand afterwards. See InstallGitOpsStep.
	GitOps         bool
	GitOpsRepoURL  string // https://... or ssh://... — Flux rejects scp-style git@host:repo
	GitOpsBranch   string // defaults to "main"
	GitOpsPath     string // path within the repo to reconcile; defaults to "./"
	GitOpsUsername string // optional, private repos over HTTPS
	GitOpsToken    string // optional, the password/PAT half of the above — SECRET
	GitOpsCACert   string // optional PEM, for a Git host using an internal CA
}

// gitOpsBranchOrDefault / gitOpsPathOrDefault keep the defaults in one place
// so the installed Kustomization and the UI preview can't disagree.
func (a AddonSelection) gitOpsBranchOrDefault() string {
	if b := strings.TrimSpace(a.GitOpsBranch); b != "" {
		return b
	}
	return "main"
}

func (a AddonSelection) gitOpsPathOrDefault() string {
	if p := strings.TrimSpace(a.GitOpsPath); p != "" {
		return p
	}
	return "./"
}

// GitOpsNeedsAuth reports whether a credentials Secret has to be created for
// the repository (i.e. it is private, or served by an internal CA).
func (a AddonSelection) GitOpsNeedsAuth() bool {
	return strings.TrimSpace(a.GitOpsToken) != "" || strings.TrimSpace(a.GitOpsCACert) != ""
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
	// Last on purpose: whatever the Git repository deploys may expect the
	// other addons to already exist (a Service of type LoadBalancer needs
	// MetalLB, a sidecar-injected Deployment needs Istio). Flux reconciles
	// asynchronously so this is not a hard guarantee, but it removes the
	// obvious race rather than leaving it to chance.
	if addons.GitOps {
		spec.Step("Install GitOps (Flux)", InstallGitOpsStep(dataDir, binDir, clusterName, addons))
	}
	return spec
}

// gitOpsNamespace / gitOpsResourceName are fixed: one PVEKube-managed GitOps
// source per cluster. Names are stable across re-applies so the step is
// idempotent rather than accumulating duplicates.
const (
	gitOpsNamespace    = "flux-system"
	gitOpsResourceName = "pvekube-gitops"
	gitOpsSecretName   = "pvekube-gitops-auth"
)

// InstallGitOpsStep installs Flux and points it at the operator's Git
// repository, so the cluster continues configuring itself from that repo
// after PVEKube's job finishes.
//
// This deliberately does NOT use `flux bootstrap`. Bootstrap commits Flux's
// own manifests into the target repository and therefore needs write-scoped
// credentials — a surprising side effect for a "create cluster" action, and
// a much larger grant than this needs. Applying the pinned install.yaml and
// creating the GitRepository/Kustomization directly achieves the same
// outcome with read-only access, no commits into someone else's repo, and
// no extra CLI binary to download and pin (it reuses kubectl, exactly like
// the MetalLB and metrics-server addons).
func InstallGitOpsStep(dataDir, binDir, clusterName string, addons AddonSelection) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		kcPath, cleanup, err := waitForWorkloadKubeconfig(c, dataDir, binDir, clusterName, "installing Flux (GitOps)")
		if err != nil {
			return err
		}
		defer cleanup()

		kubectlBin := filepath.Join(binDir, "kubectl")

		// install.yaml carries the flux-system namespace, every CRD and all
		// controllers. A plain apply is enough here — see FluxManifestURL's
		// comment on why this needs no --server-side.
		c.Logf("Applying Flux %s", versions.FluxVersion)
		if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
			"apply", "-f", versions.FluxManifestURL()); err != nil {
			return fmt.Errorf("applying Flux: %w", err)
		}

		// source-controller fetches the repo, kustomize-controller applies
		// it; both must exist before the CRs below mean anything.
		for _, deploy := range []string{"source-controller", "kustomize-controller"} {
			c.Logf("Waiting for %s to become available...", deploy)
			if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"wait", "--for=condition=Available", "deployment/"+deploy,
				"-n", gitOpsNamespace, "--timeout=300s"); err != nil {
				return fmt.Errorf("waiting for Flux's %s: %w", deploy, err)
			}
		}

		if addons.GitOpsNeedsAuth() {
			if err := createGitOpsSecret(c, kubectlBin, kcPath, addons); err != nil {
				return err
			}
		}

		if err := applyGitOpsSource(c, dataDir, kubectlBin, kcPath, addons); err != nil {
			return err
		}

		return waitForGitOpsSync(c, kubectlBin, kcPath, addons)
	}
}

// createGitOpsSecret writes the repository credentials into flux-system.
//
// Nothing here goes through runner.Run, which echoes the command it runs
// into the job log — that would put the Git token into a file on disk and
// stream it to the browser. Same reasoning, and same shape, as
// InstallRegistryCredentialsStep in registry.go.
//
// Key names are Flux's, not ours, and are easy to get subtly wrong:
// username/password for HTTPS basic auth (a PAT goes in "password"), and
// "ca.crt" for a custom CA — verified against Flux's GitRepository API
// docs rather than recalled.
func createGitOpsSecret(c *jobs.Ctx, kubectlBin, kcPath string, addons AddonSelection) error {
	args := []string{"--kubeconfig", kcPath, "create", "secret", "generic", gitOpsSecretName,
		"-n", gitOpsNamespace, "--dry-run=client", "-o", "yaml"}

	if tok := strings.TrimSpace(addons.GitOpsToken); tok != "" {
		user := strings.TrimSpace(addons.GitOpsUsername)
		if user == "" {
			// Most Git forges ignore the username for token auth but still
			// require the field to be non-empty for basic auth to be sent.
			user = "git"
		}
		args = append(args, "--from-literal=username="+user, "--from-literal=password="+tok)
	}

	var caFile string
	if ca := strings.TrimSpace(addons.GitOpsCACert); ca != "" {
		f, err := os.CreateTemp("", "gitops-ca-*.pem")
		if err != nil {
			return err
		}
		caFile = f.Name()
		defer os.Remove(caFile)
		if _, err := f.WriteString(ca + "\n"); err != nil {
			f.Close()
			return err
		}
		f.Close()
		args = append(args, "--from-file=ca.crt="+caFile)
	}

	var rendered bytes.Buffer
	createCmd := exec.CommandContext(c, kubectlBin, args...)
	createCmd.Stdout = &rendered
	var createErr bytes.Buffer
	createCmd.Stderr = &createErr
	if err := createCmd.Run(); err != nil {
		return fmt.Errorf("rendering GitOps credentials Secret: %w\n%s", err, createErr.String())
	}

	applyCmd := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath, "apply", "-f", "-")
	applyCmd.Stdin = bytes.NewReader(rendered.Bytes())
	var applyOut bytes.Buffer
	applyCmd.Stdout, applyCmd.Stderr = &applyOut, &applyOut
	if err := applyCmd.Run(); err != nil {
		return fmt.Errorf("applying GitOps credentials Secret: %w\n%s", err, applyOut.String())
	}
	c.Logf("Secret %s/%s created for the Git repository", gitOpsNamespace, gitOpsSecretName)
	return nil
}

// applyGitOpsSource creates the GitRepository (what to fetch) and the
// Kustomization (what to apply from it).
//
// targetNamespace is deliberately NOT set: leaving it unset lets the
// repository's own manifests declare their namespaces, which is what an
// operator putting "all the possible app deployments" in a repo expects.
// Setting it would force every object into a single namespace instead.
func applyGitOpsSource(c *jobs.Ctx, dataDir, kubectlBin, kcPath string, addons AddonSelection) error {
	secretRef := ""
	if addons.GitOpsNeedsAuth() {
		secretRef = fmt.Sprintf("\n  secretRef:\n    name: %s", gitOpsSecretName)
	}

	// prune: true garbage-collects objects this Kustomization previously
	// applied but that have since left the repo — scoped to its own
	// inventory, so it can never touch anything PVEKube or another addon
	// installed.
	srcYAML := fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: %s
  namespace: %s
spec:
  interval: 1m
  url: %s
  ref:
    branch: %s%s
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %s
  namespace: %s
spec:
  interval: 10m
  path: %q
  prune: true
  sourceRef:
    kind: GitRepository
    name: %s
`, gitOpsResourceName, gitOpsNamespace, addons.GitOpsRepoURL, addons.gitOpsBranchOrDefault(), secretRef,
		gitOpsResourceName, gitOpsNamespace, addons.gitOpsPathOrDefault(), gitOpsResourceName)

	f, err := os.CreateTemp(dataDir, "gitops-source-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(srcYAML); err != nil {
		f.Close()
		return err
	}
	f.Close()

	c.Logf("Pointing Flux at %s (branch %s, path %s)", addons.GitOpsRepoURL, addons.gitOpsBranchOrDefault(), addons.gitOpsPathOrDefault())
	if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath, "apply", "-f", f.Name()); err != nil {
		return fmt.Errorf("creating GitRepository/Kustomization: %w", err)
	}
	return nil
}

// waitForGitOpsSync blocks until Flux has actually fetched the repository.
//
// This is the difference between a job that reports success and a cluster
// that really is wired up. Everything before this point succeeds even when
// the URL is wrong, the token is invalid, the branch doesn't exist, or the
// Git host can't be resolved from inside the cluster — the CRs apply fine,
// and the failure only ever appears in a controller's status field that
// nobody thinks to look at. Waiting on Ready surfaces it here, and the
// condition message Flux sets says exactly which of those it was.
func waitForGitOpsSync(c *jobs.Ctx, kubectlBin, kcPath string, addons AddonSelection) error {
	c.Logf("Waiting for Flux to fetch the repository (this is where a bad URL, token or DNS failure shows up)...")
	err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
		"wait", "--for=condition=Ready", "gitrepository/"+gitOpsResourceName,
		"-n", gitOpsNamespace, "--timeout=120s")
	if err == nil {
		c.Logf("✓ Flux is syncing %s — anything committed under %s will now be applied automatically.",
			addons.GitOpsRepoURL, addons.gitOpsPathOrDefault())
		return nil
	}

	// Surface Flux's own diagnosis rather than just "timed out".
	msg, statusErr := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath,
		"get", "gitrepository", gitOpsResourceName, "-n", gitOpsNamespace,
		"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].message}`).Output()
	detail := strings.TrimSpace(string(msg))
	if statusErr != nil || detail == "" {
		detail = "no status reported yet"
	}
	c.Logf("Flux could not fetch the repository: %s", detail)
	c.Logf("Flux is installed and will keep retrying on its own; fix the cause and it will sync without recreating the cluster.")
	return fmt.Errorf("Flux could not fetch %s: %s", addons.GitOpsRepoURL, detail)
}
