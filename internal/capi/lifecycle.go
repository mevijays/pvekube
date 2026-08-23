package capi

import (
	"fmt"
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
// deleteClusterArgs is the kubectl invocation for removing a workload
// cluster. Split out so the --ignore-not-found guarantee is assertable
// without a live management cluster; see its use for why it matters.
func deleteClusterArgs(kcPath, clusterName string) []string {
	return []string{"--kubeconfig", kcPath, "delete", "cluster", clusterName,
		"--wait=false", "--ignore-not-found=true"}
}

// clusterLookupArgs renders an existence check whose exit status distinguishes
// a successful NotFound from transport/authentication failures. With
// --ignore-not-found kubectl exits successfully and prints nothing only when
// the object is genuinely absent.
func clusterLookupArgs(kcPath, clusterName string) []string {
	return []string{"--kubeconfig", kcPath, "get", "cluster", clusterName,
		"-o", "name", "--ignore-not-found=true", "--request-timeout=10s"}
}

func clusterConfirmedAbsent(output []byte, err error) bool {
	return err == nil && strings.TrimSpace(string(output)) == ""
}

func DeleteClusterSpec(dataDir, binDir, clusterName string) *jobs.Spec {
	return jobs.NewSpec("cluster.delete", "Delete cluster "+clusterName).
		Step("kubectl delete cluster", func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			c.Logf("Sending delete request for cluster %q (CAPI will cascade to all machines/VMs)", clusterName)
			// --ignore-not-found so a cluster already gone from the management
			// cluster is not an error. Without it kubectl exits non-zero on
			// NotFound, this step fails, and the job engine SKIPS every later
			// step — including "Remove local record". PVEKube's row for a
			// cluster deleted out-of-band (or half-deleted by an earlier run)
			// therefore became permanently stuck, with no route to clear it
			// through the UI at all. Hit twice for real; both times the row
			// had to be removed by editing the database by hand.
			return runner.Run(c, c, "", nil, kubectlBin, deleteClusterArgs(kcPath, clusterName)...)
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
			var lastLookupErr error
			for {
				select {
				case <-c.Done():
					return c.Err()
				case <-ticker.C:
					lookupOut, lookupErr := exec.CommandContext(c, kubectlBin,
						clusterLookupArgs(kcPath, clusterName)...).CombinedOutput()
					if clusterConfirmedAbsent(lookupOut, lookupErr) {
						c.Logf("✓ Cluster %s and all its VMs are fully torn down", clusterName)
						return nil
					}
					if lookupErr != nil {
						lastLookupErr = fmt.Errorf("%w: %s", lookupErr, strings.TrimSpace(string(lookupOut)))
						c.Logf("Could not confirm whether cluster %s still exists; will retry: %v", clusterName, lastLookupErr)
					} else {
						lastLookupErr = nil
					}
					if time.Now().After(deadline) {
						if lastLookupErr != nil {
							return fmt.Errorf("could not confirm deletion of cluster %s after 15 minutes; the management cluster remained unreachable: %w", clusterName, lastLookupErr)
						}
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
