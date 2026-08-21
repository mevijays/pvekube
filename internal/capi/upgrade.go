// Kubernetes version upgrades for an existing workload cluster.
//
// PVEKube's templates are immutable Proxmox VM clones with kubeadm/kubelet
// baked in at build time (see internal/imagebuilder), so "upgrading" a
// cluster never means an in-place package update on a running node. It
// means pointing the cluster at a DIFFERENT, already-built template and
// letting Cluster API roll every machine. That's also exactly how CAPI
// models it: ProxmoxMachineTemplate is immutable, so a version change
// requires a NEW template object, and changing the reference is what
// triggers the rolling replacement.
//
// The new machine templates are CLONED from the ones the cluster is
// currently using — not rebuilt from a fixed set of fields. That matters:
// a cluster's live machine template carries settings PVEKube injected at
// creation time and would otherwise silently lose here, including
// checks.skipCloudInitStatus (mandatory for Flatcar/Ignition clusters —
// see ignition.go, without it every new machine hangs forever at
// WaitingForCloudInit) and its real disk/CPU/memory sizing. Cloning
// preserves all of it, plus any CAPMOX field added in a future release
// that this code has never heard of; only sourceNode and templateID are
// overridden.
package capi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pvekube/internal/bootstrap"
	"pvekube/internal/jobs"
	"pvekube/internal/runner"
)

// UpgradeInput is everything a version upgrade needs. Deliberately small:
// machine sizing/network/disk are NOT here because they're cloned from the
// cluster's existing machine template rather than re-supplied (see the
// package doc comment).
type UpgradeInput struct {
	ClusterName     string
	CurrentVersion  string // e.g. "v1.36.1", read from the live KubeadmControlPlane
	NewVersion      string // e.g. "v1.37.0", from the selected template
	NewSourceNode   string // Proxmox node the new template lives on
	NewTemplateVMID int    // the new template's VMID
}

type semver struct{ major, minor, patch int }

func (s semver) String() string { return fmt.Sprintf("v%d.%d.%d", s.major, s.minor, s.patch) }

// parseSemver accepts "v1.36.1" or "1.36.1". Anything with a pre-release or
// build suffix ("v1.37.0-rc.1") keeps only the numeric core, which is all
// the skew rules below care about.
func parseSemver(v string) (semver, error) {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return semver{}, fmt.Errorf("%q is not a Kubernetes version like v1.36.1", v)
	}
	var out semver
	if _, err := fmt.Sscanf(parts[0], "%d", &out.major); err != nil {
		return semver{}, fmt.Errorf("%q is not a Kubernetes version like v1.36.1", v)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &out.minor); err != nil {
		return semver{}, fmt.Errorf("%q is not a Kubernetes version like v1.36.1", v)
	}
	if len(parts) > 2 {
		fmt.Sscanf(parts[2], "%d", &out.patch)
	}
	return out, nil
}

// ValidateUpgrade enforces kubeadm's own version skew rules BEFORE anything
// is patched. These are hard errors rather than warnings on purpose: CAPI
// applies a version change by rolling machines one at a time, so an
// unsupported jump doesn't fail cleanly up front — it fails partway through,
// leaving the cluster split across two versions with a control plane that
// may refuse to finish. Catching it here keeps a bad selection from ever
// reaching the cluster.
func ValidateUpgrade(current, target string) error {
	cur, err := parseSemver(current)
	if err != nil {
		return fmt.Errorf("current cluster version: %w", err)
	}
	tgt, err := parseSemver(target)
	if err != nil {
		return fmt.Errorf("target version: %w", err)
	}

	if tgt == cur {
		return fmt.Errorf("cluster is already running %s — pick a template with a different Kubernetes version", current)
	}
	if tgt.major < cur.major || (tgt.major == cur.major && tgt.minor < cur.minor) ||
		(tgt.major == cur.major && tgt.minor == cur.minor && tgt.patch < cur.patch) {
		return fmt.Errorf("downgrade from %s to %s is not supported — kubeadm cannot roll a cluster backwards, and etcd/API server data written by the newer version may not be readable by the older one", current, target)
	}
	if tgt.major != cur.major {
		return fmt.Errorf("upgrade from %s to %s crosses a major version — kubeadm only supports one minor version at a time", current, target)
	}
	if tgt.minor > cur.minor+1 {
		return fmt.Errorf("upgrade from %s to %s skips %d minor version(s) — kubeadm only supports one minor at a time, so go via v%d.%d first",
			current, target, tgt.minor-cur.minor-1, cur.major, cur.minor+1)
	}
	return nil
}

