package capi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pvekube/internal/bootstrap"
	"pvekube/internal/jobs"
	"pvekube/internal/runner"
)

// ScaleWorkersSpec changes a MachineDeployment's replica count, then waits
// for Cluster API to actually deliver it.
//
// The patch is the whole request — scaling on CAPI is an intent, not a
// procedure — but it returns in milliseconds while the real work (cloning a
// Proxmox VM, booting it, joining it to the cluster) takes minutes. Reporting
// "succeeded" on the patch alone made the UI flash Done with an empty log
// while a node was still provisioning, which reads as "nothing happened".
// Same reasoning as DeleteClusterSpec's teardown wait: a job finishing must
// mean the infrastructure changed, not that a request was accepted.
func ScaleWorkersSpec(dataDir, binDir, clusterName string, replicas int) *jobs.Spec {
	mdName := clusterName + "-workers"
	return jobs.NewSpec("cluster.scale_workers", fmt.Sprintf("Scale %s workers to %d", clusterName, replicas)).
		Step("kubectl patch machinedeployment", func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
			return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"patch", "machinedeployment", mdName, "--type=merge", "-p", patch)
		}).
		Step("Wait for workers to reach the new count",
			waitForScaleStep(dataDir, binDir, clusterName, "machinedeployment", mdName, replicas))
}

// ScaleControlPlaneSpec changes a KubeadmControlPlane's replica count.
// CAPMOX's own templates only ever use 1 or 3 (see cluster-template.yaml);
// enforcing that here rather than in the UI keeps the constraint in one
// place, next to the resource it actually applies to.
//
// Like ScaleWorkersSpec, this waits for the change to land rather than
// finishing on the patch. Control plane scaling is slower still: CAPI adds
// etcd members one at a time.
func ScaleControlPlaneSpec(dataDir, binDir, clusterName string, replicas int) *jobs.Spec {
	kcpName := clusterName + "-control-plane"
	return jobs.NewSpec("cluster.scale_controlplane", fmt.Sprintf("Scale %s control plane to %d", clusterName, replicas)).
		Step("kubectl patch kubeadmcontrolplane", func(c *jobs.Ctx) error {
			if replicas != 1 && replicas != 3 && replicas != 5 {
				return fmt.Errorf("control plane count must be 1, 3, or 5 for quorum — got %d", replicas)
			}
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
			return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"patch", "kubeadmcontrolplane", kcpName, "--type=merge", "-p", patch)
		}).
		Step("Wait for control plane to reach the new count",
			waitForScaleStep(dataDir, binDir, clusterName, "kubeadmcontrolplane", kcpName, replicas))
}

// waitForScaleStep polls a MachineDeployment or KubeadmControlPlane until
// both its total and ready replica counts equal want, logging each machine's
// phase on every tick so the job log shows real progress (which VM is
// provisioning, which has joined) instead of sitting blank.
//
// Works for scale-down as well: the same condition holds once surplus
// machines are gone and their Proxmox VMs deleted.
func waitForScaleStep(dataDir, binDir, clusterName, kind, resourceName string, want int) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		kubectlBin := filepath.Join(binDir, "kubectl")
		kcPath := bootstrap.KubeconfigPath(dataDir)

		c.Logf("Patch accepted — Cluster API is reconciling %s to %d replica(s).", resourceName, want)
		c.Logf("Adding a node clones a Proxmox VM, boots it and joins it to the cluster; expect a few minutes per node.")

		deadline := time.Now().Add(30 * time.Minute)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			total, terr := replicaCount(c, kubectlBin, kcPath, kind, resourceName, "{.status.replicas}")
			ready, rerr := replicaCount(c, kubectlBin, kcPath, kind, resourceName, "{.status.readyReplicas}")

			if terr == nil && rerr == nil && total == want && ready == want {
				c.Logf("✓ %s now has %d/%d replica(s) ready", resourceName, ready, want)
				return nil
			}

			if terr != nil || rerr != nil {
				c.Logf("Waiting for %s status to become readable...", resourceName)
			} else {
				c.Logf("Progress: %d/%d ready (%d machine object(s) exist, target %d)", ready, want, total, want)
				logMachinePhases(c, kubectlBin, kcPath, clusterName)
			}

			if time.Now().After(deadline) {
				return fmt.Errorf("%s still has %d/%d replicas ready after 30 minutes — scaling may be stuck; check `kubectl describe %s %s` and the CAPMOX controller logs on the management cluster",
					resourceName, ready, want, kind, resourceName)
			}

			select {
			case <-c.Done():
				return c.Err()
			case <-ticker.C:
			}
		}
	}
}

// replicaCount reads an integer status field. An empty result means the field
// is absent, which Kubernetes uses for zero (e.g. readyReplicas is omitted
// rather than set to 0), so that is reported as 0 rather than an error.
func replicaCount(c *jobs.Ctx, kubectlBin, kcPath, kind, name, jsonPath string) (int, error) {
	out, err := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath,
		"get", kind, name, "-o", "jsonpath="+jsonPath, "--request-timeout=10s").Output()
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parsing %s of %s/%s: %w", jsonPath, kind, name, err)
	}
	return n, nil
}

