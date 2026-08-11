package capi

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInjectCredentialsRefPatchesOnlyProxmoxCluster(t *testing.T) {
	out, err := InjectCredentialsRef(sampleManifest, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	docs := decodeDocs(t, out)

	// docs[3] is the ProxmoxCluster in sampleManifest (registry_test.go) —
	// its spec.credentialsRef.name must point at connection 42's Secret.
	name := nested(t, docs[3], "spec", "credentialsRef", "name")
	if want := "pvekube-conn-42-credentials"; name != want {
		t.Fatalf("credentialsRef.name = %v, want %q", name, want)
	}
	// namespace is deliberately omitted — see InjectCredentialsRef's doc
	// comment for why (CAPMOX defaults it to the object's own namespace).
	spec := nested(t, docs[3], "spec", "credentialsRef").(map[string]any)
	if _, ok := spec["namespace"]; ok {
		t.Fatalf("credentialsRef.namespace should be omitted, got %v", spec["namespace"])
	}
	// The existing spec.dnsServers sibling key must survive untouched —
	// this is exactly the case ensureMapNode's "mapping already exists"
	// branch exists for.
	dns := nested(t, docs[3], "spec", "dnsServers")
	if list, ok := dns.([]any); !ok || len(list) != 1 || list[0] != "1.1.1.1" {
		t.Fatalf("spec.dnsServers was clobbered: %v", dns)
	}
}

func TestInjectCredentialsRefPreservesOtherDocuments(t *testing.T) {
	out, err := InjectCredentialsRef(sampleManifest, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	docs := decodeDocs(t, out)
	if len(docs) != 4 {
		t.Fatalf("expected 4 documents, got %d", len(docs))
	}
	// Cluster, KubeadmControlPlane, KubeadmConfigTemplate must be
	// byte-for-byte unaffected — only the ProxmoxCluster document changes.
	if nested(t, docs[0], "kind") != "Cluster" || nested(t, docs[0], "metadata", "name") != "demo" {
		t.Fatalf("Cluster document was touched: %v", docs[0])
	}
	if v := nested(t, docs[1], "spec", "version"); v != "v1.32.1" {
		t.Fatalf("KubeadmControlPlane document was touched: %v", docs[1])
	}
	if _, ok := docs[1]["spec"].(map[string]any)["credentialsRef"]; ok {
		t.Fatal("credentialsRef leaked into KubeadmControlPlane, which has no such field")
	}
}

func TestInjectCredentialsRefErrorsWhenNoProxmoxCluster(t *testing.T) {
	noPC := `apiVersion: cluster.x-k8s.io/v1beta1
kind: Cluster
metadata:
  name: demo
`
	if _, err := InjectCredentialsRef(noPC, 1); err == nil {
		t.Fatal("expected an error when the manifest has no ProxmoxCluster document")
	}
}

// A ProxmoxCluster whose spec.credentialsRef is already present-but-null
// ("credentialsRef:" with nothing under it) must be replaced in place, not
// duplicated — a duplicate mapping key would make the whole document fail
// to parse. Mirrors registry_test.go's
// TestInjectRegistryTrustHandlesNullFilesKey for the same underlying
// ensureSeqNode/ensureMapNode class of bug.
func TestInjectCredentialsRefHandlesNullCredentialsRefKey(t *testing.T) {
	manifest := `apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ProxmoxCluster
metadata:
  name: demo
spec:
  dnsServers:
  - 1.1.1.1
  credentialsRef:
`
	out, err := InjectCredentialsRef(manifest, 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	docs := decodeDocs(t, out)
	name := nested(t, docs[0], "spec", "credentialsRef", "name")
	if want := "pvekube-conn-7-credentials"; name != want {
		t.Fatalf("credentialsRef.name = %v, want %q", name, want)
	}
}

// A ProxmoxCluster with no spec.credentialsRef key at all (the common case —
// clusterctl generate never sets one) must get it appended.
func TestInjectCredentialsRefHandlesAbsentCredentialsRef(t *testing.T) {
	manifest := `apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ProxmoxCluster
metadata:
  name: demo
spec:
  dnsServers:
  - 1.1.1.1
`
	out, err := InjectCredentialsRef(manifest, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	docs := decodeDocs(t, out)
	name := nested(t, docs[0], "spec", "credentialsRef", "name")
	if want := "pvekube-conn-3-credentials"; name != want {
		t.Fatalf("credentialsRef.name = %v, want %q", name, want)
	}
}

// Generate() chains InjectRegistryTrust then InjectCredentialsRef over the
// SAME manifest. They touch different document kinds (KubeadmControlPlane /
// KubeadmConfigTemplate vs ProxmoxCluster) so in principle can't collide,
// but "in principle" is exactly the kind of claim worth actually proving —
// this runs both in the real order and checks neither clobbers the other's
// work.
func TestInjectRegistryTrustAndCredentialsRefCompose(t *testing.T) {
	out, err := InjectRegistryTrust(sampleManifest, RegistryConfig{Host: "registry.internal.lan:5000", CACertPEM: testCA})
	if err != nil {
		t.Fatalf("InjectRegistryTrust: %v", err)
	}
	out, err = InjectCredentialsRef(out, 9)
	if err != nil {
		t.Fatalf("InjectCredentialsRef: %v", err)
	}
	docs := decodeDocs(t, out)

	// KubeadmControlPlane (docs[1]) still has its registry files injected.
	files := nested(t, docs[1], "spec", "kubeadmConfigSpec", "files")
	if list, ok := files.([]any); !ok || len(list) != 4 {
		t.Fatalf("registry files missing from KubeadmControlPlane after credentialsRef injection: %v", files)
	}
	// ProxmoxCluster (docs[3]) has credentialsRef, and registry injection
	// didn't touch it (it's not a bootstrap config document).
	name := nested(t, docs[3], "spec", "credentialsRef", "name")
	if want := "pvekube-conn-9-credentials"; name != want {
		t.Fatalf("credentialsRef.name = %v, want %q", name, want)
	}
	if _, ok := docs[3]["spec"].(map[string]any)["files"]; ok {
		t.Fatal("registry files leaked into ProxmoxCluster, which has no such field")
	}
}

// ensureMapNode is the mapping analog of registry.go's ensureSeqNode — same
// three cases, unit-tested directly rather than only indirectly through
// InjectCredentialsRef.
func TestEnsureMapNode(t *testing.T) {
	t.Run("existing mapping is returned, not replaced", func(t *testing.T) {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte("a: 1\nfoo:\n  b: 2\n"), &doc); err != nil {
			t.Fatal(err)
		}
		root := doc.Content[0]
		got := ensureMapNode(root, "foo")
		if got.Kind != yaml.MappingNode {
			t.Fatalf("expected a mapping node, got kind %v", got.Kind)
		}
		if v := mapValueNode(got, "b"); v == nil || v.Value != "2" {
			t.Fatalf("existing sibling key b was lost: %v", v)
		}
	})

	t.Run("null value is replaced with an empty mapping, not duplicated", func(t *testing.T) {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte("a: 1\nfoo:\n"), &doc); err != nil {
			t.Fatal(err)
		}
		root := doc.Content[0]
		got := ensureMapNode(root, "foo")
		if got.Kind != yaml.MappingNode {
			t.Fatalf("expected a mapping node, got kind %v", got.Kind)
		}
		// Exactly one "foo" key must exist in the mapping's flat content —
		// this is the check that would fail if a null value got appended to
		// instead of replaced in place.
		count := 0
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == "foo" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("expected exactly one 'foo' key, found %d — null value was appended to instead of replaced", count)
		}
	})

	t.Run("absent key is appended", func(t *testing.T) {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte("a: 1\n"), &doc); err != nil {
			t.Fatal(err)
		}
		root := doc.Content[0]
		got := ensureMapNode(root, "foo")
		if got.Kind != yaml.MappingNode {
			t.Fatalf("expected a mapping node, got kind %v", got.Kind)
		}
		if v := mapValueNode(root, "foo"); v != got {
			t.Fatal("appended node is not reachable via the parent mapping")
		}
	})
}