// upgradeTemplateName is the deterministic name for the machine template a
// given (cluster, role, version, vmid) upgrade produces.
//
// Built from the CLUSTER name rather than by suffixing the template being
// replaced — suffixing would compound on every successive upgrade
// ("...-1-37-0-120-1-38-0-125"). Including the VMID alongside the version
// keeps a rebuilt template at the SAME Kubernetes version from colliding
// with the earlier one: ProxmoxMachineTemplate is immutable, so re-applying
// one name with a different templateID is rejected outright.
func upgradeTemplateName(clusterName, role, version string, vmid int) string {
	return fmt.Sprintf("%s-%s-%s-%d", clusterName, role, sanitizeVersion(version), vmid)
}

// kubectlOut runs kubectl and returns stdout, folding stderr into the error.
// exec.Cmd.Output() alone reduces a failure to "exit status 1", which turns
// an actionable message ("kubeadmcontrolplanes ... not found") into a dead
// end for whoever reads the job log.
func kubectlOut(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, msg)
	}
	return stdout.String(), nil
}

// CurrentVersion reads the Kubernetes version the cluster's control plane is
// currently declared at (KubeadmControlPlane.spec.version).
func CurrentVersion(ctx context.Context, dataDir, binDir, clusterName string) (string, error) {
	kubectlBin := filepath.Join(binDir, "kubectl")
	out, err := kubectlOut(ctx, kubectlBin, "--kubeconfig", bootstrap.KubeconfigPath(dataDir),
		"get", "kubeadmcontrolplane", clusterName+"-control-plane",
		"-o", "jsonpath={.spec.version}")
	if err != nil {
		return "", fmt.Errorf("reading current version of %s: %w", clusterName, err)
	}
	v := strings.TrimSpace(out)
	if v == "" {
		return "", fmt.Errorf("cluster %s has no control plane version yet — it may still be provisioning", clusterName)
	}
	return v, nil
}

// machineTemplateRef reads the name of the ProxmoxMachineTemplate a
// KubeadmControlPlane or MachineDeployment currently points at. Reading it
// live (rather than assuming "<cluster>-control-plane") is what makes
// repeated upgrades work: after the first one the reference no longer
// matches the original clusterctl-generated name.
func machineTemplateRef(ctx context.Context, kubectlBin, kcPath, kind, name, jsonPath string) (string, error) {
	out, err := kubectlOut(ctx, kubectlBin, "--kubeconfig", kcPath,
		"get", kind, name, "-o", "jsonpath="+jsonPath)
	if err != nil {
		return "", fmt.Errorf("reading %s/%s's machine template reference: %w", kind, name, err)
	}
	ref := strings.TrimSpace(out)
	if ref == "" {
		return "", fmt.Errorf("%s/%s has no machine template reference", kind, name)
	}
	return ref, nil
}

