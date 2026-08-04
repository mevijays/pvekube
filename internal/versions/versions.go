// Package versions pins the exact upstream tool/component versions PVEKube drives.
//
// These are NOT arbitrary "latest" picks — they encode a verified compatibility
// matrix (researched 2026-08-02). Bumping any of these requires re-checking the
// matrix in PLAN.md, in particular CAPMOX's supported CAPI core range.
package versions

import "fmt"

// Compat describes one verified-compatible bundle of core + infra + ipam versions.
type Compat struct {
	Name          string
	CAPICore      string // cluster-api core version (clusterctl --core cluster-api:<ver>)
	CAPMOX        string // infrastructure-proxmox provider version
	IPAMInCluster string // ipam-in-cluster provider version
	Notes         string
}

// Default is the bundle PVEKube uses unless a user explicitly overrides it.
var Default = Compat{
	Name:          "default",
	CAPICore:      "v1.12.10",
	CAPMOX:        "v0.9.0",
	IPAMInCluster: "v1.1.0",
	Notes: "CAPMOX v0.9 supports CAPI core v1.11/v1.12 only. Core latest (v1.13.x) is " +
		"NOT compatible — do not upgrade CAPICore past v1.12.x without also confirming a " +
		"newer CAPMOX release supports it.",
}

const (
	// Kind is the bootstrap/management-cluster tool version.
	Kind = "v0.32.0"

	// Kubectl should track a version within the cluster's supported skew.
	Kubectl = "v1.32.3"

	// ClusterctlVersion pins the clusterctl CLI itself (independent of CAPICore,
	// though clusterctl generally targets the same minor line as the core it manages).
	Clusterctl = "v1.12.10"

	// CiliumCLI pins the cilium-cli tool used to actually install Cilium onto
	// a workload cluster when CNI="cilium" is selected. Needed because (unlike
	// Calico) recent Cilium releases dropped their plain-YAML "quick-install"
	// manifest in favor of Helm-driven installs — cilium-cli wraps that Helm
	// install without requiring a separate system Helm binary.
	CiliumCLI = "v0.19.7"

	// IstioctlVersion pins istioctl, used the same way as cilium-cli: driving
	// a Helm-based install without a separate system Helm binary. Note Istio's
	// own release tags have no "v" prefix (used bare in download URLs below).
	IstioctlVersion = "1.30.3"

	// MetricsServerVersion pins the components.yaml applied for the
	// "metrics-server" post-provision addon.
	MetricsServerVersion = "v0.9.0"

	// MetalLBVersion pins the metallb-native.yaml applied for the "MetalLB"
	// post-provision addon.
	MetalLBVersion = "v0.14.9"

	// ImageBuilderImage is the official containerized image-builder used to build
	// Proxmox VM templates without installing Packer/Ansible/Go toolchains on the host.
	ImageBuilderImage = "registry.k8s.io/scl-image-builder/cluster-node-image-builder-amd64:v0.1.55"

	// ImageBuilderRepoRef pins the kubernetes-sigs/image-builder git ref we
	// clone for its Makefile + Packer configs (the container image itself
	// only bundles the toolchain — Packer, Ansible, goss — not the actual
	// build definitions, which are bind-mounted in from this checkout).
	// Kept in lockstep with ImageBuilderImage's tag.
	ImageBuilderRepoRef = "v0.1.55"
)

// ProxmoxProviderURL is the exact (version-pinned, not "latest") infra provider
// components manifest clusterctl should fetch. clusterctl's built-in proxmox
// provider entry defaults to releases/latest, which we deliberately override.
func ProxmoxProviderURL() string {
	return "https://github.com/ionos-cloud/cluster-api-provider-proxmox/releases/download/" +
		Default.CAPMOX + "/infrastructure-components.yaml"
}

// CalicoManifestURL is the pinned upstream Calico install manifest. CAPMOX's
// own "calico" flavor template (verified by fetching cluster-template-calico.yaml
// and templates/crs/cni/ directly from the v0.9.0 release/repo) ships a
// ClusterResourceSet that references a ConfigMap named "calico" — but never
// defines that ConfigMap itself. Its own docs/Usage.md confirms this is
// intentional: "For templates using CNIs you're required to create ConfigMaps
// to make ClusterResourceSets available." This manifest's content is what
// PVEKube puts in that ConfigMap automatically (see EnsureCNIConfigMapStep) —
// without it, CNI="calico" silently installs nothing and every node sits at
// NotReady forever with no error surfaced anywhere.
const CalicoManifestURL = "https://raw.githubusercontent.com/projectcalico/calico/v3.28.0/manifests/calico.yaml"

// MetricsServerManifestURL is the pinned metrics-server install manifest.
func MetricsServerManifestURL() string {
	return "https://github.com/kubernetes-sigs/metrics-server/releases/download/" +
		MetricsServerVersion + "/components.yaml"
}

// MetalLBManifestURL is the pinned MetalLB install manifest.
func MetalLBManifestURL() string {
	return "https://raw.githubusercontent.com/metallb/metallb/" +
		MetalLBVersion + "/config/manifests/metallb-native.yaml"
}

// IstioctlDownloadURL is istioctl's release asset — a flat tar.gz containing
// just the istioctl binary (verified by inspecting the real archive).
func IstioctlDownloadURL(goos, arch string) string {
	return fmt.Sprintf("https://github.com/istio/istio/releases/download/%s/istioctl-%s-%s-%s.tar.gz",
		IstioctlVersion, IstioctlVersion, goos, arch)
}
