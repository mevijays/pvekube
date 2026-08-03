package bootstrap

import (
	"os"
	"path/filepath"

	"pvekube/internal/jobs"
	"pvekube/internal/runner"
	"pvekube/internal/versions"
)

// clusterctlConfigYAML pins the provider images to our exact version bundle
// (see internal/versions) rather than trusting clusterctl's built-in
// registry, which defaults the proxmox/ipam entries to "releases/latest".
// EXP_CLUSTER_RESOURCE_SET and CLUSTER_TOPOLOGY are required by CAPMOX's
// own docs (CNI delivery via ClusterResourceSet, ClusterClass support).
func clusterctlConfigYAML() string {
	return `images:
  cluster-api:
    tag: ` + versions.Default.CAPICore + `
  infrastructure-proxmox:
    tag: ` + versions.Default.CAPMOX + `
providers:
  - name: "proxmox"
    url: "` + versions.ProxmoxProviderURL() + `"
    type: "InfrastructureProvider"
`
}

func writeClusterctlConfig(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, "clusterctl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "clusterctl.yaml")
	if err := os.WriteFile(path, []byte(clusterctlConfigYAML()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// InitSpec installs Cluster API core + kubeadm bootstrap/control-plane +
// CAPMOX (infrastructure-proxmox) + the in-cluster IPAM provider into the
// KIND management cluster, all pinned to the verified-compatible bundle.
//
// Env vars CAPMOX needs at init time (PROXMOX_URL etc.) aren't required
// here — those are only needed when generating a *workload cluster*
// manifest (clusterctl generate cluster), not when installing the
// controllers. We pass empty placeholders so `clusterctl init` doesn't warn
// about unset variables; real values are supplied later by internal/capi.
func InitSpec(clusterctlBin, kindBin, dataDir string) *jobs.Spec {
	kcPath := KubeconfigPath(dataDir)
	core := "cluster-api:" + versions.Default.CAPICore
	bootstrapP := "kubeadm:" + versions.Default.CAPICore
	controlPlane := "kubeadm:" + versions.Default.CAPICore
	infra := "proxmox:" + versions.Default.CAPMOX
	ipam := "in-cluster:" + versions.Default.IPAMInCluster

	return jobs.NewSpec("bootstrap.clusterctl_init", "Install Cluster API providers").
		Step("Write clusterctl.yaml (pinned provider versions)", func(c *jobs.Ctx) error {
			path, err := writeClusterctlConfig(dataDir)
			if err != nil {
				return err
			}
			c.Logf("Wrote %s pinning core=%s capmox=%s ipam=%s", path, versions.Default.CAPICore, versions.Default.CAPMOX, versions.Default.IPAMInCluster)
			return nil
		}).
		Step("clusterctl init", func(c *jobs.Ctx) error {
			cfgPath, _ := writeClusterctlConfig(dataDir)
			env := []string{
				"KUBECONFIG=" + kcPath,
				"CLUSTERCTL_CONFIG=" + cfgPath,
				// Required by CAPMOX for CNI delivery and ClusterClass support
				// (see docs/Usage.md) even though no workload cluster exists yet.
				"EXP_CLUSTER_RESOURCE_SET=true",
				"CLUSTER_TOPOLOGY=true",
				"EXP_KUBEADM_BOOTSTRAP_FORMAT_IGNITION=true",
			}
			return runner.Run(c, c, "", env, clusterctlBin, "init",
				"--core", core,
				"--bootstrap", bootstrapP,
				"--control-plane", controlPlane,
				"--infrastructure", infra,
				"--ipam", ipam,
				"--wait-providers")
		})
}

// ProvidersHealthy checks that every expected provider deployment is
// actually Available (not just that clusterctl init exited 0 — a
// deployment can be created but crash-looping).
func ProvidersHealthy(kubectlBin, dataDir string) (bool, string, error) {
	kcPath := KubeconfigPath(dataDir)
	// "clusterctl.cluster.x-k8s.io" (present, empty value) is the label
	// clusterctl itself stamps on every provider deployment it installs —
	// verified against a live cluster, not assumed from docs.
	out, err := runner.Capture(kubectlBin, "--kubeconfig", kcPath, "get", "deployments", "-A",
		"-l", "clusterctl.cluster.x-k8s.io", "-o",
		"jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}={.status.availableReplicas}{\"\\n\"}{end}")
	if err != nil {
		return false, "", err
	}
	if out == "" {
		return false, "no provider deployments found", nil
	}
	for _, line := range splitLines(out) {
		idx := lastIndexByte(line, '=')
		if idx < 0 {
			continue
		}
		count := line[idx+1:]
		if count == "" || count == "0" {
			return false, line + " has 0 available replicas", nil
		}
	}
	return true, out, nil
}
