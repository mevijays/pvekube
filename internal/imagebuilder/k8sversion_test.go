package imagebuilder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A real (trimmed) Debian flat-repo index. The versions are the ones the
// live v1.36 series actually publishes — note 1.36.2 carries -2.1 while its
// neighbours carry -1.1. That is the whole reason this resolution exists.
const debPackagesFixture = `Package: kubeadm
Version: 1.36.1-1.1
Architecture: amd64
Filename: amd64/kubeadm_1.36.1-1.1_amd64.deb

Package: kubeadm
Version: 1.36.1-1.1
Architecture: arm64
Filename: arm64/kubeadm_1.36.1-1.1_arm64.deb

Package: kubeadm
Version: 1.36.2-2.1
Architecture: amd64
Filename: amd64/kubeadm_1.36.2-2.1_amd64.deb

Package: kubectl
Version: 1.36.9-9.9
Architecture: amd64
Filename: amd64/kubectl_1.36.9-9.9_amd64.deb

Package: kubeadm
Version: 1.36.3-1.1
Architecture: amd64
Filename: amd64/kubeadm_1.36.3-1.1_amd64.deb
`

func fixtureServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Write([]byte(body))
	}))
}

// withPackagesURL points version resolution at a test server for the length
// of one test.
func withPackagesURL(t *testing.T, url string) {
	t.Helper()
	orig := debPackagesURL
	debPackagesURL = func(series string) string { return url }
	t.Cleanup(func() { debPackagesURL = orig })
}

// The case that motivated all of this: deriving "-1.1" by convention would
// pin apt to 1.36.2-1.1, which does not exist, and the build would die at
// package install roughly 25 minutes in.
func TestResolvePicksUpstreamPackagingRevision(t *testing.T) {
	srv := fixtureServer(t, debPackagesFixture, http.StatusOK)
	defer srv.Close()
	withPackagesURL(t, srv.URL)

	kv, err := ResolveKubernetesVersion(context.Background(), "1.36.2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kv.DebVersion != "1.36.2-2.1" {
		t.Fatalf("DebVersion = %q, want 1.36.2-2.1 (the revision upstream actually publishes)", kv.DebVersion)
	}
	if kv.Semver != "v1.36.2" || kv.Series != "v1.36" || kv.RPMVersion != "1.36.2" {
		t.Fatalf("unexpected resolution: %+v", kv)
	}
}

func TestResolveAcceptsBothVPrefixedAndBare(t *testing.T) {
	srv := fixtureServer(t, debPackagesFixture, http.StatusOK)
	defer srv.Close()
	withPackagesURL(t, srv.URL)

	for _, in := range []string{"1.36.2", "v1.36.2", "  v1.36.2  "} {
		kv, err := ResolveKubernetesVersion(context.Background(), in)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", in, err)
		}
		if kv.Semver != "v1.36.2" {
			t.Fatalf("%q resolved to %q", in, kv.Semver)
		}
	}
}

// Blank must stay blank: that path leaves image-builder's own pinned
// defaults alone, which is what every template built before this existed
// relied on.
func TestResolveEmptyIsNotAnError(t *testing.T) {
	kv, err := ResolveKubernetesVersion(context.Background(), "   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kv.Requested() {
		t.Fatalf("blank input should not request a version, got %+v", kv)
	}
	if kv.packerVars() != "" {
		t.Fatalf("blank input must add no packer vars, got %q", kv.packerVars())
	}
}

// A version that isn't published must fail at the form, naming what IS
// available — not 25 minutes later inside apt.
func TestResolveRejectsUnpublishedVersion(t *testing.T) {
	srv := fixtureServer(t, debPackagesFixture, http.StatusOK)
	defer srv.Close()
	withPackagesURL(t, srv.URL)

	_, err := ResolveKubernetesVersion(context.Background(), "1.36.99")
	if err == nil {
		t.Fatal("expected an error for a version that isn't published")
	}
	for _, want := range []string{"1.36.99", "1.36.1", "1.36.2", "1.36.3"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should mention %q so the operator can pick a real one; got: %v", want, err)
		}
	}
}

func TestResolveRejectsMalformedVersions(t *testing.T) {
	for _, bad := range []string{"1.36", "latest", "v1", "1.36.x", "1..2", "stable-1.36"} {
		if _, err := ResolveKubernetesVersion(context.Background(), bad); err == nil {
			t.Fatalf("ResolveKubernetesVersion(%q) should have failed", bad)
		}
	}
}

// A nonexistent minor series must say so rather than surfacing a bare 404.
func TestResolveRejectsUnknownSeries(t *testing.T) {
	srv := fixtureServer(t, "", http.StatusNotFound)
	defer srv.Close()
	withPackagesURL(t, srv.URL)

	_, err := ResolveKubernetesVersion(context.Background(), "1.99.0")
	if err == nil {
		t.Fatal("expected an error for a series with no package repository")
	}
	if !strings.Contains(err.Error(), "v1.99") {
		t.Fatalf("error should name the series, got: %v", err)
	}
}

// Only kubeadm's versions count — kubectl's 1.36.9-9.9 in the fixture must
// not be offered, since kubeadm is what pins the cluster's version.
func TestResolveIgnoresOtherPackages(t *testing.T) {
	srv := fixtureServer(t, debPackagesFixture, http.StatusOK)
	defer srv.Close()
	withPackagesURL(t, srv.URL)

	if _, err := ResolveKubernetesVersion(context.Background(), "1.36.9"); err == nil {
		t.Fatal("1.36.9 exists only as kubectl in the fixture and must not resolve")
	}
}

// The rendered flags are what actually reach Packer; a typo here silently
// builds the wrong version.
func TestPackerVarsRendersAllFourVariables(t *testing.T) {
	kv := KubernetesVersion{Semver: "v1.36.2", Series: "v1.36", DebVersion: "1.36.2-2.1", RPMVersion: "1.36.2"}
	got := kv.packerVars()
	for _, want := range []string{
		"--var kubernetes_semver=v1.36.2",
		"--var kubernetes_series=v1.36",
		"--var kubernetes_deb_version=1.36.2-2.1",
		"--var kubernetes_rpm_version=1.36.2",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("packerVars() missing %q; got %q", want, got)
		}
	}
}