// logMachinePhases prints one line per machine, including the Ready
// condition's message for any machine that isn't up yet.
//
// That message is where CAPI puts the actual blocker, and it is often
// something no amount of waiting will fix — e.g. "cannot reserve
// 8589934592B of memory on node host245: 5685293055B available memory left"
// when the Proxmox host is out of capacity. Without surfacing it here the
// operator sees only a counter that never advances and has to go run
// `kubectl describe machine` by hand to find out why.
func logMachinePhases(c *jobs.Ctx, kubectlBin, kcPath, clusterName string) {
	out, err := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath,
		"get", "machines", "-l", "cluster.x-k8s.io/cluster-name="+clusterName,
		"-o", `custom-columns=NAME:.metadata.name,PHASE:.status.phase,WHY:.status.conditions[?(@.type=="Ready")].message`,
		"--no-headers", "--request-timeout=10s").Output()
	if err != nil {
		return
	}
	for _, line := range splitLines(string(out)) {
		if strings.TrimSpace(line) != "" {
			c.Logf("  → %s", strings.TrimRight(line, " "))
		}
	}
}

// DeleteClusterSpec deletes the Cluster object, which CAPI cascades: every
// Machine (and, via CAPMOX, its underlying Proxmox VM) is torn down before
// the Cluster itself is removed. The delete request itself uses --wait=false
// (so it returns immediately rather than blocking on kubectl's own delete
// wait, which doesn't stream progress), but the job as a whole DOES wait —
// via a second step that polls until the Cluster object is actually gone —
// so job "succeeded" means teardown is genuinely complete, not just
// requested. This also means callers can safely add a step after this spec's
// steps (e.g. removing a local DB record) knowing the infrastructure is
// really gone by the time it runs.
func DeleteClusterSpec(dataDir, binDir, clusterName string) *jobs.Spec {
	return jobs.NewSpec("cluster.delete", "Delete cluster "+clusterName).
		Step("kubectl delete cluster", func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			c.Logf("Sending delete request for cluster %q (CAPI will cascade to all machines/VMs)", clusterName)
			return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"delete", "cluster", clusterName, "--wait=false")
		}).
		Step("Wait for teardown to finish", func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			deadline := time.Now().Add(15 * time.Minute)

			// Log the initial machine count so the user knows what will be torn down.
			if machineOut, err := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath,
				"get", "machines",
				"-l", "cluster.x-k8s.io/cluster-name="+clusterName,
				"-o", "custom-columns=NAME:.metadata.name,PHASE:.status.phase",
				"--no-headers").Output(); err == nil {
				lines := splitLines(string(machineOut))
				c.Logf("Waiting for %d machine(s) to be deleted and their Proxmox VMs to be removed...", countNonEmpty(lines))
				for _, line := range lines {
					if line != "" {
						c.Logf("  → %s", line)
					}
				}
			}

			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-c.Done():
					return c.Err()
				case <-ticker.C:
					err := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath,
						"get", "cluster", clusterName).Run()
					if err != nil {
						// kubectl get returning an error (NotFound, in practice) is
						// exactly the "gone" signal we're waiting for.
						c.Logf("✓ Cluster %s and all its VMs are fully torn down", clusterName)
						return nil
					}
					if time.Now().After(deadline) {
						return fmt.Errorf("cluster %s still exists after 15 minutes — teardown may be stuck; check `kubectl describe cluster %s` on the management cluster", clusterName, clusterName)
					}
					// Log remaining machines so the user can see teardown progress.
					machineOut, merr := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath,
						"get", "machines",
						"-l", "cluster.x-k8s.io/cluster-name="+clusterName,
						"-o", "custom-columns=NAME:.metadata.name,PHASE:.status.phase",
						"--no-headers").Output()
					if merr == nil && len(machineOut) > 0 {
						remaining := countNonEmpty(splitLines(string(machineOut)))
						c.Logf("Still tearing down — %d machine(s) remaining:", remaining)
						for _, line := range splitLines(string(machineOut)) {
							if line != "" {
								c.Logf("  → %s", line)
							}
						}
					} else {
						c.Logf("Still tearing down %s... (waiting for cluster object to be removed)", clusterName)
					}
				}
			}
		})
}

// splitLines splits a string into individual lines, trimming trailing whitespace.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			// trim trailing \r
			for len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			out = append(out, line)
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func countNonEmpty(lines []string) int {
	n := 0
	for _, l := range lines {
		if l != "" {
			n++
		}
	}
	return n
}

