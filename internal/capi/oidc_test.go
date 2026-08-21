package capi

import "testing"

func TestInjectOIDCAuthDisabledIsByteIdentical(t *testing.T) {
	out, err := InjectOIDCAuth(sampleManifest, OIDCConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != sampleManifest {
		t.Fatal("manifest was modified even though no OIDC was configured")
	}
}

// A ClientID with no IssuerURL (or vice versa) isn't "configured" — half a
// setup would either be silently ignored by kube-apiserver or, worse,
// error at apiserver startup over a missing required flag. Enabled()
// requires both.
func TestInjectOIDCAuthRequiresBothIssuerAndClientID(t *testing.T) {
	for _, cfg := range []OIDCConfig{
		{IssuerURL: "https://dex.example.com"},
		{ClientID: "kubernetes"},
	} {
		out, err := InjectOIDCAuth(sampleManifest, cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out != sampleManifest {
			t.Fatalf("manifest was modified for a half-configured OIDCConfig: %+v", cfg)
		}
	}
}

func TestInjectOIDCAuthSetsExtraArgsAsListNotMap(t *testing.T) {
	out, err := InjectOIDCAuth(sampleManifest, OIDCConfig{
		IssuerURL: "https://dex.internal.lan", ClientID: "kubernetes",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	docs := decodeDocs(t, out)

	// docs[1] is the KubeadmControlPlane in sampleManifest (registry_test.go).
	raw := nested(t, docs[1], "spec", "kubeadmConfigSpec", "clusterConfiguration", "apiServer", "extraArgs")
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("extraArgs decoded as %T, want a list — kubeadm v1beta4 requires []Arg, not the old map[string]string shape", raw)
	}

	want := map[string]string{
		"oidc-issuer-url":     "https://dex.internal.lan",
		"oidc-client-id":      "kubernetes",
		"oidc-username-claim": "email",  // defaulted
		"oidc-groups-claim":   "groups", // defaulted
	}
	got := map[string]string{}
	for _, entry := range list {
		m := entry.(map[string]any)
		got[m["name"].(string)] = m["value"].(string)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("extraArgs[%q] = %q, want %q (full list: %v)", k, got[k], v, got)
		}
	}
	if _, ok := got["oidc-ca-file"]; ok {
		t.Fatalf("oidc-ca-file present without a CACertPEM being set: %v", got)
	}
}

func TestInjectOIDCAuthCACertAddsFileAndFlag(t *testing.T) {
	out, err := InjectOIDCAuth(sampleManifest, OIDCConfig{
		IssuerURL: "https://dex.internal.lan", ClientID: "kubernetes", CACertPEM: testCA,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	docs := decodeDocs(t, out)

	list := nested(t, docs[1], "spec", "kubeadmConfigSpec", "clusterConfiguration", "apiServer", "extraArgs").([]any)
	found := false
	for _, entry := range list {
		m := entry.(map[string]any)
		if m["name"] == "oidc-ca-file" {
			found = true
			if m["value"] != oidcCACertPath {
				t.Fatalf("oidc-ca-file value = %v, want %q", m["value"], oidcCACertPath)
			}
		}
	}
	if !found {
		t.Fatal("oidc-ca-file missing from extraArgs despite CACertPEM being set")
	}

	files := nested(t, docs[1], "spec", "kubeadmConfigSpec", "files").([]any)
	if !filePathPresent(files, oidcCACertPath) {
		t.Fatalf("CA cert file entry missing from files: %v", files)
	}
}

func TestInjectOIDCAuthDefaultsAreOverridable(t *testing.T) {
	out, err := InjectOIDCAuth(sampleManifest, OIDCConfig{
		IssuerURL: "https://login.microsoftonline.com/tenant-id/v2.0", ClientID: "app-id",
		UsernameClaim: "upn", GroupsClaim: "roles",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	docs := decodeDocs(t, out)
	list := nested(t, docs[1], "spec", "kubeadmConfigSpec", "clusterConfiguration", "apiServer", "extraArgs").([]any)
	got := map[string]string{}
	for _, entry := range list {
		m := entry.(map[string]any)
		got[m["name"].(string)] = m["value"].(string)
	}
	if got["oidc-username-claim"] != "upn" {
		t.Fatalf("oidc-username-claim = %q, want overridden value \"upn\"", got["oidc-username-claim"])
	}
	if got["oidc-groups-claim"] != "roles" {
		t.Fatalf("oidc-groups-claim = %q, want overridden value \"roles\"", got["oidc-groups-claim"])
	}
}

// Composes with InjectRegistryTrust the same way InjectIgnitionFormat does
// (ignition_test.go) — both touch KubeadmControlPlane's kubeadmConfigSpec
// but different sub-keys (files/preKubeadmCommands vs
// clusterConfiguration), so neither should clobber the other.
func TestInjectOIDCAuthComposesWithRegistryTrust(t *testing.T) {
	out, err := InjectRegistryTrust(sampleManifest, RegistryConfig{Host: "registry.internal.lan:5000", CACertPEM: testCA})
	if err != nil {
		t.Fatalf("InjectRegistryTrust: %v", err)
	}
	out, err = InjectOIDCAuth(out, OIDCConfig{IssuerURL: "https://dex.internal.lan", ClientID: "kubernetes"})
	if err != nil {
		t.Fatalf("InjectOIDCAuth: %v", err)
	}
	docs := decodeDocs(t, out)

	files := nested(t, docs[1], "spec", "kubeadmConfigSpec", "files")
	if list, ok := files.([]any); !ok || len(list) != 4 {
		t.Fatalf("registry files missing after OIDC injection: %v", files)
	}
	extraArgs := nested(t, docs[1], "spec", "kubeadmConfigSpec", "clusterConfiguration", "apiServer", "extraArgs")
	if list, ok := extraArgs.([]any); !ok || len(list) != 4 {
		t.Fatalf("OIDC extraArgs missing after composing with registry trust: %v", extraArgs)
	}
}
