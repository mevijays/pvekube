// OIDC authentication for the workload cluster's API server — Dex, GitHub
// (via Dex), Azure Entra ID, or any standard OIDC provider.
//
// Like registry trust (registry.go), this has to be baked into the
// manifest at generate time, not applied as a kubectl patch after the
// cluster exists: kube-apiserver only reads --oidc-* flags at process
// startup, and CAPI does not retroactively re-render an already-created
// control-plane machine's bootstrap data when KubeadmControlPlane.spec
// changes later — a patch against a live cluster only affects a NEW or
// replaced control-plane machine, not the one already running. Baking it
// into the manifest means the API server has OIDC configured from its very
// first start, with no window where it's up without auth configured.
//
// This only wires AUTHENTICATION. An OIDC-authenticated user or group gets
// exactly zero permissions until separate RBAC (Role/ClusterRoleBinding)
// grants them some — kube-apiserver's OIDC plugin only answers "who is
// this", never "what can they do". Surfaced as a note in the UI, not
// enforced here.
package capi

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"pvekube/internal/jobs"
)

// oidcCACertPath is where the IdP's CA lands on the control-plane node,
// matching kube-apiserver's own convention of keeping certs under its pki
// directory. Both cloud-init and Ignition create missing parent
// directories when writing a file, so this doesn't need /etc/kubernetes/pki
// to already exist — kubeadm itself creates the rest of that directory
// later, after this file is already in place.
const oidcCACertPath = "/etc/kubernetes/pki/oidc-ca.pem"

// OIDCConfig is the workload cluster's API server OIDC authentication
// setup, collected from the cluster creation form.
type OIDCConfig struct {
	// Provider is informational only (drives which setup hint the UI shows)
	// — it doesn't change what gets injected. Every provider ends up as the
	// same five --oidc-* flags; only how an operator arrives at the right
	// IssuerURL/ClientID/CACertPEM values differs.
	Provider string // "dex" | "github" | "azuread" | "generic"

	IssuerURL     string // oidc-issuer-url, e.g. "https://dex.example.com" or "https://login.microsoftonline.com/<tenant>/v2.0"
	ClientID      string // oidc-client-id — the audience your ID tokens are issued for
	UsernameClaim string // oidc-username-claim; defaults to "email" if empty
	GroupsClaim   string // oidc-groups-claim; defaults to "groups" if empty
	CACertPEM     string // optional — only needed for a self-signed/internal-CA issuer (typically self-hosted Dex)

	// DefaultUsersGroup, when set, gets a ClusterRoleBinding to the built-in
	// "view" ClusterRole (read-only cluster-wide, excluding Secrets and RBAC
	// objects — the conventional "cluster-reader" role) created automatically
	// once the cluster is up. This is the ONLY name used as-is: since
	// InjectOIDCAuth never sets --oidc-groups-prefix, whatever the IdP's
	// groups claim returns is what RBAC sees, unprefixed. Optional — OIDC
	// auth works fine without it, just leaves every authenticated user with
	// zero permissions until RBAC is set up by hand.
	DefaultUsersGroup string
}

// Enabled reports whether OIDC was actually configured. Everything in this
// file is a no-op when it isn't, matching RegistryConfig.Enabled's
// contract — a cluster created without OIDC produces byte-identical output
// to before this feature existed.
func (o OIDCConfig) Enabled() bool {
	return strings.TrimSpace(o.IssuerURL) != "" && strings.TrimSpace(o.ClientID) != ""
}

func (o OIDCConfig) usernameClaim() string {
	if c := strings.TrimSpace(o.UsernameClaim); c != "" {
		return c
	}
	return "email"
}

func (o OIDCConfig) groupsClaim() string {
	if c := strings.TrimSpace(o.GroupsClaim); c != "" {
		return c
	}
	return "groups"
}

