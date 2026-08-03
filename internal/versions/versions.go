// Package versions pins the exact upstream tool/component versions PVEKube drives.
//
// These are NOT arbitrary "latest" picks — they encode a verified compatibility
// matrix (researched 2026-08-02). Bumping any of these requires re-checking the
// matrix in PLAN.md, in particular CAPMOX's supported CAPI core range.
package versions

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
