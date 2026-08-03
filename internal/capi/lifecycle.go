package capi

import (
	"fmt"
	"os"
	"path/filepath"

	"pvekube/internal/bootstrap"
	"pvekube/internal/jobs"
	"pvekube/internal/runner"
)

// ScaleWorkersSpec changes a MachineDeployment's replica count. CAPI's own
// controller does the rest — this is a single patch, not a loop, because
// scaling is exactly that on Cluster API: an intent, not a procedure.
func ScaleWorkersSpec(dataDir, binDir, clusterName string, replicas int) *jobs.Spec {
	return jobs.NewSpec("cluster.scale_workers", fmt.Sprintf("Scale %s workers to %d", clusterName, replicas)).
		Step("kubectl patch machinedeployment", func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
			return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"patch", "machinedeployment", clusterName+"-workers", "--type=merge", "-p", patch)
		})
}

// ScaleControlPlaneSpec changes a KubeadmControlPlane's replica count.
// CAPMOX's own templates only ever use 1 or 3 (see cluster-template.yaml);
// enforcing that here rather than in the UI keeps the constraint in one
// place, next to the resource it actually applies to.
func ScaleControlPlaneSpec(dataDir, binDir, clusterName string, replicas int) *jobs.Spec {
	return jobs.NewSpec("cluster.scale_controlplane", fmt.Sprintf("Scale %s control plane to %d", clusterName, replicas)).
		Step("kubectl patch kubeadmcontrolplane", func(c *jobs.Ctx) error {
			if replicas != 1 && replicas != 3 && replicas != 5 {
				return fmt.Errorf("control plane count must be 1, 3, or 5 for quorum — got %d", replicas)
			}
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
			return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"patch", "kubeadmcontrolplane", clusterName+"-control-plane", "--type=merge", "-p", patch)
		})
}

// DeleteClusterSpec deletes the Cluster object, which CAPI cascades: every
// Machine (and, via CAPMOX, its underlying Proxmox VM) is torn down before
// the Cluster itself is removed. --wait=false so the job finishes once
// deletion is *requested* — actual teardown can take several minutes and is
// tracked by the cluster no longer appearing on refresh, not by this job.
func DeleteClusterSpec(dataDir, binDir, clusterName string) *jobs.Spec {
	return jobs.NewSpec("cluster.delete", "Delete cluster "+clusterName).
		Step("kubectl delete cluster", func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"delete", "cluster", clusterName, "--wait=false")
		})
}

// UpgradeInput describes a version upgrade: PVEKube's templates are
// immutable Proxmox VM clones pinned to one Kubernetes version each (built
// once by the template builder), so "upgrading" a cluster means pointing it
// at a DIFFERENT, already-built template — not an in-place package update.
// This matches how CAPMOX itself models it: ProxmoxMachineTemplate is
// immutable, so a version change means creating a new one and repointing
// the control plane / MachineDeployment at it, which is exactly what
// triggers Cluster API's built-in rolling replacement.
type UpgradeInput struct {
	ClusterName          string
	NewKubernetesVersion string
	NewSourceNode        string
	NewTemplateVMID      int
	Bridge               string
	NumSockets           int
	NumCores             int
	MemoryMiB            int
	BootVolumeDevice     string
	BootVolumeSizeGB     int
}

// newMachineTemplateYAML renders a ProxmoxMachineTemplate manifest. Shape
// verified against the real cluster-template.yaml (ionos-cloud/cluster-api-provider-proxmox
// v0.9.0) — same fields as the ones clusterctl generate produces initially,
// just under a fresh name so the immutable resource can be created anew.
func newMachineTemplateYAML(name, sourceNode string, vmid int, in UpgradeInput) string {
	return fmt.Sprintf(`apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
kind: ProxmoxMachineTemplate
metadata:
  name: %s
spec:
  template:
    spec:
      sourceNode: %s
      templateID: %d
      format: "qcow2"
      full: true
      numSockets: %d
      numCores: %d
      memoryMiB: %d
      disks:
        bootVolume:
          disk: %s
          sizeGb: %d
      network:
        networkDevices:
        - bridge: %s
          model: virtio
          name: net0
`, name, sourceNode, vmid, in.NumSockets, in.NumCores, in.MemoryMiB, in.BootVolumeDevice, in.BootVolumeSizeGB, in.Bridge)
}

// UpgradeSpec creates new ProxmoxMachineTemplates for control plane and
// workers, then patches KubeadmControlPlane and MachineDeployment to
// reference them and the new Kubernetes version — Cluster API's controllers
// take it from there, replacing machines one at a time.
//
// NOTE: verified against CAPMOX's real resource schema (fetched and read,
// not guessed) but — unlike every other feature in PVEKube — not exercised
// against a live rolling upgrade end-to-end; that takes a running cluster
// with two real templates already built, which needs a Linux host this
// session didn't have. Treat this path with a bit more scrutiny than the
// rest before relying on it against a production cluster.
func UpgradeSpec(dataDir, binDir string, in UpgradeInput) *jobs.Spec {
	suffix := fmt.Sprintf("-v%s", sanitizeVersion(in.NewKubernetesVersion))
	cpTemplateName := in.ClusterName + "-control-plane" + suffix
	workerTemplateName := in.ClusterName + "-worker" + suffix

	return jobs.NewSpec("cluster.upgrade", "Upgrade "+in.ClusterName+" to "+in.NewKubernetesVersion).
		Step("Create control-plane machine template", func(c *jobs.Ctx) error {
			return applyYAML(c, dataDir, binDir, newMachineTemplateYAML(cpTemplateName, in.NewSourceNode, in.NewTemplateVMID, in))
		}).
		Step("Create worker machine template", func(c *jobs.Ctx) error {
			return applyYAML(c, dataDir, binDir, newMachineTemplateYAML(workerTemplateName, in.NewSourceNode, in.NewTemplateVMID, in))
		}).
		Step("Patch KubeadmControlPlane", func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			patch := fmt.Sprintf(`{"spec":{"version":%q,"machineTemplate":{"spec":{"infrastructureRef":{"name":%q}}}}}`,
				in.NewKubernetesVersion, cpTemplateName)
			return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"patch", "kubeadmcontrolplane", in.ClusterName+"-control-plane", "--type=merge", "-p", patch)
		}).
		Step("Patch MachineDeployment", func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"version":%q,"infrastructureRef":{"name":%q}}}}}`,
				in.NewKubernetesVersion, workerTemplateName)
			return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"patch", "machinedeployment", in.ClusterName+"-workers", "--type=merge", "-p", patch)
		})
}

func applyYAML(c *jobs.Ctx, dataDir, binDir, yamlContent string) error {
	kubectlBin := filepath.Join(binDir, "kubectl")
	kcPath := bootstrap.KubeconfigPath(dataDir)
	f, err := writeTempYAML(dataDir, yamlContent)
	if err != nil {
		return err
	}
	defer removeFile(f)
	return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath, "apply", "-f", f)
}

func writeTempYAML(dataDir, content string) (string, error) {
	f, err := os.CreateTemp(dataDir, "lifecycle-*.yaml")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func removeFile(path string) { os.Remove(path) }

func sanitizeVersion(v string) string {
	out := make([]byte, 0, len(v))
	for i := 0; i < len(v); i++ {
		ch := v[i]
		if ch == '.' {
			out = append(out, '-')
		} else if ch != 'v' {
			out = append(out, ch)
		}
	}
	return string(out)
}