// cloneMachineTemplate copies an existing ProxmoxMachineTemplate under a new
// name, overriding only the two fields a version change actually requires.
// apiVersion/kind are carried over from the source object rather than
// hardcoded, so a CAPMOX API bump doesn't silently produce an object the
// cluster's CRDs reject.
func cloneMachineTemplate(c *jobs.Ctx, kubectlBin, kcPath, srcName, newName, sourceNode string, vmid int) error {
	raw, err := kubectlOut(c, kubectlBin, "--kubeconfig", kcPath,
		"get", "proxmoxmachinetemplate", srcName, "-o", "json")
	if err != nil {
		return fmt.Errorf("reading machine template %s: %w", srcName, err)
	}

	var src map[string]any
	if err := json.Unmarshal([]byte(raw), &src); err != nil {
		return fmt.Errorf("parsing machine template %s: %w", srcName, err)
	}

	spec, ok := nestedMap(src, "spec", "template", "spec")
	if !ok {
		return fmt.Errorf("machine template %s has no spec.template.spec", srcName)
	}
	spec["sourceNode"] = sourceNode
	spec["templateID"] = vmid

	namespace, _ := nestedMap(src, "metadata")
	ns, _ := namespace["namespace"].(string)
	if ns == "" {
		ns = "default"
	}

	obj := map[string]any{
		"apiVersion": src["apiVersion"],
		"kind":       src["kind"],
		"metadata": map[string]any{
			"name":      newName,
			"namespace": ns,
		},
		"spec": map[string]any{
			"template": map[string]any{"spec": spec},
		},
	}
	body, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("encoding new machine template %s: %w", newName, err)
	}

	applyCmd := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath, "apply", "-f", "-")
	applyCmd.Stdin = bytes.NewReader(body)
	var out bytes.Buffer
	applyCmd.Stdout, applyCmd.Stderr = &out, &out
	if err := applyCmd.Run(); err != nil {
		return fmt.Errorf("creating machine template %s: %w\n%s", newName, err, out.String())
	}
	c.Logf("Machine template %s created (cloned from %s, now templateID %d on node %s)", newName, srcName, vmid, sourceNode)
	return nil
}

