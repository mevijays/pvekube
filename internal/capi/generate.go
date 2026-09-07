// Package capi drives clusterctl against the KIND management cluster to
// render and apply Cluster API manifests for Proxmox workload clusters.
// Variable names and formats here are taken directly from CAPMOX's own
// cluster-template.yaml and docs/Usage.md (ionos-cloud/cluster-api-provider-proxmox
// v0.9.0) — verified by fetching and reading the real template, not guessed.
package capi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"pvekube/internal/bootstrap"
	"pvekube/internal/ipplan"
	"pvekube/internal/jobs"
	"pvekube/internal/runner"
	"pvekube/internal/versions"
)

// CNIFlavor selects which cluster-template-*.yaml clusterctl renders.
// "" (empty) is CAPMOX's plain cluster-template.yaml with no CNI installed.
type CNIFlavor string

const (
	CNIDefault CNIFlavor = ""
	CNICilium  CNIFlavor = "cilium"
	CNICalico  CNIFlavor = "calico"
)

// GenerateInput is every value CAPMOX's cluster-template.yaml substitutes,
// collected from: the selected template (SourceNode, TemplateVMID,
// KubernetesVersion), the Proxmox connection (ProxmoxURL/TokenID/Secret),
// discovery (AllowedNodes, Bridge), and the cluster designer form
// (everything else).
type GenerateInput struct {
	ClusterName       string
	KubernetesVersion string
	ControlPlaneCount int
	WorkerCount       int
	CNI               CNIFlavor

	ProxmoxURL     string
	ProxmoxTokenID string
	ProxmoxSecret  string

	SourceNode   string
	TemplateVMID int
	AllowedNodes []string
	VMSSHKeys    []string

	// OSFlavor is the selected template's OS (templates.os_flavor, e.g.
	// "ubuntu-2404" or "flatcar") — used only to decide whether the
	// bootstrap payload needs Ignition instead of cloud-config. See
	// InjectIgnitionFormat in ignition.go.
	OSFlavor string

	ControlPlaneEndpointIP string
	NodeIPRange            string // "start-end"
	Gateway                string
	IPPrefix               int
	DNSServers             []string
	Bridge                 string

	BootVolumeDevice string // defaults to "scsi0" if empty
	BootVolumeSizeGB int    // defaults to 100 if 0
	NumSockets       int    // defaults to 2 if 0
	NumCores         int    // defaults to 4 if 0
	MemoryMiB        int    // defaults to 8048 if 0

	// Registry, when set, has its CA and containerd config injected into
	// every machine template — see InjectRegistryTrust. Zero value means no
	// registry and leaves the manifest untouched.
	Registry RegistryConfig

	// InternalCA is the organisation's private CA, in PEM. Distinct from
	// Registry.CACertPEM, which only ever covered the registry: this is the
	// CA behind *any* internal HTTPS endpoint the nodes or workloads talk
	// to, so it is configurable without a registry and reaches places the
	// registry CA never did. See trust.go. Empty leaves the manifest
	// untouched.
	InternalCA string

	// PrivateDNS routes internal domains to internal resolvers in the
	// workload cluster's CoreDNS. Applied after the cluster is up (it is a
	// ConfigMap, not bootstrap data), so it is carried here only so the
	// preview and the apply job agree on one source of truth. See dns.go.
	PrivateDNS PrivateDNSConfig

	// OIDC, when set, wires the workload cluster's API server to
	// authenticate against Dex/GitHub/Azure Entra ID/any OIDC provider —
	// see InjectOIDCAuth. Zero value means no OIDC and leaves the manifest
	// untouched.
	OIDC OIDCConfig

	// ConnectionID identifies which Proxmox connection this cluster targets
	// — used to inject spec.credentialsRef into the generated ProxmoxCluster
	// document (see InjectCredentialsRef in credentials.go) so the cluster
	// resolves its own dedicated credentials Secret rather than depending on
	// CAPMOX's shared global fallback, which is only ever correct for one
	// connection at a time.
	ConnectionID int64
}

