package capi

import (
	"strings"
	"testing"
)

func TestInjectInternalCATrustIsNoOpWhenUnset(t *testing.T) {
	for _, ca := range []string{"", "   ", "\n\t "} {
		out, err := InjectInternalCATrust(sampleManifest, ca)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out != sampleManifest {
			t.Fatalf("expected byte-identical output for CA %q", ca)
		}
	}
}

// The CA has to reach BOTH bootstrap documents. Only patching the
// KubeadmControlPlane would give control-plane nodes the trust and leave
// every worker without it — the asymmetric failure that is hardest to spot,
// because the cluster comes up fine and only some pods' nodes misbehave.
func TestInjectInternalCATrustReachesControlPlaneAndWorkers(t *testing.T) {
	out, err := InjectInternalCATrust(sampleManifest, testCA)
	if err != nil {
		t.Fatalf("InjectInternalCATrust: %v", err)
	}
	docs := decodeDocs(t, out)

	var checked int
	for _, doc := range docs {
		kind, _ := nested(t, doc, "kind").(string)
		var files any
		switch kind {
		case "KubeadmControlPlane":
			files = nested(t, doc, "spec", "kubeadmConfigSpec", "files")
		case "KubeadmConfigTemplate":
			files = nested(t, doc, "spec", "template", "spec", "files")
		default:
			continue
		}
		list, ok := files.([]any)
		if !ok {
			t.Fatalf("%s: files is not a list: %v", kind, files)
		}
		if !filePathPresent(list, internalCASystemPath) {
			t.Errorf("%s: internal CA missing from the node trust store", kind)
		}
		if !filePathPresent(list, internalCAScriptPath) {
			t.Errorf("%s: update-ca-certificates script missing", kind)
		}
		checked++
	}
	if checked < 2 {
		t.Fatalf("expected to check a control plane and a worker document, checked %d", checked)
	}
	if !strings.Contains(out, internalCAScriptPath) {
		t.Error("the script was never added to preKubeadmCommands")
	}
}

// Registry trust and internal CA trust both append to the same files: list.
// Running both must yield both sets, not one overwriting the other — the
// exact failure the shared injectBootstrapFiles helper exists to prevent.
func TestInternalCAComposesWithRegistryTrust(t *testing.T) {
	out, err := InjectRegistryTrust(sampleManifest, RegistryConfig{Host: "registry.internal.lan:5000", CACertPEM: testCA})
	if err != nil {
		t.Fatalf("InjectRegistryTrust: %v", err)
	}
	out, err = InjectInternalCATrust(out, testCA)
	if err != nil {
		t.Fatalf("InjectInternalCATrust: %v", err)
	}
	docs := decodeDocs(t, out)
	files := nested(t, docs[1], "spec", "kubeadmConfigSpec", "files").([]any)

	if !filePathPresent(files, registrySetupScriptPath) {
		t.Error("registry setup script lost when the internal CA was added")
	}
	if !filePathPresent(files, internalCASystemPath) {
		t.Error("internal CA missing after composing with registry trust")
	}
	if !filePathPresent(files, "/etc/containerd/certs.d/registry.internal.lan:5000/ca.crt") {
		t.Error("registry containerd CA lost when the internal CA was added")
	}
}

// On Flatcar the internal CA must survive InjectIgnitionFormat's /usr sweep
// by being rehomed, not dropped. Dropping it is precisely the bug that left
// Flatcar nodes unable to verify any internal HTTPS endpoint.
func TestInternalCASurvivesFlatcarIgnitionPass(t *testing.T) {
	out, err := InjectInternalCATrust(sampleManifest, testCA)
	if err != nil {
		t.Fatalf("InjectInternalCATrust: %v", err)
	}
	out, err = InjectIgnitionFormat(out, "flatcar")
	if err != nil {
		t.Fatalf("InjectIgnitionFormat: %v", err)
	}
	docs := decodeDocs(t, out)
	files := nested(t, docs[1], "spec", "kubeadmConfigSpec", "files").([]any)

	if filePathPresent(files, internalCASystemPath) {
		t.Fatal("the /usr path survived — Ignition would halt the boot")
	}
	if !filePathPresent(files, "/etc/ssl/certs/pvekube-internal-ca.pem") {
		t.Fatalf("internal CA was dropped instead of rehomed for Flatcar: %v", files)
	}
	// The script that refreshes the bundle lives under /etc and must be kept.
	if !filePathPresent(files, internalCAScriptPath) {
		t.Error("update-ca-certificates script lost on Flatcar")
	}
}

func TestInternalCAConfigMapsCoverEveryNamespace(t *testing.T) {
	namespaces := []string{"default", "kube-system", "flux-system"}
	out, err := internalCAConfigMaps(namespaces, testCA)
	if err != nil {
		t.Fatalf("internalCAConfigMaps: %v", err)
	}
	if n := strings.Count(out, "ConfigMap"); n != len(namespaces) {
		t.Fatalf("expected %d ConfigMaps, found %d:\n%s", len(namespaces), n, out)
	}
	for _, ns := range namespaces {
		if !strings.Contains(out, `"namespace":"`+ns+`"`) {
			t.Errorf("no ConfigMap for namespace %q", ns)
		}
	}
	// Every document needs the separator or kubectl reads one object.
	if n := strings.Count(out, "---\n"); n != len(namespaces) {
		t.Errorf("expected %d document separators, got %d", len(namespaces), n)
	}
	if !strings.Contains(out, InternalCAKey) {
		t.Error("the CA is not stored under the documented key")
	}
}

// A PEM body is multi-line and full of characters that break naive string
// building; JSON encoding is what keeps the document valid.
func TestInternalCAConfigMapsSurviveAwkwardPEM(t *testing.T) {
	awkward := "-----BEGIN CERTIFICATE-----\nline\"with\\quotes\nand: a colon\n-----END CERTIFICATE-----\n"
	out, err := internalCAConfigMaps([]string{"default"}, awkward)
	if err != nil {
		t.Fatalf("internalCAConfigMaps: %v", err)
	}
	if strings.Contains(out, "\nline\"with") {
		t.Fatalf("raw newlines leaked into the document, which would not parse:\n%s", out)
	}
	if !strings.Contains(out, `line\"with\\quotes`) {
		t.Errorf("PEM body was not escaped as expected:\n%s", out)
	}
}

func TestFlatcarTrustAnchorPath(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"/usr/local/share/ca-certificates/pvekube-internal-ca.crt", "/etc/ssl/certs/pvekube-internal-ca.pem", true},
		// Already .pem: renamed once, not twice.
		{"/usr/local/share/ca-certificates/ca.pem", "/etc/ssl/certs/ca.pem", true},
		// Not a CA path — no defensible destination, so it stays dropped.
		{"/usr/local/bin/helper", "", false},
		{"/usr/share/something", "", false},
		// A nested path would change meaning if flattened.
		{"/usr/local/share/ca-certificates/sub/dir.crt", "", false},
	}
	for _, tc := range tests {
		got, ok := flatcarTrustAnchorPath(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("flatcarTrustAnchorPath(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}
