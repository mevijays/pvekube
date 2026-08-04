package capi

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Shaped like the real CAPMOX v0.9.0 output: several documents, only two of
// which are kubeadm bootstrap configs, and neither of which already has a
// files: or preKubeadmCommands: key.
const sampleManifest = `apiVersion: cluster.x-k8s.io/v1beta1
kind: Cluster
metadata:
  name: demo
spec:
  controlPlaneRef:
    name: demo-control-plane
---
apiVersion: controlplane.cluster.x-k8s.io/v1beta1
kind: KubeadmControlPlane
metadata:
  name: demo-control-plane
spec:
  replicas: 1
  kubeadmConfigSpec:
    initConfiguration:
      nodeRegistration:
        kubeletExtraArgs:
          provider-id: proxmox://'{{ ds.meta_data.instance_id }}'
  version: v1.32.1
---
apiVersion: bootstrap.cluster.x-k8s.io/v1beta1
kind: KubeadmConfigTemplate
metadata:
  name: demo-worker
spec:
  template:
    spec:
      joinConfiguration:
        nodeRegistration:
          kubeletExtraArgs:
            provider-id: proxmox://'{{ ds.meta_data.instance_id }}'
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ProxmoxCluster
metadata:
  name: demo
spec:
  dnsServers:
  - 1.1.1.1
`

const testCA = `-----BEGIN CERTIFICATE-----
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

func decodeDocs(t *testing.T, manifest string) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	var docs []map[string]any
	for {
		var d map[string]any
		err := dec.Decode(&d)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("output is not valid YAML: %v", err)
		}
		docs = append(docs, d)
	}
	return docs
}

// nested walks a decoded document, failing the test if any hop is missing.
func nested(t *testing.T, m map[string]any, keys ...string) any {
	t.Helper()
	var cur any = m
	for _, k := range keys {
		asMap, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("expected a mapping at %q, got %T", k, cur)
		}
		cur, ok = asMap[k]
		if !ok {
			t.Fatalf("missing key %q", k)
		}
	}
	return cur
}

func TestInjectRegistryTrustDisabledIsByteIdentical(t *testing.T) {
	out, err := InjectRegistryTrust(sampleManifest, RegistryConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != sampleManifest {
		t.Fatal("manifest was modified even though no registry was configured")
	}
}

func TestInjectRegistryTrustPatchesBothBootstrapKinds(t *testing.T) {
	reg := RegistryConfig{Host: "registry.internal.lan:5000", CACertPEM: testCA}
	out, err := InjectRegistryTrust(sampleManifest, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	docs := decodeDocs(t, out)
	if len(docs) != 4 {
		t.Fatalf("expected 4 documents to survive the round trip, got %d", len(docs))
	}

	// Both bootstrap kinds must be patched; the other two must not sprout a
	// files: key.
	var checked int
	for _, d := range docs {
		kind, _ := d["kind"].(string)
		var spec map[string]any
		switch kind {
		case "KubeadmControlPlane":
			spec = nested(t, d, "spec", "kubeadmConfigSpec").(map[string]any)
		case "KubeadmConfigTemplate":
			spec = nested(t, d, "spec", "template", "spec").(map[string]any)
		default:
			if s, ok := d["spec"].(map[string]any); ok {
				if _, bad := s["files"]; bad {
					t.Fatalf("%s should not have been touched", kind)
				}
			}
			continue
		}
		checked++

		files, ok := spec["files"].([]any)
		if !ok {
			t.Fatalf("%s: files was not injected", kind)
		}
		// script + hosts.toml + ca.crt + system trust cert
		if len(files) != 4 {
			t.Fatalf("%s: expected 4 files, got %d", kind, len(files))
		}

		paths := map[string]string{}
		for _, f := range files {
			fm := f.(map[string]any)
			paths[fm["path"].(string)] = fm["content"].(string)
		}
		hosts, ok := paths["/etc/containerd/certs.d/registry.internal.lan:5000/hosts.toml"]
		if !ok {
			t.Fatalf("%s: hosts.toml missing; got paths %v", kind, paths)
		}
		if !strings.Contains(hosts, `ca = "/etc/containerd/certs.d/registry.internal.lan:5000/ca.crt"`) {
			t.Fatalf("%s: hosts.toml does not pin the CA:\n%s", kind, hosts)
		}
		if strings.Contains(hosts, "skip_verify") {
			t.Fatalf("%s: a CA was supplied, TLS verification must not be skipped", kind)
		}
		if ca := paths["/etc/containerd/certs.d/registry.internal.lan:5000/ca.crt"]; !strings.Contains(ca, "BEGIN CERTIFICATE") {
			t.Fatalf("%s: CA file content is not the PEM we passed in", kind)
		}

		cmds, ok := spec["preKubeadmCommands"].([]any)
		if !ok || len(cmds) != 1 || cmds[0].(string) != registrySetupScriptPath {
			t.Fatalf("%s: preKubeadmCommands not wired up: %v", kind, spec["preKubeadmCommands"])
		}
	}
	if checked != 2 {
		t.Fatalf("expected to patch 2 bootstrap documents, patched %d", checked)
	}
}

// The PEM must land as a readable literal block, not a single \n-escaped
// line — the manifest preview is the feature that makes apply trustworthy.
func TestInjectRegistryTrustUsesLiteralBlocksForContent(t *testing.T) {
	out, err := InjectRegistryTrust(sampleManifest, RegistryConfig{
		Host: "registry.internal.lan:5000", CACertPEM: testCA,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "content: |") {
		t.Fatal("file contents were not emitted as literal blocks")
	}
	if strings.Contains(out, `\n-----END CERTIFICATE-----`) {
		t.Fatal("PEM was escaped onto one line instead of a literal block")
	}
}

// Untouched documents must survive verbatim in meaning — a re-encode that
// drops or mangles unrelated fields would be worse than no feature at all.
func TestInjectRegistryTrustPreservesOtherDocuments(t *testing.T) {
	out, err := InjectRegistryTrust(sampleManifest, RegistryConfig{
		Host: "reg:5000", CACertPEM: testCA,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, d := range decodeDocs(t, out) {
		if d["kind"] == "ProxmoxCluster" {
			dns := nested(t, d, "spec", "dnsServers").([]any)
			if len(dns) != 1 || dns[0].(string) != "1.1.1.1" {
				t.Fatalf("ProxmoxCluster spec was altered: %v", d["spec"])
			}
			return
		}
	}
	t.Fatal("ProxmoxCluster document disappeared from the manifest")
}

func TestInjectRegistryTrustWithoutCASkipsVerification(t *testing.T) {
	out, err := InjectRegistryTrust(sampleManifest, RegistryConfig{Host: "reg:5000"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "skip_verify = true") {
		t.Fatal("no CA given, so hosts.toml should skip verification")
	}
	if strings.Contains(out, "ca = ") {
		t.Fatal("no CA was given but hosts.toml still pins one")
	}
}

func TestInjectRegistryTrustErrorsWhenNoBootstrapDocs(t *testing.T) {
	_, err := InjectRegistryTrust("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n",
		RegistryConfig{Host: "reg:5000", CACertPEM: testCA})
	if err == nil {
		t.Fatal("expected an error when there is nothing to inject into, got nil")
	}
}

func TestNormalizeRegistryHost(t *testing.T) {
	for in, want := range map[string]string{
		"registry.internal.lan:5000":         "registry.internal.lan:5000",
		"https://registry.internal.lan:5000": "registry.internal.lan:5000",
		"http://registry.internal.lan":       "registry.internal.lan",
		"https://registry.internal.lan/v2/":  "registry.internal.lan",
		"  registry.internal.lan:5000  ":     "registry.internal.lan:5000",
	} {
		if got := NormalizeRegistryHost(in); got != want {
			t.Errorf("NormalizeRegistryHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// CAPMOX's real templates ship preKubeadmCommands, so appending must not
// clobber what's already there.
func TestInjectRegistryTrustPreservesExistingCommandsAndFiles(t *testing.T) {
	manifest := `apiVersion: controlplane.cluster.x-k8s.io/v1beta1
