// Internal container registry support.
//
// Unlike the post-provision addons in addons.go, registry trust CANNOT be
// delivered with `kubectl apply` after the cluster is up: containerd has to
// already trust the registry's CA the first time kubelet pulls an image,
// which happens during `kubeadm init` — long before there's an API server to
// apply anything to. So the trust half of this feature is injected into the
// CAPI manifest itself (KubeadmControlPlane / KubeadmConfigTemplate), which
// has a second benefit: because it lives in the machine *templates*, workers
// added later by "Scale workers" inherit it automatically instead of
// silently coming up unable to pull.
//
// Registry credentials are deliberately NOT part of that manifest. The
// manifest is rendered verbatim on the preview screen and stored in the
// clusters table, so anything embedded in it is visible on screen and at
// rest. Credentials instead go in as a normal dockerconfigjson Secret after
// the cluster is reachable — see InstallRegistryCredentialsStep.
package capi

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"pvekube/internal/jobs"
	"pvekube/internal/runner"
)

// RegistrySecretName is the dockerconfigjson Secret created in the workload
// cluster, and the name wired into the default ServiceAccount's
// imagePullSecrets.
const RegistrySecretName = "pvekube-registry"

// registrySetupScriptPath is written by cloud-init (via the manifest's
// files:) and then run from preKubeadmCommands. It's a real script rather
// than a pile of inline commands so that what runs on the node is legible in
// the manifest preview instead of a wall of escaped shell.
const registrySetupScriptPath = "/etc/pvekube/configure-registry.sh"

// RegistryConfig is an existing internal registry the new cluster's nodes
// should trust and be able to pull from, collected from the cluster creation
// form.
type RegistryConfig struct {
	Host      string // "registry.internal.lan:5000" — no scheme
	CACertPEM string // PEM CA bundle; empty means "don't verify TLS" (see hostsTOML)
	Username  string // optional
	Password  string // optional
}

// Enabled reports whether a registry was actually configured. Everything in
// this file is a no-op when it isn't, so a cluster created without one
// produces byte-identical output to before this feature existed.
func (r RegistryConfig) Enabled() bool { return strings.TrimSpace(r.Host) != "" }

// HasAuth reports whether credentials were supplied for authenticated pulls.
func (r RegistryConfig) HasAuth() bool {
	return r.Enabled() && strings.TrimSpace(r.Username) != ""
}