// UpgradeInput describes a version upgrade: PVEKube's templates are
// immutable Proxmox VM clones pinned to one Kubernetes version each (built
// once by the template builder), so "upgrading" a cluster means pointing it
// at a DIFFERENT, already-built template — not an in-place package update.
// This matches how CAPMOX itself models it: ProxmoxMachineTemplate is
// immutable, so a version change means creating a new one and repointing
// the control plane / MachineDeployment at it, which is exactly what
// triggers Cluster API's built-in rolling replacement.
type UpgradeInput struct {
	ClusterName          string
	NewKubernetesVersion string
	NewSourceNode        string
	NewTemplateVMID      int
	Bridge               string
	NumSockets           int
	NumCores             int
	MemoryMiB            int
	BootVolumeDevice     string
	BootVolumeSizeGB     int
}

// newMachineTemplateYAML renders a ProxmoxMachineTemplate manifest. Shape
// verified against the real cluster-template.yaml (ionos-cloud/cluster-api-provider-proxmox
// v0.9.0) — same fields as the ones clusterctl generate produces initially,
// just under a fresh name so the immutable resource can be created anew.
func newMachineTemplateYAML(name, sourceNode string, vmid int, in UpgradeInput) string {
	return fmt.Sprintf(`apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
kind: ProxmoxMachineTemplate
metadata:
  name: %s
spec:
  template:
    spec:
      sourceNode: %s
      templateID: %d
      full: false
      numSockets: %d
      numCores: %d
      memoryMiB: %d
      disks:
        bootVolume:
          disk: %s
          sizeGb: %d
      network:
        networkDevices:
        - bridge: %s
          model: virtio
          name: net0
`, name, sourceNode, vmid, in.NumSockets, in.NumCores, in.MemoryMiB, in.BootVolumeDevice, in.BootVolumeSizeGB, in.Bridge)
}

// UpgradeSpec creates new ProxmoxMachineTemplates for control plane and
// workers, then patches KubeadmControlPlane and MachineDeployment to
// reference them and the new Kubernetes version — Cluster API's controllers
// take it from there, replacing machines one at a time.
//
// NOTE: verified against CAPMOX's real resource schema (fetched and read,
// not guessed) but — unlike every other feature in PVEKube — not exercised
// against a live rolling upgrade end-to-end; that takes a running cluster
// with two real templates already built, which needs a Linux host this
// session didn't have. Treat this path with a bit more scrutiny than the
// rest before relying on it against a production cluster.
func UpgradeSpec(dataDir, binDir string, in UpgradeInput) *jobs.Spec {
	suffix := fmt.Sprintf("-v%s", sanitizeVersion(in.NewKubernetesVersion))
	cpTemplateName := in.ClusterName + "-control-plane" + suffix
	workerTemplateName := in.ClusterName + "-worker" + suffix

	return jobs.NewSpec("cluster.upgrade", "Upgrade "+in.ClusterName+" to "+in.NewKubernetesVersion).
		Step("Create control-plane machine template", func(c *jobs.Ctx) error {
			return applyYAML(c, dataDir, binDir, newMachineTemplateYAML(cpTemplateName, in.NewSourceNode, in.NewTemplateVMID, in))
		}).
		Step("Create worker machine template", func(c *jobs.Ctx) error {
			return applyYAML(c, dataDir, binDir, newMachineTemplateYAML(workerTemplateName, in.NewSourceNode, in.NewTemplateVMID, in))
		}).
		Step("Patch KubeadmControlPlane", func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			patch := fmt.Sprintf(`{"spec":{"version":%q,"machineTemplate":{"spec":{"infrastructureRef":{"name":%q}}}}}`,
				in.NewKubernetesVersion, cpTemplateName)
			return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"patch", "kubeadmcontrolplane", in.ClusterName+"-control-plane", "--type=merge", "-p", patch)
		}).
		Step("Patch MachineDeployment", func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"version":%q,"infrastructureRef":{"name":%q}}}}}`,
				in.NewKubernetesVersion, workerTemplateName)
			return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"patch", "machinedeployment", in.ClusterName+"-workers", "--type=merge", "-p", patch)
		})
}

func applyYAML(c *jobs.Ctx, dataDir, binDir, yamlContent string) error {
	kubectlBin := filepath.Join(binDir, "kubectl")
	kcPath := bootstrap.KubeconfigPath(dataDir)
	f, err := writeTempYAML(dataDir, yamlContent)
	if err != nil {
		return err
	}
	defer removeFile(f)
	return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath, "apply", "-f", f)
}

func writeTempYAML(dataDir, content string) (string, error) {
	f, err := os.CreateTemp(dataDir, "lifecycle-*.yaml")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func removeFile(path string) { os.Remove(path) }

func sanitizeVersion(v string) string {
	out := make([]byte, 0, len(v))
	for i := 0; i < len(v); i++ {
		ch := v[i]
		if ch == '.' {
			out = append(out, '-')
		} else if ch != 'v' {
			out = append(out, ch)
		}
	}
	return string(out)
}
