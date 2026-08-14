package capi

import (
	"strings"
	"testing"
)

func TestInjectIgnitionFormatNoOpForCloudInitFlavors(t *testing.T) {
	for _, flavor := range []string{"ubuntu-2404", "ubuntu-2604-efi", "rockylinux-9", "", "unknown-flavor"} {
		out, err := InjectIgnitionFormat(sampleManifest, flavor)
		if err != nil {
			t.Fatalf("flavor %q: unexpected error: %v", flavor, err)
		}
		if out != sampleManifest {
			t.Fatalf("flavor %q: expected byte-identical output, manifest was modified", flavor)
		}
	}
}

func TestInjectIgnitionFormatSetsFormatOnBothBootstrapDocuments(t *testing.T) {
	out, err := InjectIgnitionFormat(sampleManifest, "flatcar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	docs := decodeDocs(t, out)

	if got := nested(t, docs[1], "spec", "kubeadmConfigSpec", "format"); got != "ignition" {
		t.Fatalf("KubeadmControlPlane format = %v, want ignition", got)
	}
	if got := nested(t, docs[2], "spec", "template", "spec", "format"); got != "ignition" {
		t.Fatalf("KubeadmConfigTemplate format = %v, want ignition", got)
	}
	// Sibling fields must survive untouched.
	if v := nested(t, docs[1], "spec", "version"); v != "v1.32.1" {
		t.Fatalf("KubeadmControlPlane document was otherwise touched: %v", docs[1])
	}
	// Non-bootstrap documents are unaffected.
	if _, ok := docs[0]["spec"].(map[string]any)["format"]; ok {
		t.Fatal("format leaked into the Cluster document, which has no such field")
	}
	if _, ok := docs[3]["spec"].(map[string]any)["format"]; ok {
		t.Fatal("format leaked into the ProxmoxCluster document, which has no such field")
	}
}

// Case-insensitivity matters because os_flavor is stored/selected as a plain
// string from the templates table — a stray "Flatcar" or "FLATCAR" must not
// silently fall through to the no-op path and leave a Flatcar cluster on
// cloud-config again.
func TestInjectIgnitionFormatIsCaseInsensitive(t *testing.T) {
	for _, flavor := range []string{"Flatcar", "FLATCAR", " flatcar "} {
		out, err := InjectIgnitionFormat(sampleManifest, flavor)
		if err != nil {
			t.Fatalf("flavor %q: unexpected error: %v", flavor, err)
		}
		docs := decodeDocs(t, out)
		if got := nested(t, docs[1], "spec", "kubeadmConfigSpec", "format"); got != "ignition" {
			t.Fatalf("flavor %q: format = %v, want ignition", flavor, got)
		}
	}
}

// A files: entry with no permissions key (CAPMOX's own base template ships
// the kube-vip static pod exactly like this) must get one defaulted —
// otherwise CABPK's Ignition transpiler warns "mode unspecified for file,
// defaulting to 0644" and the pinned CABPK version escalates that warning
// into a fatal reconcile error, confirmed against a real stuck cluster.
func TestInjectIgnitionFormatDefaultsMissingFilePermissions(t *testing.T) {
	manifest := `apiVersion: controlplane.cluster.x-k8s.io/v1beta1
kind: KubeadmControlPlane
metadata:
  name: demo-control-plane
spec:
  kubeadmConfigSpec:
    files:
    - content: kube-vip-pod-yaml-here
      owner: root:root
      path: /etc/kubernetes/manifests/kube-vip.yaml
    - content: some-script
      owner: root:root
      path: /etc/kube-vip-prepare.sh
      permissions: "0700"
    - content: null-perm-case
      owner: root:root
      path: /etc/foo
      permissions:
`
	out, err := InjectIgnitionFormat(manifest, "flatcar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	docs := decodeDocs(t, out)
	files := nested(t, docs[0], "spec", "kubeadmConfigSpec", "files").([]any)

	f0 := files[0].(map[string]any)
	if f0["permissions"] != "0644" {
		t.Fatalf("kube-vip file (no permissions key at all) = %v, want defaulted to 0644", f0["permissions"])
	}
	f1 := files[1].(map[string]any)
	if f1["permissions"] != "0700" {
		t.Fatalf("prepare script's explicit permissions was clobbered: %v, want 0700", f1["permissions"])
	}
	f2 := files[2].(map[string]any)
	if f2["permissions"] != "0644" {
		t.Fatalf("null permissions value = %v, want defaulted to 0644, not left null", f2["permissions"])
	}
}

// /usr is read-only on Flatcar — Ignition treats a failed write there as
// CRITICAL and halts boot entirely, confirmed live against a real VM stuck
// in an emergency-shell/reboot loop. Any files: entry under /usr must be
// dropped when applying ignition format, not just left to fail at runtime.
func TestInjectIgnitionFormatDropsUsrFiles(t *testing.T) {
	manifest := `apiVersion: controlplane.cluster.x-k8s.io/v1beta1
kind: KubeadmControlPlane
metadata:
  name: demo-control-plane
spec:
  kubeadmConfigSpec:
    files:
    - content: trust-bundle
      owner: root:root
      path: /usr/local/share/ca-certificates/pvekube-registry.crt
      permissions: "0644"
    - content: containerd-ca
      owner: root:root
      path: /etc/containerd/certs.d/registry/ca.crt
      permissions: "0644"
`
	out, err := InjectIgnitionFormat(manifest, "flatcar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	docs := decodeDocs(t, out)
	files := nested(t, docs[0], "spec", "kubeadmConfigSpec", "files").([]any)
	for _, f := range files {
		path := f.(map[string]any)["path"]
		if s, ok := path.(string); ok && strings.HasPrefix(s, "/usr") {
			t.Fatalf("a /usr file survived: %v", path)
		}
	}
	if !filePathPresent(files, "/etc/containerd/certs.d/registry/ca.crt") {
		t.Fatalf("the surviving /etc file is missing: %v", files)
	}
}

// filePathPresent reports whether any file entry in a decoded files: list
// has the given path.
func filePathPresent(files []any, path string) bool {
	for _, f := range files {
		if f.(map[string]any)["path"] == path {
			return true
		}
	}
	return false
}

// CAPMOX's VM-readiness poll runs `cloud-init status` over the QEMU guest
// agent, which never succeeds on Flatcar (no cloud-init binary at all) —
// confirmed live: a fully healthy, fully joined 4-node cluster stuck with
// every ProxmoxMachine permanently not-ready, which left the
// node.cluster.x-k8s.io/uninitialized taint on every worker forever,
// silently blocking anything that doesn't tolerate it. CAPMOX's own
// documented fix is spec.checks.skipCloudInitStatus: true on the
// ProxmoxMachineTemplate.
func TestInjectIgnitionFormatSkipsCloudInitStatusCheck(t *testing.T) {
	manifest := `apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
kind: ProxmoxMachineTemplate
metadata:
  name: demo-control-plane
spec:
  template:
    spec:
      sourceNode: pve1
      templateID: 110
`
	out, err := InjectIgnitionFormat(manifest, "flatcar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	docs := decodeDocs(t, out)
	skip := nested(t, docs[0], "spec", "template", "spec", "checks", "skipCloudInitStatus")
	if skip != true {
		t.Fatalf("checks.skipCloudInitStatus = %v (%T), want bool true", skip, skip)
	}
	// Sibling fields must survive untouched.
	if v := nested(t, docs[0], "spec", "template", "spec", "sourceNode"); v != "pve1" {
		t.Fatalf("ProxmoxMachineTemplate document was otherwise touched: %v", docs[0])
	}
}

func TestInjectIgnitionFormatNoOpOnProxmoxMachineTemplateForCloudInitFlavors(t *testing.T) {
	manifest := `apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
kind: ProxmoxMachineTemplate
metadata:
  name: demo-control-plane
spec:
  template:
    spec:
      sourceNode: pve1
`
	out, err := InjectIgnitionFormat(manifest, "ubuntu-2404")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != manifest {
		t.Fatal("expected byte-identical output for a cloud-init-capable flavor")
	}
}

// Generate() chains InjectRegistryTrust, InjectCredentialsRef, then
// InjectIgnitionFormat over the same manifest — prove the format key lands
// alongside registry files rather than one clobbering the other.
func TestInjectIgnitionFormatComposesWithRegistryTrust(t *testing.T) {
	out, err := InjectRegistryTrust(sampleManifest, RegistryConfig{Host: "registry.internal.lan:5000", CACertPEM: testCA})
	if err != nil {
		t.Fatalf("InjectRegistryTrust: %v", err)
	}
	out, err = InjectIgnitionFormat(out, "flatcar")
	if err != nil {
		t.Fatalf("InjectIgnitionFormat: %v", err)
	}
	docs := decodeDocs(t, out)

	if got := nested(t, docs[1], "spec", "kubeadmConfigSpec", "format"); got != "ignition" {
		t.Fatalf("format = %v, want ignition", got)
	}
	// 5: InjectRegistryTrust's usual 4 minus the /usr/local/share/
	// ca-certificates entry dropUsrFiles removes (see
	// TestInjectIgnitionFormatDropsUsrFiles), plus the 2 untaint-workaround
	// files injectUntaintWorkaround adds.
	files := nested(t, docs[1], "spec", "kubeadmConfigSpec", "files")
	list, ok := files.([]any)
	if !ok || len(list) != 5 {
		t.Fatalf("unexpected files list after ignition injection: %v", files)
	}
	if !filePathPresent(list, "/etc/pvekube/configure-registry.sh") {
		t.Fatal("registry setup script missing after ignition injection")
	}
	if !filePathPresent(list, untaintScriptPath) {
		t.Fatal("untaint script missing after ignition injection")
	}

	cmds := nested(t, docs[1], "spec", "kubeadmConfigSpec", "postKubeadmCommands")
	cmdList, ok := cmds.([]any)
	if !ok || len(cmdList) != 1 || !strings.Contains(cmdList[0].(string), "pvekube-untaint.service") {
		t.Fatalf("postKubeadmCommands missing the untaint service enable: %v", cmds)
	}
}
