// Package bootstrap creates and verifies the local KIND management cluster
// that hosts Cluster API's controllers, and drives clusterctl to install
// CAPI core + CAPMOX (Proxmox infrastructure provider) + the in-cluster IPAM
// provider into it, at the exact pinned versions in internal/versions.
//
// This is the "management cluster" in Cluster API terms: it never runs
// workloads itself, it just reconciles the CRDs that describe workload
// clusters on Proxmox. Per PLAN.md this stays a permanent KIND cluster for
// now (the user chose not to pivot to a self-managed cluster).
package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"

	"pvekube/internal/jobs"
	"pvekube/internal/runner"
)

const ClusterName = "pvekube"

// KindConfigYAML is the generated KIND cluster definition. Single control
// plane node is sufficient — this cluster only runs CAPI controllers, not
// user workloads, so it doesn't need HA. Exposed as a named function (not
// just an embedded string) because "generate kind cluster files" was
// explicitly requested as a visible, inspectable prerequisite step, not a
// hidden implementation detail.
func KindConfigYAML() string {
	return `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: ` + ClusterName + `
nodes:
  - role: control-plane
    kubeadmConfigPatches:
      - |
        kind: InitConfiguration
        nodeRegistration:
          kubeletExtraArgs:
            node-labels: "pvekube.io/role=management"
`
}

// WriteKindConfig writes the generated config to dataDir/kind/kind-config.yaml
// and returns its path. Called both by the prereq fix job and exposed via a
// download link so a curious user can see exactly what will run.
func WriteKindConfig(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, "kind")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "kind-config.yaml")
	if err := os.WriteFile(path, []byte(KindConfigYAML()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// KubeconfigPath is where the management cluster's kubeconfig is exported to
// — kept inside our own data dir rather than touching the user's ~/.kube,
// since this is a headless appliance, not a developer workstation.
func KubeconfigPath(dataDir string) string {
	return filepath.Join(dataDir, "kubeconfigs", "management.yaml")
}

// IsClusterRunning does a cheap, side-effect-free check: does a KIND cluster
// named ClusterName currently exist?
func IsClusterRunning(kindBin string) (bool, error) {
	out, err := runCapture(kindBin, "get", "clusters")
	if err != nil {
		return false, err
	}
	for _, line := range splitLines(out) {
		if line == ClusterName {
			return true, nil
		}
	}
	return false, nil
}

// CreateClusterSpec is the job that generates the kind config and creates
// the management cluster.
func CreateClusterSpec(kindBin, dataDir string) *jobs.Spec {
	return jobs.NewSpec("bootstrap.kind_create", "Create local management cluster (KIND)").
		Step("Generate kind-config.yaml", func(c *jobs.Ctx) error {
			path, err := WriteKindConfig(dataDir)
			if err != nil {
				return err
			}
			c.Logf("Wrote %s", path)
			return nil
		}).
		Step("Create KIND cluster", func(c *jobs.Ctx) error {
			cfgPath, _ := WriteKindConfig(dataDir)
			kcPath := KubeconfigPath(dataDir)
			if err := os.MkdirAll(filepath.Dir(kcPath), 0o755); err != nil {
				return err
			}
			return runner.Run(c, c, "", nil, kindBin, "create", "cluster",
				"--config", cfgPath, "--kubeconfig", kcPath, "--wait", "3m")
		})
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func runCapture(name string, args ...string) (string, error) {
	out, err := runner.Capture(name, args...)
	if err != nil {
		return "", fmt.Errorf("%s %v: %w", name, args, err)
	}
	return out, nil
}