// NormalizeRegistryHost strips a scheme and any trailing path/slash, since
// containerd's certs.d directory and hosts.toml both key off a bare
// host[:port]. Operators reliably paste "https://registry:5000" here.
func NormalizeRegistryHost(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// certsDir is containerd's per-registry config directory for this host.
func (r RegistryConfig) certsDir() string {
	return "/etc/containerd/certs.d/" + r.Host
}

// systemCertPath is where the CA also gets dropped for the OS trust store, so
// non-containerd tooling on the node (curl, crictl against an HTTPS endpoint,
// anything using the system bundle) trusts the registry too.
func (r RegistryConfig) systemCertPath() string {
	safe := strings.NewReplacer(":", "_", "/", "_", " ", "_").Replace(r.Host)
	return "/usr/local/share/ca-certificates/pvekube-" + safe + ".crt"
}

// hostsTOML is containerd's per-registry config. With a CA we point at the
// pinned cert; without one there's nothing to verify against, so the only
// way the pull can succeed is to skip verification — surfaced in the UI as
// the insecure option rather than silently doing it.
func (r RegistryConfig) hostsTOML() string {
	url := "https://" + r.Host
	var b strings.Builder
	fmt.Fprintf(&b, "server = %q\n\n", url)
	fmt.Fprintf(&b, "[host.%q]\n", url)
	b.WriteString("  capabilities = [\"pull\", \"resolve\"]\n")
	if strings.TrimSpace(r.CACertPEM) != "" {
		fmt.Fprintf(&b, "  ca = %q\n", r.certsDir()+"/ca.crt")
	} else {
		b.WriteString("  skip_verify = true\n")
	}
	return b.String()
}

// setupScript makes containerd actually consult /etc/containerd/certs.d.
// Dropping hosts.toml there does nothing on its own: containerd only reads
// that tree when config_path is set under the CRI registry plugin. Recent
// image-builder images do set it, but that's an assumption about someone
// else's default, so this verifies and repairs it rather than trusting it.
func (r RegistryConfig) setupScript() string {
	return `#!/bin/bash
# Written by PVEKube. Makes this node trust and pull from the internal
# registry ` + r.Host + `.
set -euo pipefail

CONF=/etc/containerd/config.toml

# Pick up the CA we just dropped into /usr/local/share/ca-certificates.
update-ca-certificates || true

# containerd ignores /etc/containerd/certs.d unless config_path points at it.
if ! grep -q 'io.containerd.grpc.v1.cri".registry\]' "$CONF"; then
  printf '\n[plugins."io.containerd.grpc.v1.cri".registry]\n  config_path = "/etc/containerd/certs.d"\n' >> "$CONF"
elif ! grep -qE '^[[:space:]]*config_path[[:space:]]*=' "$CONF"; then
  sed -i 's|\(\[plugins\."io\.containerd\.grpc\.v1\.cri"\.registry\]\)|\1\n    config_path = "/etc/containerd/certs.d"|' "$CONF"
fi

systemctl restart containerd

# kubeadm starts pulling almost immediately after this returns, so don't hand
# back control until containerd is actually answering again.
for _ in $(seq 1 30); do
  if ctr version >/dev/null 2>&1; then exit 0; fi
  sleep 1
done
echo "containerd did not come back within 30s after restart" >&2
exit 1
`
}

// cloudInitFile mirrors the kubeadm bootstrap provider's file schema.
type cloudInitFile struct {
	Path        string `yaml:"path"`
	Owner       string `yaml:"owner"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
}

func (r RegistryConfig) files() []cloudInitFile {
	files := []cloudInitFile{
		{Path: registrySetupScriptPath, Owner: "root:root", Permissions: "0755", Content: r.setupScript()},
		{Path: r.certsDir() + "/hosts.toml", Owner: "root:root", Permissions: "0644", Content: r.hostsTOML()},
	}
	if ca := strings.TrimSpace(r.CACertPEM); ca != "" {
		ca += "\n"
		files = append(files,
			cloudInitFile{Path: r.certsDir() + "/ca.crt", Owner: "root:root", Permissions: "0644", Content: ca},
			cloudInitFile{Path: r.systemCertPath(), Owner: "root:root", Permissions: "0644", Content: ca},
		)
	}
	return files
}

// InjectRegistryTrust adds the registry's CA, containerd host config, and
// setup script to every machine the cluster will create, by appending to the
// files/preKubeadmCommands of both KubeadmControlPlane (control plane nodes)
// and KubeadmConfigTemplate (workers).
//
// This parses the YAML rather than string-splicing it. The existing tweaks in
// Generate get away with regex because they rewrite scalars in place; adding
// structured list entries under a specific nested key in two specific
// documents does not survive that approach. Documents are decoded into
// yaml.Node (not map[string]any) so key order and comments in the untouched
// ~95% of the manifest are preserved and the preview stays readable.
func InjectRegistryTrust(manifestYAML string, reg RegistryConfig) (string, error) {
	if !reg.Enabled() {
		return manifestYAML, nil
	}

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
		configSpec := bootstrapConfigSpec(doc)
		if configSpec == nil {
			continue
		}
		filesNode := ensureSeqNode(configSpec, "files")
		for _, f := range reg.files() {
			n, err := literalContentNode(f)
			if err != nil {
				return "", err
			}
			filesNode.Content = append(filesNode.Content, n)
		}
		cmdsNode := ensureSeqNode(configSpec, "preKubeadmCommands")
		cmdsNode.Content = append(cmdsNode.Content, &yaml.Node{
			Kind: yaml.ScalarNode, Tag: "!!str", Value: registrySetupScriptPath,
		})
		patched++
	}
	if patched == 0 {
		return "", fmt.Errorf("registry trust requested but the generated manifest had no KubeadmControlPlane or KubeadmConfigTemplate to inject into")
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

// bootstrapConfigSpec returns the mapping that holds files/preKubeadmCommands
// for a kubeadm bootstrap document, or nil if this document isn't one. The
// two kinds nest that mapping at different depths.
func bootstrapConfigSpec(doc *yaml.Node) *yaml.Node {
	root := doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil
		}
		root = root.Content[0]
	}
	kindNode := mapValueNode(root, "kind")
	if kindNode == nil {
		return nil
	}
	spec := mapValueNode(root, "spec")
	switch kindNode.Value {
	case "KubeadmControlPlane":
		return mapValueNode(spec, "kubeadmConfigSpec")
	case "KubeadmConfigTemplate":
		return mapValueNode(mapValueNode(spec, "template"), "spec")
	}
	return nil
}

// mapValueNode returns the value node for key within a mapping node. yaml.Node
// mappings are a flat [k1, v1, k2, v2, ...] slice, hence the stride of 2.
func mapValueNode(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// ensureSeqNode returns the sequence stored at key so callers can append to
// it, creating one if needed. Three cases matter, and getting any of them
// wrong silently corrupts the manifest:
//   - key holds a sequence (upstream templates DO ship preKubeadmCommands):
//     return it so existing entries are preserved, not overwritten;
//   - key is present but null ("files:" with nothing under it): replace the
//     value in place — appending another "files" key would emit a duplicate
//     key, which is invalid YAML;
//   - key is absent: append the new key/value pair.
func ensureSeqNode(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		if v := m.Content[i+1]; v.Kind == yaml.SequenceNode {
			return v
		}
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		m.Content[i+1] = seq
		return seq
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	v := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	m.Content = append(m.Content, k, v)
	return v
}

// literalContentNode encodes one file entry, forcing its multi-line content
// to render as a literal block ("content: |") instead of a single escaped
// line. Purely for the preview: an inlined PEM with \n escapes is unreadable
// and makes the manifest impossible to eyeball before applying.
func literalContentNode(f cloudInitFile) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(f); err != nil {
		return nil, fmt.Errorf("encoding file entry %s: %w", f.Path, err)
	}
	if c := mapValueNode(&n, "content"); c != nil {
		c.Style = yaml.LiteralStyle
	}
	return &n, nil
}

// InstallRegistryCredentialsStep creates the dockerconfigjson Secret in the
// workload cluster and wires it into the default namespace's default
// ServiceAccount, so pods there pull from the private registry without every
// manifest having to name an imagePullSecret.
//
// Nothing here goes through runner.Run, which echoes the command it runs into
// the job log — that would print the registry password into a log file that
// persists on disk and streams to the browser.
func InstallRegistryCredentialsStep(dataDir, binDir, clusterName string, reg RegistryConfig) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		kcPath, cleanup, err := waitForWorkloadKubeconfig(c, dataDir, binDir, clusterName, "configuring registry credentials")
		if err != nil {
			return err
		}
		defer cleanup()

		kubectlBin := filepath.Join(binDir, "kubectl")

		// create --dry-run=client | apply is the standard create-or-update
		// idiom, and matters here because re-applying must not fail if a
		// previous run already made the Secret.
		var rendered bytes.Buffer
		createCmd := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath,
			"create", "secret", "docker-registry", RegistrySecretName,
			"--docker-server="+reg.Host,
			"--docker-username="+reg.Username,
			"--docker-password="+reg.Password,
			"-n", "default", "--dry-run=client", "-o", "yaml")
		createCmd.Stdout = &rendered
		var createErr bytes.Buffer
		createCmd.Stderr = &createErr
		if err := createCmd.Run(); err != nil {
			return fmt.Errorf("rendering registry Secret: %w\n%s", err, createErr.String())
		}

		applyCmd := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath, "apply", "-f", "-")
		applyCmd.Stdin = bytes.NewReader(rendered.Bytes())
		var applyOut bytes.Buffer
		applyCmd.Stdout, applyCmd.Stderr = &applyOut, &applyOut
		if err := applyCmd.Run(); err != nil {
			return fmt.Errorf("applying registry Secret: %w\n%s", err, applyOut.String())
		}
		c.Logf("Secret %s/%s created for registry %s", "default", RegistrySecretName, reg.Host)

		if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
			"patch", "serviceaccount", "default", "-n", "default",
			"-p", `{"imagePullSecrets":[{"name":"`+RegistrySecretName+`"}]}`); err != nil {
			return fmt.Errorf("wiring imagePullSecrets into the default ServiceAccount: %w", err)
		}

		c.Logf("Pods in the 'default' namespace will now pull from %s automatically.", reg.Host)
		c.Logf("For other namespaces, copy the Secret and patch that namespace's ServiceAccount:")
		c.Logf("  kubectl get secret %s -n default -o yaml | sed 's/namespace: default/namespace: <ns>/' | kubectl apply -f -", RegistrySecretName)
		c.Logf("  kubectl patch serviceaccount default -n <ns> -p '{\"imagePullSecrets\":[{\"name\":\"%s\"}]}'", RegistrySecretName)
		return nil
	}
}