kind: KubeadmControlPlane
metadata:
  name: demo-control-plane
spec:
  kubeadmConfigSpec:
    preKubeadmCommands:
    - hostnamectl set-hostname "{{ ds.meta_data.hostname }}"
    files:
    - path: /etc/existing.conf
      owner: root:root
      permissions: "0644"
      content: keep-me
`
	out, err := InjectRegistryTrust(manifest, RegistryConfig{Host: "reg:5000", CACertPEM: testCA})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	spec := nested(t, decodeDocs(t, out)[0], "spec", "kubeadmConfigSpec").(map[string]any)

	cmds := spec["preKubeadmCommands"].([]any)
	if len(cmds) != 2 || !strings.Contains(cmds[0].(string), "hostnamectl") {
		t.Fatalf("existing preKubeadmCommands were not preserved: %v", cmds)
	}
	if cmds[1].(string) != registrySetupScriptPath {
		t.Fatalf("registry script was not appended last: %v", cmds)
	}

	files := spec["files"].([]any)
	if len(files) != 5 {
		t.Fatalf("expected 1 existing + 4 injected files, got %d", len(files))
	}
	if got := files[0].(map[string]any)["path"].(string); got != "/etc/existing.conf" {
		t.Fatalf("existing file was displaced: %q", got)
	}
}

// A key that is present but null must be replaced, not duplicated — a
// duplicate mapping key makes the whole manifest fail to parse.
func TestInjectRegistryTrustHandlesNullFilesKey(t *testing.T) {
	manifest := `apiVersion: bootstrap.cluster.x-k8s.io/v1beta1
kind: KubeadmConfigTemplate
metadata:
  name: demo-worker
spec:
  template:
    spec:
      files:
      preKubeadmCommands:
`
	out, err := InjectRegistryTrust(manifest, RegistryConfig{Host: "reg:5000", CACertPEM: testCA})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// decodeDocs fails the test if duplicate keys made this unparseable.
	spec := nested(t, decodeDocs(t, out)[0], "spec", "template", "spec").(map[string]any)
	if files, ok := spec["files"].([]any); !ok || len(files) != 4 {
		t.Fatalf("files was not replaced with the injected sequence: %v", spec["files"])
	}
	if cmds, ok := spec["preKubeadmCommands"].([]any); !ok || len(cmds) != 1 {
		t.Fatalf("preKubeadmCommands was not replaced: %v", spec["preKubeadmCommands"])
	}
}
