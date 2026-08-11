// Per-connection cluster credentials.
//
// A ProxmoxCluster with spec.credentialsRef set gets its Proxmox client
// built from a LIVE read of the referenced Secret on every single CAPMOX
// reconcile (verified by reading CAPMOX v0.9.0's pkg/scope/cluster.go
// directly) — no controller restart, no shared global state, safe for
// concurrent clusters on different Proxmox hosts. Without credentialsRef, a
// ProxmoxCluster falls back to ONE global Secret (capmox-manager-
// credentials) baked into the controller's env vars at pod start — see
// EnsureCredentialsStep in generate.go — which is fine for a single host
// but actively wrong for multiple: whichever host connected last would
// silently redirect every no-credentialsRef cluster's reconciliation to
// itself.
//
// So every cluster created after multi-host support ships gets its own
// dedicated Secret (EnsureConnectionCredentialsSecret, generate.go) and this
// file's InjectCredentialsRef wires the ProxmoxCluster document to it.
package capi

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// connectionCredentialsSecretName is shared between InjectCredentialsRef
// (which points the manifest at it) and EnsureConnectionCredentialsSecret
// (which creates it) — one connection, one Secret, reused by every cluster
// on that host so rotating credentials later only means updating one
// object, not one per cluster.
func connectionCredentialsSecretName(connID int64) string {
	return "pvekube-conn-" + strconv.FormatInt(connID, 10) + "-credentials"
}

// InjectCredentialsRef points the generated ProxmoxCluster document at its
// connection's dedicated credentials Secret. Uses the exact same
// decode-find-mutate-reencode pattern as InjectRegistryTrust in registry.go:
// yaml.Node preserves every other document byte-for-byte, which matters
// here as much as it does there — this manifest is what gets shown on the
// preview screen and stored in the clusters table.
//
// namespace is deliberately omitted from the injected credentialsRef: CAPMOX
// defaults a missing namespace to the ProxmoxCluster's own namespace
// (confirmed in pkg/scope/cluster.go), which is always "default" in
// PVEKube's generated manifests — so the Secret just needs to live there
// too, which EnsureConnectionCredentialsSecret does.
func InjectCredentialsRef(manifestYAML string, connID int64) (string, error) {
	dec := yaml.NewDecoder(strings.NewReader(manifestYAML))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return "", fmt.Errorf("parsing generated manifest: %w", err)
		}
		docs = append(docs, &doc)
	}

	patched := 0
	for _, doc := range docs {
		root := doc
		if root.Kind == yaml.DocumentNode {
			if len(root.Content) == 0 {
				continue
			}
			root = root.Content[0]
		}
		kindNode := mapValueNode(root, "kind")
		if kindNode == nil || kindNode.Value != "ProxmoxCluster" {
			continue
		}
		spec := ensureMapNode(root, "spec")
		credRef := ensureMapNode(spec, "credentialsRef")
		setMapValue(credRef, "name", connectionCredentialsSecretName(connID))
		patched++
	}
	if patched == 0 {
		return "", fmt.Errorf("credentialsRef injection requested but the generated manifest had no ProxmoxCluster document")
	}

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return "", fmt.Errorf("re-encoding manifest: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("re-encoding manifest: %w", err)
	}
	return out.String(), nil
}

// ensureMapNode is ensureSeqNode's (registry.go) mapping analog: returns the
// mapping stored at key, creating one if needed. Same three cases, same
// reason they matter — getting the "present but null" case wrong would
// append a second "spec" or "credentialsRef" key, producing invalid YAML
// with a duplicate mapping key:
//   - key holds a mapping already: return it so existing sibling keys under
//     it (there are none for credentialsRef today, but spec always has
//     plenty) are preserved, not overwritten;
//   - key is present but null: replace the value in place;
//   - key is absent: append the new key/value pair.
func ensureMapNode(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		if v := m.Content[i+1]; v.Kind == yaml.MappingNode {
			return v
		}
		mp := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		m.Content[i+1] = mp
		return mp
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, k, v)
	return v
}

// setMapValue sets key=value on a mapping node, replacing the value if the
// key already exists (rather than appending a duplicate) — credentialsRef
// is always freshly created by ensureMapNode within one InjectCredentialsRef
// call, so in practice this always takes the "absent" path, but written to
// be correct regardless in case a future caller reuses it against an
// existing mapping.
func setMapValue(m *yaml.Node, key, value string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}
