// Rollout state for an existing workload cluster.
//
// Upgrade and Resize both work by repointing the control plane and the
// worker MachineDeployment at a new ProxmoxMachineTemplate, after which Cluster
// API replaces every machine over many minutes. Two things go wrong if that
// state is not read carefully:
//
//   - Starting a second rollout while the first is running stacks a third
//     MachineSet on top of two others and leaves the cluster with machines on
//     three different templates (this destroyed a worker pool once).
//   - Declaring the rollout finished too early. Right after a patch the
//     controller has not yet observed the new spec, so status still describes
//     the OLD, fully healthy state: replicas == upToDate == ready. A wait loop
//     that only compares those counters returns "complete" a second after the
//     patch, before a single machine has been replaced.
//
// EvaluateRollout answers both questions from one snapshot of the API objects.
package capi

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"pvekube/internal/bootstrap"
)

// RolloutState is the verdict for one cluster at one instant.
type RolloutState struct {
	// Settled is true only when every controller has observed the latest spec
	// and every machine is up to date, ready and the only one of its kind.
	Settled bool
	// Reasons lists, in plain words, why the cluster is not settled.
	Reasons []string
	// Summary is a one-line progress string for logs.
	Summary string
}

// Reason returns the reasons as one sentence fragment.
func (r RolloutState) Reason() string { return strings.Join(r.Reasons, "; ") }

func intField(m map[string]any, def int, keys ...string) int {
	cur := m
	for i, k := range keys {
		v, ok := cur[k]
		if !ok || v == nil {
			return def
		}
		if i == len(keys)-1 {
			if f, ok := v.(float64); ok {
				return int(f)
			}
			return def
		}
		next, ok := v.(map[string]any)
		if !ok {
			return def
		}
		cur = next
	}
	return def
}

// EvaluateRollout inspects the KubeadmControlPlane, the MachineDeployment and
// the cluster's Machines (all as decoded JSON). Missing or malformed input is
// never "settled": the safe answer to "I cannot tell" is "wait".
func EvaluateRollout(kcp, md map[string]any, machines []map[string]any) RolloutState {
	var reasons []string
	if kcp == nil || md == nil {
		return RolloutState{Reasons: []string{"control plane or worker pool not readable yet"}, Summary: "waiting for cluster objects"}
	}

	check := func(label string, obj map[string]any) (want, upToDate, ready int) {
		gen := intField(obj, 0, "metadata", "generation")
		obs := intField(obj, -1, "status", "observedGeneration")
		want = intField(obj, 1, "spec", "replicas")
		total := intField(obj, 0, "status", "replicas")
		upToDate = intField(obj, 0, "status", "upToDateReplicas")
		ready = intField(obj, 0, "status", "readyReplicas")

		// The stale-status guard: until the controller has seen the newest
		// spec, none of the counters below describe the requested state.
		if obs < gen {
			reasons = append(reasons, fmt.Sprintf("%s controller has not yet observed the latest change (generation %d, observed %d)", label, gen, obs))
		}
		if total != want {
			reasons = append(reasons, fmt.Sprintf("%s has %d machines, wants %d", label, total, want))
		}
		if upToDate != want {
			reasons = append(reasons, fmt.Sprintf("%s has %d/%d machines up to date", label, upToDate, want))
		}
		if ready != want {
			reasons = append(reasons, fmt.Sprintf("%s has %d/%d machines ready", label, ready, want))
		}
		// Controllers publish their own opinion as conditions. A rollout or
		// scale in flight shows up here even where the counters look equal.
		if conds, ok := obj["status"].(map[string]any); ok {
			if list, ok := conds["conditions"].([]any); ok {
				for _, ci := range list {
					c, _ := ci.(map[string]any)
					t, _ := c["type"].(string)
					st, _ := c["status"].(string)
					if st == "True" && (t == "RollingOut" || t == "ScalingUp" || t == "ScalingDown" || t == "Remediating") {
						reasons = append(reasons, fmt.Sprintf("%s is %s", label, t))
					}
				}
			}
		}
		return want, upToDate, ready
	}
	cpWant, cpUp, cpReady := check("control plane", kcp)
	mdWant, mdUp, mdReady := check("workers", md)

	deleting, total := 0, len(machines)
	for _, m := range machines {
		if meta, ok := m["metadata"].(map[string]any); ok {
			if ts, _ := meta["deletionTimestamp"].(string); ts != "" {
				deleting++
			}
		}
	}
	if deleting > 0 {
		reasons = append(reasons, fmt.Sprintf("%d machine(s) are still being deleted", deleting))
	}
	if total != cpWant+mdWant {
		reasons = append(reasons, fmt.Sprintf("cluster has %d machines, expected %d", total, cpWant+mdWant))
	}

	return RolloutState{
		Settled: len(reasons) == 0,
		Reasons: reasons,
		Summary: fmt.Sprintf("control plane %d/%d up-to-date (%d ready) · workers %d/%d up-to-date (%d ready) · %d machines (%d deleting)",
			cpUp, cpWant, cpReady, mdUp, mdWant, mdReady, total, deleting),
	}
}

func getJSON(ctx context.Context, kubectlBin, kcPath string, args ...string) (map[string]any, error) {
	full := append([]string{kubectlBin, "--kubeconfig", kcPath, "get"}, args...)
	full = append(full, "-o", "json", "--request-timeout=15s")
	out, err := kubectlOut(ctx, full...)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return nil, fmt.Errorf("decoding kubectl output: %w", err)
	}
	return m, nil
}

// ClusterRollout reads the live objects and evaluates them.
func ClusterRollout(ctx context.Context, dataDir, binDir, clusterName string) (RolloutState, error) {
	kubectlBin := filepath.Join(binDir, "kubectl")
	kcPath := bootstrap.KubeconfigPath(dataDir)
	kcp, err := getJSON(ctx, kubectlBin, kcPath, "kubeadmcontrolplane", clusterName+"-control-plane")
	if err != nil {
		return RolloutState{}, fmt.Errorf("reading control plane of %s: %w", clusterName, err)
	}
	md, err := getJSON(ctx, kubectlBin, kcPath, "machinedeployment", clusterName+"-workers")
	if err != nil {
		return RolloutState{}, fmt.Errorf("reading workers of %s: %w", clusterName, err)
	}
	ml, err := getJSON(ctx, kubectlBin, kcPath, "machines", "-l", "cluster.x-k8s.io/cluster-name="+clusterName)
	if err != nil {
		return RolloutState{}, fmt.Errorf("reading machines of %s: %w", clusterName, err)
	}
	var machines []map[string]any
	if items, ok := ml["items"].([]any); ok {
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				machines = append(machines, m)
			}
		}
	}
	return EvaluateRollout(kcp, md, machines), nil
}

// RolloutBlocker is the guard used before starting an upgrade or a resize. It
// returns a non-empty explanation when the cluster must be left alone, and
// fails closed: if the state cannot be read, the operation is refused rather
// than risk stacking on an unknown rollout.
func RolloutBlocker(ctx context.Context, dataDir, binDir, clusterName, what string) string {
	st, err := ClusterRollout(ctx, dataDir, binDir, clusterName)
	if err != nil {
		return fmt.Sprintf("Cannot %s %s right now: its rollout state could not be read (%v).", what, clusterName, err)
	}
	if st.Settled {
		return ""
	}
	return fmt.Sprintf("Cannot %s %s while it is still rolling or recovering — %s. Wait until every machine is up to date and ready, then try again.", what, clusterName, st.Reason())
}
