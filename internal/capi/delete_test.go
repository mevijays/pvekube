package capi

import (
	"errors"
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

func TestClusterLookupDistinguishesNotFoundFromCommandFailure(t *testing.T) {
	args := strings.Join(clusterLookupArgs("/kc", "demo"), " ")
	for _, want := range []string{"get cluster demo", "-o name", "--ignore-not-found=true", "--request-timeout=10s"} {
		if !strings.Contains(args, want) {
			t.Fatalf("lookup args missing %q: %s", want, args)
		}
	}

	if !clusterConfirmedAbsent(nil, nil) {
		t.Fatal("successful empty lookup should confirm absence")
	}
	if clusterConfirmedAbsent(nil, errors.New("connection refused")) {
		t.Fatal("a kubectl failure must not confirm absence")
	}
	if clusterConfirmedAbsent([]byte("cluster.cluster.x-k8s.io/demo\n"), nil) {
		t.Fatal("a returned cluster name must not confirm absence")
	}
}