func (in GenerateInput) env() []string {
	bootDevice := in.BootVolumeDevice
	if bootDevice == "" {
		bootDevice = "scsi0"
	}
	bootSize := in.BootVolumeSizeGB
	if bootSize == 0 {
		bootSize = 100
	}
	sockets := in.NumSockets
	if sockets == 0 {
		sockets = 2
	}
	cores := in.NumCores
	if cores == 0 {
		cores = 4
	}
	mem := in.MemoryMiB
	if mem == 0 {
		mem = 8048
	}

	return []string{
		"PROXMOX_URL=" + in.ProxmoxURL,
		"PROXMOX_TOKEN=" + in.ProxmoxTokenID,
		"PROXMOX_SECRET=" + in.ProxmoxSecret,

		"PROXMOX_SOURCENODE=" + in.SourceNode,
		fmt.Sprintf("TEMPLATE_VMID=%d", in.TemplateVMID),
		"ALLOWED_NODES=" + bracketList(in.AllowedNodes),
		"VM_SSH_KEYS=" + strings.Join(in.VMSSHKeys, ", "),

		"CONTROL_PLANE_ENDPOINT_IP=" + in.ControlPlaneEndpointIP,
		"NODE_IP_RANGES=" + ipplan.CAPMOXRangeSyntax(in.NodeIPRange),
		"GATEWAY=" + in.Gateway,
		fmt.Sprintf("IP_PREFIX=%d", in.IPPrefix),
		"DNS_SERVERS=" + bracketList(in.DNSServers),
		"BRIDGE=" + in.Bridge,

		"BOOT_VOLUME_DEVICE=" + bootDevice,
		fmt.Sprintf("BOOT_VOLUME_SIZE=%d", bootSize),
		fmt.Sprintf("NUM_SOCKETS=%d", sockets),
		fmt.Sprintf("NUM_CORES=%d", cores),
		fmt.Sprintf("MEMORY_MIB=%d", mem),

		// Required by CAPMOX itself (docs/Usage.md): CNI delivery via
		// ClusterResourceSet and ClusterClass templating support.
		"EXP_CLUSTER_RESOURCE_SET=true",
		"CLUSTER_TOPOLOGY=true",
		"EXP_KUBEADM_BOOTSTRAP_FORMAT_IGNITION=true",
	}
}

func bracketList(items []string) string {
	return "[" + strings.Join(items, ",") + "]"
}

// clusterctlConfigPath must match what bootstrap.InitSpec wrote — both need
// the same pinned provider versions/URLs so `generate` fetches the same
// cluster-template.yaml release the running controllers actually match.
func clusterctlConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "clusterctl", "clusterctl.yaml")
}

// Generate renders (but does not apply) the cluster manifest by shelling
// out to `clusterctl generate cluster`. This is read-only against the
// management cluster — it only needs KUBECONFIG to know which providers
// are installed, it doesn't create anything.
func Generate(ctx context.Context, dataDir, binDir string, in GenerateInput) (string, error) {
	clusterctlBin := filepath.Join(binDir, "clusterctl")
	args := []string{"generate", "cluster", in.ClusterName,
		"--infrastructure", "proxmox",
		"--kubernetes-version", in.KubernetesVersion,
		"--control-plane-machine-count", fmt.Sprint(in.ControlPlaneCount),
		"--worker-machine-count", fmt.Sprint(in.WorkerCount),
	}
	if in.CNI != CNIDefault {
		args = append(args, "--flavor", string(in.CNI))
	}

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, clusterctlBin, args...)
	cmd.Env = append(os.Environ(),
		"KUBECONFIG="+bootstrap.KubeconfigPath(dataDir),
		"CLUSTERCTL_CONFIG="+clusterctlConfigPath(dataDir),
	)
	cmd.Env = append(cmd.Env, in.env()...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("clusterctl generate cluster failed: %w\n%s", err, stderr.String())
	}

	// Tweak: Use Linked Clones instead of Full Clones to vastly speed up VM cloning
	// and power-on time. The default cluster-template.yaml from CAPMOX uses full: true.
	manifest := strings.ReplaceAll(stdout.String(), "full: true", "full: false")

	// CAPMOX webhook strictly requires format to NOT be set when full is false.
	// We use regex to remove any format: qcow2 or format: "qcow2" lines.
	manifest = regexp.MustCompile(`(?m)^\s*format:\s*"?qcow2"?\s*$`).ReplaceAllString(manifest, "")

	// Internal registry trust has to be baked into the machine templates,
	// not applied afterwards — containerd needs the CA before kubeadm pulls
	// its first image. No-op (and byte-identical output) when unset.
	manifest, err := InjectRegistryTrust(manifest, in.Registry)
	if err != nil {
		return "", err
	}

	// OIDC auth needs to be live before the API server ever starts —
	// there's no window where a kubectl patch afterward would actually
	// take effect on the already-running process. No-op when unset.
	manifest, err = InjectOIDCAuth(manifest, in.OIDC)
	if err != nil {
		return "", err
	}

	// Point the cluster at its own connection's dedicated credentials
	// Secret rather than CAPMOX's shared global fallback — see
	// credentials.go's package doc comment for why that matters once more
	// than one Proxmox host is in play. Unlike registry trust this is never
	// a no-op: every cluster gets credentialsRef now, regardless of which
	// connection it targets.
	manifest, err = InjectCredentialsRef(manifest, in.ConnectionID)
	if err != nil {
		return "", err
	}

	// The internal CA goes into every node's OS trust store. Must precede
	// InjectIgnitionFormat: that pass rewrites /usr paths for Flatcar, so
	// this one has to have written them already. No-op when unset.
	manifest, err = InjectInternalCATrust(manifest, in.InternalCA)
	if err != nil {
		return "", err
	}

	// Flatcar images have no cloud-init — without this, control-plane and
	// worker VMs boot, get an IP, and then never run kubeadm at all. No-op
	// for every other flavor. See ignition.go.
	manifest, err = InjectIgnitionFormat(manifest, in.OSFlavor)
	if err != nil {
		return "", err
	}

	return manifest, nil
}