// InjectOIDCAuth sets the workload cluster's API server --oidc-* flags via
// KubeadmControlPlane's spec.kubeadmConfigSpec.clusterConfiguration.
// apiServer.extraArgs, and — if a CA was supplied — adds it as a control-
// plane file so kube-apiserver can read it at startup.
//
// extraArgs is a LIST of {name, value} pairs here, not a map. That's not a
// style choice: kubeadm's v1beta4 API (what this app's pinned CABPK/CAPMOX
// generate for Kubernetes 1.31+) changed every ExtraArgs field from
// map[string]string to []Arg — confirmed by reading a real generated
// KubeadmControlPlane's kubeletExtraArgs directly, which already uses this
// same list shape. The old map syntax (still what most copy-pasted kubeadm
// patch examples online show, v1beta3 and earlier) is silently the WRONG
// shape here and would fail validation.
func InjectOIDCAuth(manifestYAML string, oidc OIDCConfig) (string, error) {
	if !oidc.Enabled() {
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
		root := doc
		if root.Kind == yaml.DocumentNode {
			if len(root.Content) == 0 {
				continue
			}
			root = root.Content[0]
		}
		kindNode := mapValueNode(root, "kind")
		if kindNode == nil || kindNode.Value != "KubeadmControlPlane" {
			continue
		}
		configSpec := mapValueNode(mapValueNode(root, "spec"), "kubeadmConfigSpec")
		if configSpec == nil {
			continue
		}

		clusterConfig := ensureMapNode(configSpec, "clusterConfiguration")
		apiServer := ensureMapNode(clusterConfig, "apiServer")
		extraArgs := ensureSeqNode(apiServer, "extraArgs")

		appendExtraArg(extraArgs, "oidc-issuer-url", oidc.IssuerURL)
		appendExtraArg(extraArgs, "oidc-client-id", oidc.ClientID)
		appendExtraArg(extraArgs, "oidc-username-claim", oidc.usernameClaim())
		appendExtraArg(extraArgs, "oidc-groups-claim", oidc.groupsClaim())

		if ca := strings.TrimSpace(oidc.CACertPEM); ca != "" {
			appendExtraArg(extraArgs, "oidc-ca-file", oidcCACertPath)

			filesNode := ensureSeqNode(configSpec, "files")
			n, err := literalContentNode(cloudInitFile{
				Path: oidcCACertPath, Owner: "root:root", Permissions: "0644", Content: ca + "\n",
			})
			if err != nil {
				return "", err
			}
			filesNode.Content = append(filesNode.Content, n)
		}

		patched++
	}
	if patched == 0 {
		return "", fmt.Errorf("OIDC auth requested but the generated manifest had no KubeadmControlPlane to inject into")
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

// defaultUsersClusterRoleBindingName is fixed rather than derived from the
// group name so re-applying (e.g. on a re-run after changing the group in
// the form and recreating) doesn't accumulate stale bindings under the old
// name — there's only ever one "default users" binding per cluster.
const defaultUsersClusterRoleBindingName = "pvekube-oidc-default-users-view"

// InstallOIDCDefaultGroupRBACStep grants DefaultUsersGroup read-only
// cluster-wide access (the built-in "view" ClusterRole) once the workload
// cluster is reachable. A no-op if DefaultUsersGroup is empty — OIDC auth
// alone grants nothing, so most operators will want at least this much
// wired up automatically rather than starting from zero RBAC.
//
// Uses create --dry-run=client -o yaml | apply -f -, the same idempotent
// create-or-update idiom InstallRegistryCredentialsStep (registry.go) uses,
// so re-running an apply (e.g. after scaling) doesn't fail on "already
// exists".
func InstallOIDCDefaultGroupRBACStep(dataDir, binDir, clusterName, group string) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		kcPath, cleanup, err := waitForWorkloadKubeconfig(c, dataDir, binDir, clusterName, "granting default OIDC group access")
		if err != nil {
			return err
		}
		defer cleanup()

		kubectlBin := filepath.Join(binDir, "kubectl")

		var rendered bytes.Buffer
		renderCmd := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath,
			"create", "clusterrolebinding", defaultUsersClusterRoleBindingName,
			"--clusterrole=view", "--group="+group,
			"--dry-run=client", "-o", "yaml")
		renderCmd.Stdout = &rendered
		var renderErr bytes.Buffer
		renderCmd.Stderr = &renderErr
		if err := renderCmd.Run(); err != nil {
			return fmt.Errorf("rendering ClusterRoleBinding for OIDC group %q: %w\n%s", group, err, renderErr.String())
		}

		applyCmd := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath, "apply", "-f", "-")
		applyCmd.Stdin = strings.NewReader(rendered.String())
		var applyOut bytes.Buffer
		applyCmd.Stdout, applyCmd.Stderr = &applyOut, &applyOut
		if err := applyCmd.Run(); err != nil {
			return fmt.Errorf("applying ClusterRoleBinding for OIDC group %q: %w\n%s", group, err, applyOut.String())
		}
		c.Logf("Group %q granted the built-in 'view' ClusterRole cluster-wide (read-only, excludes Secrets) via %s", group, defaultUsersClusterRoleBindingName)
		return nil
	}
}

// appendExtraArg appends one {name: ..., value: ...} entry to an extraArgs
// sequence — the v1beta4 kubeadm API's shape for every *ExtraArgs field.
func appendExtraArg(seq *yaml.Node, name, value string) {
	entry := &yaml.Node{
		Kind: yaml.MappingNode, Tag: "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: name},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "value"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
		},
	}
	seq.Content = append(seq.Content, entry)
}
