// Package capi drives clusterctl against the KIND management cluster to
// render and apply Cluster API manifests for Proxmox workload clusters.
// Variable names and formats here are taken directly from CAPMOX's own
// cluster-template.yaml and docs/Usage.md (ionos-cloud/cluster-api-provider-proxmox
// v0.9.0) — verified by fetching and reading the real template, not guessed.
package capi

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	return stdout.String(), nil
}

// EnsureCredentialsStep creates or updates the capmox-manager-credentials
// Secret that CAPMOX's controller uses for any ProxmoxCluster that doesn't
// set its own credentialsRef (the default flavor's cluster-template.yaml
// never sets one — verified by reading the real template). That Secret is
// defined INSIDE infrastructure-components.yaml, with its values coming
// from clusterctl init-time env vars — which may run before a Proxmox
// connection even exists in PVEKube, leaving it empty.
//
// Patching the Secret alone is NOT enough to fix a running controller: its
// Deployment wires PROXMOX_URL/TOKEN/SECRET in via secretKeyRef, which
// Kubernetes resolves into the container's environment once at pod start,
// not on every reconcile (confirmed by testing — patching without a
// restart left the controller reporting stale "no credentials" errors
// indefinitely). So this step also rolls the controller Deployment after
// updating the Secret, which is what actually makes it pick up new values.
func EnsureCredentialsStep(dataDir, binDir, proxmoxURL, tokenID, secret string) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		kubectlBin := filepath.Join(binDir, "kubectl")
		kcPath := bootstrap.KubeconfigPath(dataDir)

		// Skip the patch+restart entirely if the Secret already holds these
		// exact values — the common case once a cluster or two has already
		// been applied with the same Proxmox connection. A restart briefly
		// interrupts reconciliation for every cluster on this management
		// cluster, not just the one being applied now, so it's worth
		// avoiding when nothing actually changed.
		if current, err := exec.Command(kubectlBin, "--kubeconfig", kcPath, "get", "secret", "capmox-manager-credentials",
			"-n", "capmox-system", "-o",
			`jsonpath={.data.url}{"\n"}{.data.token}{"\n"}{.data.secret}`).Output(); err == nil {
			want := base64.StdEncoding.EncodeToString([]byte(proxmoxURL)) + "\n" +
				base64.StdEncoding.EncodeToString([]byte(tokenID)) + "\n" +
				base64.StdEncoding.EncodeToString([]byte(secret))
			if strings.TrimSpace(string(current)) == want {
				c.Logf("capmox-manager-credentials already up to date, skipping controller restart")
				return nil
			}
		}

		// create --dry-run=client -o yaml | apply -f - is the standard
		// kubectl idiom for "create or update" without a separate read.
		var manifest bytes.Buffer
		createCmd := exec.Command(kubectlBin, "--kubeconfig", kcPath, "create", "secret", "generic", "capmox-manager-credentials",
			"-n", "capmox-system",
			"--from-literal=url="+proxmoxURL,
			"--from-literal=token="+tokenID,
			"--from-literal=secret="+secret,
			"--dry-run=client", "-o", "yaml")
		createCmd.Stdout = &manifest
		if err := createCmd.Run(); err != nil {
			return fmt.Errorf("rendering credentials secret: %w", err)
		}

		applyCmd := exec.Command(kubectlBin, "--kubeconfig", kcPath, "apply", "-f", "-")
		applyCmd.Stdin = bytes.NewReader(manifest.Bytes())
		var out bytes.Buffer
		applyCmd.Stdout, applyCmd.Stderr = &out, &out
		if err := applyCmd.Run(); err != nil {
			return fmt.Errorf("applying credentials secret: %w\n%s", err, out.String())
		}
		c.Logf("capmox-manager-credentials Secret is up to date")

		if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
			"rollout", "restart", "deployment/capmox-controller-manager", "-n", "capmox-system"); err != nil {
			return fmt.Errorf("restarting capmox controller to pick up credentials: %w", err)
		}
		if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
			"rollout", "status", "deployment/capmox-controller-manager", "-n", "capmox-system", "--timeout=60s"); err != nil {
			return fmt.Errorf("waiting for capmox controller restart: %w", err)
		}
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

// ApplySpec wraps ApplyStep as a job, for the standard job-engine/SSE-progress
// UI pattern used everywhere else in PVEKube. Proxmox credentials are
// re-synced to the management cluster first — see EnsureCredentialsStep.
// Any selected post-provision addons (metrics-server, Istio, MetalLB) are
// installed last, after CNI, so they land on a cluster that already has pod
// networking.
func ApplySpec(clusterName, dataDir, binDir, proxmoxURL, tokenID, secret, manifestYAML string, cni CNIFlavor, addons AddonSelection) *jobs.Spec {
	spec := jobs.NewSpec("cluster.apply", "Apply cluster "+clusterName).
		Step("Sync Proxmox credentials", EnsureCredentialsStep(dataDir, binDir, proxmoxURL, tokenID, secret)).
		Step("kubectl apply", ApplyStep(dataDir, binDir, manifestYAML)).
		Step("Install CNI", EnsureCNIStep(dataDir, binDir, clusterName, cni))
	return AddonSteps(spec, dataDir, binDir, clusterName, addons)
}