// nestedMap walks a decoded JSON object down a key path.
func nestedMap(m map[string]any, keys ...string) (map[string]any, bool) {
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// UpgradeSpec performs the whole upgrade as one job: clone both machine
// templates at the new template's VMID, repoint the KubeadmControlPlane and
// MachineDeployment at them with the new version, then watch Cluster API
// roll the machines.
//
// Control plane first, then workers, is deliberate and matches kubeadm's own
// rule that the control plane must never trail the nodes: kubelet may be up
// to two minors BEHIND the API server but never ahead of it. CAPI reconciles
// both patches concurrently once applied, but sequencing the patches this
// way keeps the control plane's rollout starting first.
func UpgradeSpec(dataDir, binDir string, in UpgradeInput) *jobs.Spec {
	kcpName := in.ClusterName + "-control-plane"
	mdName := in.ClusterName + "-workers"
	cpNewName := upgradeTemplateName(in.ClusterName, "control-plane", in.NewVersion, in.NewTemplateVMID)
	workerNewName := upgradeTemplateName(in.ClusterName, "worker", in.NewVersion, in.NewTemplateVMID)

	return jobs.NewSpec("cluster.upgrade", fmt.Sprintf("Upgrade %s: %s to %s", in.ClusterName, in.CurrentVersion, in.NewVersion)).
		Step("Clone machine templates at the new version", func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)

			cpSrc, err := machineTemplateRef(c, kubectlBin, kcPath, "kubeadmcontrolplane", kcpName,
				"{.spec.machineTemplate.spec.infrastructureRef.name}")
			if err != nil {
				return err
			}
			mdSrc, err := machineTemplateRef(c, kubectlBin, kcPath, "machinedeployment", mdName,
				"{.spec.template.spec.infrastructureRef.name}")
			if err != nil {
				return err
			}
			c.Logf("Cloning from the templates this cluster uses today: %s (control plane), %s (workers)", cpSrc, mdSrc)

			if err := cloneMachineTemplate(c, kubectlBin, kcPath, cpSrc, cpNewName, in.NewSourceNode, in.NewTemplateVMID); err != nil {
				return err
			}
			return cloneMachineTemplate(c, kubectlBin, kcPath, mdSrc, workerNewName, in.NewSourceNode, in.NewTemplateVMID)
		}).
		Step("Patch control plane to "+in.NewVersion, func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			patch := fmt.Sprintf(`{"spec":{"version":%q,"machineTemplate":{"spec":{"infrastructureRef":{"name":%q}}}}}`,
				in.NewVersion, cpNewName)
			if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"patch", "kubeadmcontrolplane", kcpName, "--type=merge", "-p", patch); err != nil {
				return err
			}
			c.Logf("Cluster API will now replace control plane machine(s) one at a time — each is a fresh Proxmox VM.")
			return nil
		}).
		Step("Patch workers to "+in.NewVersion, func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"version":%q,"infrastructureRef":{"name":%q}}}}}`,
				in.NewVersion, workerNewName)
			return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"patch", "machinedeployment", mdName, "--type=merge", "-p", patch)
		}).
		Step("Wait for the rolling upgrade to finish", waitForUpgradeStep(dataDir, binDir, in))
}

// waitForUpgradeStep polls until every control plane and worker replica is
// both up-to-date (running the new machine template) and ready.
//
// upToDateReplicas — not readyReplicas alone — is what actually says the
// rollout finished: a cluster mid-upgrade regularly reports every replica
// ready while half of them are still the OLD version, because CAPI only
// removes an old machine once its replacement is healthy.
func waitForUpgradeStep(dataDir, binDir string, in UpgradeInput) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		kubectlBin := filepath.Join(binDir, "kubectl")
		kcPath := bootstrap.KubeconfigPath(dataDir)
		kcpName := in.ClusterName + "-control-plane"
		mdName := in.ClusterName + "-workers"

		c.Logf("Every node is replaced, not updated in place: expect several minutes per machine.")

		// 90 minutes: each machine is a full clone+boot+join cycle, and a
		// 3-control-plane + several-worker cluster serialises a lot of them.
		deadline := time.Now().Add(90 * time.Minute)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()

		for {
			cpWant, e1 := replicaCount(c, kubectlBin, kcPath, "kubeadmcontrolplane", kcpName, "{.spec.replicas}")
			cpUpToDate, e2 := replicaCount(c, kubectlBin, kcPath, "kubeadmcontrolplane", kcpName, "{.status.upToDateReplicas}")
			cpReady, e3 := replicaCount(c, kubectlBin, kcPath, "kubeadmcontrolplane", kcpName, "{.status.readyReplicas}")
			mdWant, e4 := replicaCount(c, kubectlBin, kcPath, "machinedeployment", mdName, "{.spec.replicas}")
			mdUpToDate, e5 := replicaCount(c, kubectlBin, kcPath, "machinedeployment", mdName, "{.status.upToDateReplicas}")
			mdReady, e6 := replicaCount(c, kubectlBin, kcPath, "machinedeployment", mdName, "{.status.readyReplicas}")

			readable := e1 == nil && e2 == nil && e3 == nil && e4 == nil && e5 == nil && e6 == nil
			if readable {
				done := cpUpToDate == cpWant && cpReady == cpWant && mdUpToDate == mdWant && mdReady == mdWant
				if done {
					c.Logf("✓ Upgrade complete — control plane %d/%d and workers %d/%d are on %s and ready",
						cpUpToDate, cpWant, mdUpToDate, mdWant, in.NewVersion)
					return nil
				}
				c.Logf("Progress: control plane %d/%d up-to-date (%d ready) · workers %d/%d up-to-date (%d ready)",
					cpUpToDate, cpWant, cpReady, mdUpToDate, mdWant, mdReady)
				logMachinePhases(c, kubectlBin, kcPath, in.ClusterName)
			} else {
				c.Logf("Waiting for upgrade status to become readable...")
			}

			if time.Now().After(deadline) {
				return fmt.Errorf("cluster %s was still rolling to %s after 90 minutes — the upgrade is not rolled back, it is simply unfinished; check `kubectl describe kubeadmcontrolplane %s` and the CAPMOX controller logs on the management cluster",
					in.ClusterName, in.NewVersion, kcpName)
			}

			select {
			case <-c.Done():
				return c.Err()
			case <-ticker.C:
			}
		}
	}
}
