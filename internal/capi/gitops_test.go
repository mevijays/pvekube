package capi

import (
	"testing"
)

func TestGitOpsDefaults(t *testing.T) {
	a := AddonSelection{GitOps: true, GitOpsRepoURL: "https://git.example/r.git"}
	if got := a.gitOpsBranchOrDefault(); got != "main" {
		t.Fatalf("branch default = %q, want main", got)
	}
	if got := a.gitOpsPathOrDefault(); got != "./" {
		t.Fatalf("path default = %q, want ./", got)
	}
	a.GitOpsBranch, a.GitOpsPath = "develop", "./clusters/dev"
	if got := a.gitOpsBranchOrDefault(); got != "develop" {
		t.Fatalf("explicit branch was overridden: %q", got)
	}
	if got := a.gitOpsPathOrDefault(); got != "./clusters/dev" {
		t.Fatalf("explicit path was overridden: %q", got)
	}
}

// A Secret is only needed when there's actually something to put in it —
// creating an empty one for a public repo would attach a secretRef that
// Flux then fails to authenticate with.
func TestGitOpsNeedsAuth(t *testing.T) {
	cases := []struct {
		name string
		a    AddonSelection
		want bool
	}{
		{"public repo", AddonSelection{GitOpsRepoURL: "https://git.example/r.git"}, false},
		{"token", AddonSelection{GitOpsToken: "ghp_x"}, true},
		{"ca only (public repo, internal CA)", AddonSelection{GitOpsCACert: "-----BEGIN CERTIFICATE-----"}, true},
		{"whitespace token is not a token", AddonSelection{GitOpsToken: "   "}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.GitOpsNeedsAuth(); got != c.want {
				t.Fatalf("GitOpsNeedsAuth() = %v, want %v", got, c.want)
			}
		})
	}
}