// EnsureConnectionCredentialsSecret creates or updates the dedicated
// credentials Secret one Proxmox connection's clusters point at via
// spec.credentialsRef (see credentials.go). This needs NO controller
// restart: CAPMOX reads a credentialsRef Secret live via the Kubernetes
// client on every single reconcile (verified against CAPMOX v0.9.0's
// pkg/scope/cluster.go directly), so an update here takes effect on the
// next reconcile of every cluster referencing it — typically seconds. This
// is now the ONLY credentials path PVEKube writes to; see ClusterConnection
// (below) for why the old global-Secret-plus-restart mechanism was removed
// entirely rather than kept as a "legacy" fallback.
//
// Lives in the `default` namespace (not capmox-system, where the legacy
// global Secret lives) — same namespace as the ProxmoxCluster objects that
// reference it, which lets InjectCredentialsRef omit credentialsRef's
// optional namespace field entirely (CAPMOX defaults a missing one to the
// referencing object's own namespace).
//
// insecureTLS is written explicitly as the Secret's "insecure" key, always
// — CAPMOX defaults InsecureSkipVerify to TRUE when that key is absent
// entirely ("pre-v0.7 backward-compat behavior", per its own source
// comment), so omitting it would silently disable TLS verification
// regardless of what the operator configured for this connection.
//
// One Secret is shared by every cluster on this connection, not owned by
// any single one — DeleteClusterSpec (lifecycle.go) must never delete it.
// This is also safe on CAPMOX's side, not just PVEKube's: verified live
// (create a cluster, delete it, watch the Secret) and by reading
// internal/controller/proxmoxcluster_controller.go directly —
// reconcileNormalCredentialsSecret adds an owner reference PER referencing
// ProxmoxCluster (EnsureOwnerRef, additive) and its delete counterpart only
// removes CAPMOX's finalizer once len(ownerReferences) <= 1, at which point
// Kubernetes' own garbage collection removes the Secret. A Secret shared by
// two clusters survives either one being deleted; it's only actually
// removed once the last referencing cluster is gone.
func EnsureConnectionCredentialsSecret(dataDir, binDir string, connID int64, url, tokenID, secret string, insecureTLS bool) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		kubectlBin := filepath.Join(binDir, "kubectl")
		kcPath := bootstrap.KubeconfigPath(dataDir)
		name := connectionCredentialsSecretName(connID)
		insecureVal := "false"
		if insecureTLS {
			insecureVal = "true"
		}

		var manifest bytes.Buffer
		createCmd := exec.Command(kubectlBin, "--kubeconfig", kcPath, "create", "secret", "generic", name,
			"-n", "default",
			"--from-literal=url="+url,
			"--from-literal=token="+tokenID,
			"--from-literal=secret="+secret,
			"--from-literal=insecure="+insecureVal,
			"--dry-run=client", "-o", "yaml")
		createCmd.Stdout = &manifest
		if err := createCmd.Run(); err != nil {
			return fmt.Errorf("rendering %s: %w", name, err)
		}

		applyCmd := exec.Command(kubectlBin, "--kubeconfig", kcPath, "apply", "-f", "-")
		applyCmd.Stdin = bytes.NewReader(manifest.Bytes())
		var out bytes.Buffer
		applyCmd.Stdout, applyCmd.Stderr = &out, &out
		if err := applyCmd.Run(); err != nil {
			return fmt.Errorf("applying %s: %w\n%s", name, err, out.String())
		}
		c.Logf("%s Secret is up to date (no controller restart needed — credentialsRef reads it live)", name)
		return nil
	}
}

