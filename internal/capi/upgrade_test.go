package capi

import (
	"strings"
	"testing"
)

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in   string
		want semver
	}{
		{"v1.36.1", semver{1, 36, 1}},
		{"1.36.1", semver{1, 36, 1}},
		{"v1.37", semver{1, 37, 0}},
		{" v1.36.1 ", semver{1, 36, 1}},
		// Pre-release/build metadata is trimmed to the numeric core — the
		// skew rules below only ever compare major/minor/patch.
		{"v1.37.0-rc.1", semver{1, 37, 0}},
		{"v1.37.0+build5", semver{1, 37, 0}},
	}
	for _, c := range cases {
		got, err := parseSemver(c.in)
		if err != nil {
			t.Fatalf("parseSemver(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("parseSemver(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "latest", "v1", "image-builder default", "abc.def.ghi"} {
		if _, err := parseSemver(bad); err == nil {
			t.Fatalf("parseSemver(%q) should have failed", bad)
		}
	}
}

// The whole point of ValidateUpgrade is to reject an unsupported jump BEFORE
// anything is patched — CAPI applies a version change by rolling machines, so
// an invalid target fails halfway through with the cluster split across two
// versions rather than failing cleanly.
func TestValidateUpgradeRejectsUnsupportedJumps(t *testing.T) {
	cases := []struct {
		name, current, target, wantSubstr string
	}{
		{"same version", "v1.36.1", "v1.36.1", "already running"},
		{"patch downgrade", "v1.36.1", "v1.36.0", "downgrade"},
		{"minor downgrade", "v1.36.1", "v1.35.4", "downgrade"},
		{"major downgrade", "v2.0.0", "v1.36.1", "downgrade"},
		{"skips one minor", "v1.36.1", "v1.38.0", "skips 1 minor"},
		{"skips several minors", "v1.30.0", "v1.36.1", "skips 5 minor"},
		{"major jump", "v1.36.1", "v2.0.0", "major version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateUpgrade(c.current, c.target)
			if err == nil {
				t.Fatalf("ValidateUpgrade(%q, %q) should have failed", c.current, c.target)
			}
			if !strings.Contains(err.Error(), c.wantSubstr) {
				t.Fatalf("error %q should mention %q", err.Error(), c.wantSubstr)
			}
		})
	}
}

func TestValidateUpgradeAcceptsSupportedJumps(t *testing.T) {
	cases := []struct{ name, current, target string }{
		{"patch bump", "v1.36.1", "v1.36.4"},
		{"one minor up", "v1.36.1", "v1.37.0"},
		{"one minor up, lower patch", "v1.36.5", "v1.37.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateUpgrade(c.current, c.target); err != nil {
				t.Fatalf("ValidateUpgrade(%q, %q): unexpected error: %v", c.current, c.target, err)
			}
		})
	}
}

// A non-semver version (the string "image-builder default" used to get
// recorded for templates built without an explicit version) must produce a
// clear error rather than being silently treated as v0.0.0 — which would
// make every real version look like a valid upgrade from it.
func TestValidateUpgradeRejectsUnparseableVersions(t *testing.T) {
	if err := ValidateUpgrade("image-builder default", "v1.36.1"); err == nil {
		t.Fatal("expected an error for an unparseable current version")
	}
	if err := ValidateUpgrade("v1.36.1", "not-a-version"); err == nil {
		t.Fatal("expected an error for an unparseable target version")
	}
}

// The generated name must be derived from the CLUSTER, not by suffixing the
// template being replaced — otherwise successive upgrades compound into
// "<cluster>-control-plane-1-37-0-120-1-38-0-125".
func TestUpgradeTemplateNameDoesNotCompound(t *testing.T) {
	first := upgradeTemplateName("devk8s", "control-plane", "v1.37.0", 120)
	if first != "devk8s-control-plane-1-37-0-120" {
		t.Fatalf("got %q", first)
	}
	second := upgradeTemplateName("devk8s", "control-plane", "v1.38.0", 125)
	if second != "devk8s-control-plane-1-38-0-125" {
		t.Fatalf("second upgrade name compounded or drifted: %q", second)
	}
	if strings.Contains(second, "1-37-0") {
		t.Fatalf("second upgrade name carries the first upgrade's version: %q", second)
	}
}

// ProxmoxMachineTemplate is immutable, so re-applying one name with a
// different templateID is rejected by the API server. Two templates built at
// the SAME Kubernetes version (a rebuild) must therefore produce different
// names — which is why the VMID is part of it.
func TestUpgradeTemplateNameDistinguishesRebuiltTemplates(t *testing.T) {
	a := upgradeTemplateName("devk8s", "worker", "v1.37.0", 120)
	b := upgradeTemplateName("devk8s", "worker", "v1.37.0", 131)
	if a == b {
		t.Fatalf("a rebuilt template at the same version collides: both %q", a)
	}
}

// Names go straight into a Kubernetes object's metadata.name, which must be a
// DNS-1123 subdomain: lowercase alphanumerics, '-' and '.' only.
func TestUpgradeTemplateNameIsDNSSafe(t *testing.T) {
	name := upgradeTemplateName("devk8s", "control-plane", "v1.37.0", 120)
	for _, ch := range name {
		ok := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-'
		if !ok {
			t.Fatalf("name %q contains %q, which is not valid in a DNS-1123 subdomain", name, ch)
		}
	}
}
