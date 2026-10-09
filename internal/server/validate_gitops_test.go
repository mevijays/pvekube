package server

import (
	"strings"
	"testing"

	"pvekube/internal/capi"
)

const testCAPEM = `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----`

func TestValidateGitOpsFormAcceptsValid(t *testing.T) {
	cases := []capi.AddonSelection{
		{GitOps: true, GitOpsRepoURL: "https://github.com/org/repo.git"},
		{GitOps: true, GitOpsRepoURL: "http://gitea.internal/org/repo.git"},
		{GitOps: true, GitOpsRepoURL: "ssh://git@github.com/org/repo.git"},
		{GitOps: true, GitOpsRepoURL: "https://gitea.internal/org/repo.git", GitOpsUsername: "bot", GitOpsToken: "tok"},
		{GitOps: true, GitOpsRepoURL: "https://gitea.internal/org/repo.git", GitOpsCACert: testCAPEM},
		// Not enabled and nothing filled in — the untouched default.
		{},
	}
	for _, c := range cases {
		if err := validateGitOpsForm(c); err != nil {
			t.Fatalf("validateGitOpsForm(%+v) unexpectedly failed: %v", c, err)
		}
	}
}

// Flux's GitRepository API rejects the scp-style shorthand outright, so
// catching it in the form is the difference between an error the operator
// sees immediately and a cluster that comes up with a Flux that can never
// fetch anything.
func TestValidateGitOpsFormRejectsScpStyleURL(t *testing.T) {
	err := validateGitOpsForm(capi.AddonSelection{GitOps: true, GitOpsRepoURL: "git@github.com:org/repo.git"})
	if err == nil {
		t.Fatal("scp-style URL should be rejected")
	}
	if !strings.Contains(err.Error(), "ssh://") {
		t.Fatalf("error should point at the supported form; got: %v", err)
	}
}

func TestValidateGitOpsFormRejectsBadInput(t *testing.T) {
	cases := []struct {
		name       string
		a          capi.AddonSelection
		wantSubstr string
	}{
		{"enabled without url", capi.AddonSelection{GitOps: true}, "no repository URL"},
		{"settings without checkbox", capi.AddonSelection{GitOpsRepoURL: "https://x/y.git"}, "checkbox is not ticked"},
		{"unsupported scheme", capi.AddonSelection{GitOps: true, GitOpsRepoURL: "ftp://x/y.git"}, "must start with"},
		{"token on ssh url", capi.AddonSelection{GitOps: true, GitOpsRepoURL: "ssh://git@h/r.git", GitOpsToken: "t"}, "cannot authenticate an ssh://"},
		{"garbage ca", capi.AddonSelection{GitOps: true, GitOpsRepoURL: "https://x/y.git", GitOpsCACert: "not a cert"}, "not valid PEM"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateGitOpsForm(c.a)
			if err == nil {
				t.Fatalf("expected an error for %+v", c.a)
			}
			if !strings.Contains(err.Error(), c.wantSubstr) {
				t.Fatalf("error %q should mention %q", err.Error(), c.wantSubstr)
			}
		})
	}
}

// The form is pre-filled from the previous cluster's saved GitOps settings and
// those fields are still submitted when the box is unticked. The tick has to
// win: leftover values must neither be an error nor reach the cluster.
func TestUntickedGitOpsIgnoresPrefilledSettings(t *testing.T) {
	prefilled := capi.AddonSelection{
		MetricsServer: true,
		GitOpsRepoURL: "https://gitea.internal/org/repo.git", GitOpsBranch: "main", GitOpsPath: "./clusters/dev",
		GitOpsUsername: "bot", GitOpsToken: "tok", GitOpsCACert: testCAPEM,
	}
	got := withoutUntickedGitOps(prefilled)
	if err := validateGitOpsForm(got); err != nil {
		t.Fatalf("unticked GitOps with saved values should validate, got: %v", err)
	}
	if got.GitOpsRepoURL != "" || got.GitOpsToken != "" || got.GitOpsCACert != "" || got.GitOpsUsername != "" || got.GitOpsBranch != "" || got.GitOpsPath != "" {
		t.Errorf("GitOps settings survived an unticked box: %+v", got)
	}
	if !got.MetricsServer {
		t.Error("unrelated add-ons must be left alone")
	}

	// Ticked: everything is kept, including the saved values.
	prefilled.GitOps = true
	if kept := withoutUntickedGitOps(prefilled); kept != prefilled {
		t.Errorf("ticked GitOps was altered: %+v", kept)
	}
}