// ApplyStep applies an already-rendered manifest to the management cluster.
// Piping the exact bytes the operator previewed (rather than re-running
// generate) means what gets applied is provably what was shown on screen.
func ApplyStep(dataDir, binDir, manifestYAML string) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		kubectlBin := filepath.Join(binDir, "kubectl")
		f, err := os.CreateTemp(dataDir, "cluster-apply-*.yaml")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		if _, err := f.WriteString(manifestYAML); err != nil {
			f.Close()
			return err
		}
		f.Close()

		env := []string{"KUBECONFIG=" + bootstrap.KubeconfigPath(dataDir)}
		return runner.Run(c, c, "", env, kubectlBin, "apply", "-f", f.Name())
	}
}

// EnsureCNIStep makes the selected CNI actually get installed onto the
// workload cluster — the one piece clusterctl's generated manifest never
// does on its own for either flavor PVEKube offers.
//
// Calico: CAPMOX's "calico" flavor manifest includes a ClusterResourceSet
// that references a ConfigMap named "calico" in the "default" namespace, but
// never defines that ConfigMap — see CalicoManifestURL's doc comment for the
// full story. Without this, the cluster applies "successfully", every VM
// boots and joins fine, but every node sits at NotReady forever (kubelet:
// "cni plugin not initialized") with nothing hinting why — confirmed by
// hitting exactly this live. The ConfigMap name is fixed (not per-cluster)
// and namespace is always "default", matching CAPMOX's template — so it's a
// shared, idempotent resource: the first cluster that needs it creates it,
// every later calico-flavored cluster reuses it by name.
//
// Cilium: has no equivalent ClusterResourceSet/ConfigMap path — recent
// releases dropped the plain-YAML "quick-install.yaml" install option in
// favor of Helm-only, so this waits for the workload cluster's API to become
// reachable (its kubeconfig Secret only exists once CAPI has that far) and
// then runs `cilium install` via cilium-cli, which drives the Helm install
// internally without needing a separate system Helm binary.
func EnsureCNIStep(dataDir, binDir, clusterName string, cni CNIFlavor) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		switch cni {
		case CNIDefault:
			return nil
		case CNICilium:
			return installCilium(c, dataDir, binDir, clusterName)
		case CNICalico:
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)

			checkCmd := exec.Command(kubectlBin, "--kubeconfig", kcPath, "get", "configmap", "calico", "-n", "default")
			if err := checkCmd.Run(); err == nil {
				c.Logf("ConfigMap calico already exists in the management cluster — reused by every calico-flavored cluster, nothing to do")
				return nil
			}

			c.Logf("Fetching Calico install manifest: %s", versions.CalicoManifestURL)
			req, err := http.NewRequestWithContext(c, http.MethodGet, versions.CalicoManifestURL, nil)
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("fetching Calico manifest: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("fetching Calico manifest: HTTP %d", resp.StatusCode)
			}
			manifest, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("reading Calico manifest: %w", err)
			}

			f, err := os.CreateTemp(dataDir, "calico-manifest-*.yaml")
			if err != nil {
				return err
			}
			defer os.Remove(f.Name())
			if _, err := f.Write(manifest); err != nil {
				f.Close()
				return err
			}
			f.Close()

			if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath, "create", "configmap", "calico",
				"--from-file=calico.yaml="+f.Name(), "-n", "default"); err != nil {
				return fmt.Errorf("creating calico ConfigMap: %w", err)
			}
			c.Logf("ConfigMap calico created — the ClusterResourceSet will apply it to the workload cluster automatically")
			return nil
		default:
			return nil
		}
	}
}

// installCilium waits for the workload cluster's API to become reachable
// (see waitForWorkloadKubeconfig) then runs cilium-cli's install, which
// itself waits for Cilium's own pods to come up before returning.
func installCilium(c *jobs.Ctx, dataDir, binDir, clusterName string) error {
	kcPath, cleanup, err := waitForWorkloadKubeconfig(c, dataDir, binDir, clusterName, "installing Cilium")
	if err != nil {
		return err
	}
	defer cleanup()

	c.Logf("Control plane reachable — installing Cilium via cilium-cli")
	ciliumBin := filepath.Join(binDir, "cilium")
	return runner.Run(c, c, "", nil, ciliumBin, "install", "--kubeconfig", kcPath)
}

