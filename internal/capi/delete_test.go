package capi

import (
	"strings"
	"testing"
)

// A cluster whose CAPI object is already gone is the case that stranded
// PVEKube's row twice for real: without --ignore-not-found kubectl exits
// non-zero on NotFound, the step fails, and the job engine skips every later
// step — including the one that removes the local record, leaving it
// unremovable through the UI and needing the database edited by hand.
func TestDeleteClusterArgsIgnoreNotFound(t *testing.T) {
	args := deleteClusterArgs("/tmp/kubeconfig", "demo")
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--ignore-not-found=true") {
		t.Fatalf("delete must tolerate an already-gone cluster; got: %s", joined)
	}
	// The rest of the invocation must stay intact.
	for _, want := range []string{"--kubeconfig /tmp/kubeconfig", "delete cluster demo", "--wait=false"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in: %s", want, joined)
		}
	}
}

// The cluster name must be passed as its own argument rather than
// interpolated, so a name never merges into an adjacent flag.
func TestDeleteClusterArgsKeepsNameSeparate(t *testing.T) {
	args := deleteClusterArgs("/kc", "my-cluster")
	found := false
	for i, a := range args {
		if a == "cluster" && i+1 < len(args) && args[i+1] == "my-cluster" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cluster name is not a discrete argument: %v", args)
	}
}
