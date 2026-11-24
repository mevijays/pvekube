package capi

import (
	"strings"
	"testing"
)

type obj = map[string]any

func ctrl(gen, obs, want, total, up, ready int, conds ...[2]string) obj {
	cs := []any{}
	for _, c := range conds {
		cs = append(cs, obj{"type": c[0], "status": c[1]})
	}
	return obj{
		"metadata": obj{"generation": float64(gen)},
		"spec":     obj{"replicas": float64(want)},
		"status": obj{
			"observedGeneration": float64(obs), "replicas": float64(total),
			"upToDateReplicas": float64(up), "readyReplicas": float64(ready),
			"conditions": cs,
		},
	}
}

func machs(n, deleting int) []obj {
	var out []obj
	for i := 0; i < n; i++ {
		m := obj{"metadata": obj{"name": "m"}}
		if i < deleting {
			m["metadata"] = obj{"name": "m", "deletionTimestamp": "2026-11-22T10:00:00Z"}
		}
		out = append(out, m)
	}
	return out
}

func TestEvaluateRolloutSettled(t *testing.T) {
	st := EvaluateRollout(ctrl(2, 2, 1, 1, 1, 1, [2]string{"RollingOut", "False"}), ctrl(5, 5, 3, 3, 3, 3), machs(4, 0))
	if !st.Settled {
		t.Fatalf("expected settled, got %v", st.Reasons)
	}
}

// The regression: right after a patch the status still shows the old,
// healthy counters. Only observedGeneration < generation gives it away.
func TestEvaluateRolloutStaleStatusIsNotSettled(t *testing.T) {
	st := EvaluateRollout(ctrl(2, 2, 1, 1, 1, 1), ctrl(6, 5, 3, 3, 3, 3), machs(4, 0))
	if st.Settled {
		t.Fatal("status from before the patch must not count as finished")
	}
	if !strings.Contains(st.Reason(), "not yet observed") {
		t.Fatalf("reason should name the stale status: %q", st.Reason())
	}
}

func TestEvaluateRolloutInFlight(t *testing.T) {
	cases := map[string]struct {
		kcp, md obj
		n, del  int
		want    string
	}{
		"missing replicas":   {ctrl(2, 2, 1, 1, 1, 1), ctrl(5, 5, 3, 3, 3, 2), 4, 0, "2/3 machines ready"},
		"old not up to date": {ctrl(2, 2, 1, 1, 1, 1), ctrl(5, 5, 3, 3, 1, 3), 4, 0, "1/3 machines up to date"},
		"surge machine":      {ctrl(2, 2, 1, 1, 1, 1), ctrl(5, 5, 3, 4, 3, 3), 5, 0, "has 4 machines, wants 3"},
		"rolling condition":  {ctrl(2, 2, 1, 1, 1, 1), ctrl(5, 5, 3, 3, 3, 3, [2]string{"RollingOut", "True"}), 4, 0, "workers is RollingOut"},
		"scaling up":         {ctrl(2, 2, 1, 1, 1, 1, [2]string{"ScalingUp", "True"}), ctrl(5, 5, 3, 3, 3, 3), 4, 0, "control plane is ScalingUp"},
		"deleting machine":   {ctrl(2, 2, 1, 1, 1, 1), ctrl(5, 5, 3, 3, 3, 3), 4, 1, "still being deleted"},
		"orphan machine":     {ctrl(2, 2, 1, 1, 1, 1), ctrl(5, 5, 3, 3, 3, 3), 5, 0, "has 5 machines, expected 4"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			st := EvaluateRollout(tc.kcp, tc.md, machs(tc.n, tc.del))
			if st.Settled {
				t.Fatal("should not be settled")
			}
			if !strings.Contains(st.Reason(), tc.want) {
				t.Fatalf("reason %q lacks %q", st.Reason(), tc.want)
			}
		})
	}
}

func TestEvaluateRolloutUnreadableIsNotSettled(t *testing.T) {
	if EvaluateRollout(nil, ctrl(5, 5, 3, 3, 3, 3), nil).Settled {
		t.Fatal("nil control plane must not be settled")
	}
	// A missing observedGeneration (unknown) must also fail safe.
	kcp := ctrl(2, 2, 1, 1, 1, 1)
	delete(kcp["status"].(obj), "observedGeneration")
	if EvaluateRollout(kcp, ctrl(5, 5, 3, 3, 3, 3), machs(4, 0)).Settled {
		t.Fatal("absent observedGeneration must not be settled")
	}
}