// ClusterConnection bundles the Proxmox connection identity and raw
// credentials ApplySpec needs to sync into that connection's dedicated
// credentials Secret (named by connection ID, referenced by the manifest's
// ProxmoxCluster via credentialsRef — see Generate/InjectCredentialsRef).
//
// IsPrimary is informational only (drives the "(primary)" badge in the UI)
// — it does NOT change what ApplySpec does. It used to: an earlier version
// of this function additionally called EnsureCredentialsStep for the
// primary connection, to keep pre-credentialsRef clusters working. That was
// removed after finding, against a real cluster, that it actively breaks
// credentialsRef for EVERY cluster, not just legacy ones: CAPMOX's
// controller builds one shared "global fallback" Proxmox client at startup
// from whatever EnsureCredentialsStep last wrote to
// capmox-manager-credentials, and per CAPMOX's own source
// (cmd/main.go:setupProxmoxClient, pkg/scope/cluster.go:NewClusterScope) —
// that global client takes priority over credentialsRef UNCONDITIONALLY
// whenever it's non-nil, for every ProxmoxCluster regardless of whether it
// has a valid credentialsRef of its own. So the moment any primary-
// connection apply ran EnsureCredentialsStep, EVERY cluster silently
// stopped using its own credentialsRef and started reconciling through
// whatever host the global Secret pointed at instead — the actual cause of
// a real "cannot find node with name X" failure this was diagnosed from,
// on a cluster whose credentialsRef was entirely correct. CAPMOX's
// setupProxmoxClient explicitly documents empty env vars as the supported
// way to force credentialsRef-only routing ("so the proxmoxcontroller can
// create the client later from spec.credentialsRef"), so
// capmox-manager-credentials is now kept empty and this function never
// writes to it — every cluster, primary connection or not, resolves its
// own credentials from its own Secret, which is what "isolated per-
// connection credentials" was supposed to mean in the first place.
type ClusterConnection struct {
	ID          int64
	URL         string
	TokenID     string
	Secret      string
	InsecureTLS bool
	IsPrimary   bool
}

// ApplySpec wraps ApplyStep as a job, for the standard job-engine/SSE-progress
// UI pattern used everywhere else in PVEKube. The connection's own
// credentials Secret is always synced first — before kubectl apply, since
// the manifest's ProxmoxCluster already references it via credentialsRef
// and CAPMOX would otherwise reconcile against a Secret that doesn't exist
// yet. Any selected post-provision addons (metrics-server, Istio, MetalLB)
// are installed last, after CNI, so they land on a cluster that already has
// pod networking.
func ApplySpec(clusterName, dataDir, binDir string, conn ClusterConnection, manifestYAML string, cni CNIFlavor, addons AddonSelection, registry RegistryConfig, oidc OIDCConfig, dns PrivateDNSConfig, internalCA string) *jobs.Spec {
	spec := jobs.NewSpec("cluster.apply", "Apply cluster "+clusterName).
		Step("Sync connection credentials", EnsureConnectionCredentialsSecret(dataDir, binDir, conn.ID, conn.URL, conn.TokenID, conn.Secret, conn.InsecureTLS)).
		Step("kubectl apply", ApplyStep(dataDir, binDir, manifestYAML)).
		Step("Install CNI", EnsureCNIStep(dataDir, binDir, clusterName, cni)).
		Step("Wait for nodes & CNI readiness", WaitForNodesReadyStep(dataDir, binDir, clusterName))
	// Private DNS comes first among the post-provision steps, and the order
	// is load-bearing rather than cosmetic: everything below that talks to
	// an internal host from inside the pod network — Flux against an
	// internal Git server, most obviously — resolves through CoreDNS, and
	// until this runs those lookups are a coin flip between the internal
	// resolver and a public one that answers NXDOMAIN. See dns.go.
	if dns.Enabled() {
		spec.Step("Configure private DNS", ConfigurePrivateDNSStep(dataDir, binDir, clusterName, dns))
	}
	// Likewise before the addons: a workload pulling a Helm chart or Git
	// repo over internal HTTPS needs the CA published before it starts, and
	// pods cannot see the node's trust store. See trust.go.
	if strings.TrimSpace(internalCA) != "" {
		spec.Step("Publish internal CA for workloads", DistributeInternalCAStep(dataDir, binDir, clusterName, internalCA))
	}
	// Registry trust itself is already on the nodes via the manifest; this
	// only adds the pull credentials, which need a reachable API server.
	if registry.HasAuth() {
		spec.Step("Configure registry credentials", InstallRegistryCredentialsStep(dataDir, binDir, clusterName, registry))
	}
	// OIDC auth itself is already live via the manifest; this only grants
	// the default group's RBAC, which needs a reachable API server.
	if group := strings.TrimSpace(oidc.DefaultUsersGroup); group != "" {
		spec.Step("Grant default OIDC group access", InstallOIDCDefaultGroupRBACStep(dataDir, binDir, clusterName, group))
	}
	return AddonSteps(spec, dataDir, binDir, clusterName, addons)
}
