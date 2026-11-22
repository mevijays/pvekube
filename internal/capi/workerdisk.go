// Extra data disks for worker nodes.
//
// Cluster API's Proxmox provider (CAPMOX) can only describe a machine's boot
// volume, so a Ceph/Rook OSD disk cannot be declared in the cluster spec. PVEKube
// attaches it through the Proxmox API instead, to the Proxmox VM behind each
// worker. Because every Upgrade/Resize replaces the VMs (and a node-local disk
// dies with its VM), the disk is stored as a per-cluster policy and re-applied
// at the end of those operations.
//
// Control-plane machines are never touched: workers are identified as the
// ProxmoxMachines owned by the workers MachineDeployment and NOT labelled as
// control plane, and both conditions are required.
package capi

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"pvekube/internal/bootstrap"
	"pvekube/internal/jobs"
	"pvekube/internal/proxmox"
)

const (
	labelDeployment   = "cluster.x-k8s.io/deployment-name"
	labelControlPlane = "cluster.x-k8s.io/control-plane"
)

// WorkerVM is the Proxmox VM behind one worker machine.
type WorkerVM struct {
	Machine string
	Node    string
	VMID    int
}

// parseWorkerVMs extracts worker VMs from a decoded `kubectl get
// proxmoxmachines -o json` list. Anything that is a control-plane machine,
// not owned by a MachineDeployment, being deleted, or not yet placed on a
// Proxmox node is left out; the second return value says why, per machine.
func parseWorkerVMs(clusterName string, list map[string]any) (vms []WorkerVM, skipped []string) {
	items, _ := list["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		meta, _ := m["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		labels, _ := meta["labels"].(map[string]any)

		if c, _ := labels["cluster.x-k8s.io/cluster-name"].(string); c != clusterName {
			continue
		}
		if _, isCP := labels[labelControlPlane]; isCP {
			continue // never a worker, and not worth a "skipped" line
		}
		if d, _ := labels[labelDeployment].(string); d == "" {
			skipped = append(skipped, name+": not owned by the workers MachineDeployment")
			continue
		}
		if ts, _ := meta["deletionTimestamp"].(string); ts != "" {
			skipped = append(skipped, name+": being deleted")
			continue
		}
		vmid := intField(m, 0, "spec", "virtualMachineID")
		st, _ := m["status"].(map[string]any)
		node, _ := st["proxmoxNode"].(string)
		if vmid <= 0 || node == "" {
			skipped = append(skipped, name+": its VM is not created yet")
			continue
		}
		vms = append(vms, WorkerVM{Machine: name, Node: node, VMID: vmid})
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].Machine < vms[j].Machine })
	return vms, skipped
}

// WorkerVMs lists the Proxmox VMs behind a cluster's worker machines.
func WorkerVMs(ctx context.Context, dataDir, binDir, clusterName string) ([]WorkerVM, []string, error) {
	list, err := getJSON(ctx, filepath.Join(binDir, "kubectl"), bootstrap.KubeconfigPath(dataDir),
		"proxmoxmachines", "-l", "cluster.x-k8s.io/cluster-name="+clusterName)
	if err != nil {
		return nil, nil, fmt.Errorf("listing worker VMs of %s: %w", clusterName, err)
	}
	vms, skipped := parseWorkerVMs(clusterName, list)
	return vms, skipped, nil
}

// DiskEnsurer is the part of the Proxmox client the disk step needs.
type DiskEnsurer interface {
	EnsureExtraDisk(ctx context.Context, node string, vmid int, storage string, sizeGiB int) (proxmox.DiskResult, error)
}

// ensureWorkerDisks applies the disk to every VM, carrying on past a failure so
// one broken node does not leave the rest without a disk, and reports all
// failures together.
func ensureWorkerDisks(ctx context.Context, logf func(string, ...any), vms []WorkerVM, pve DiskEnsurer, storage string, sizeGiB int) error {
	if err := proxmox.ValidateExtraDisk(storage, sizeGiB); err != nil {
		return err
	}
	var failed []string
	for _, vm := range vms {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := pve.EnsureExtraDisk(ctx, vm.Node, vm.VMID, storage, sizeGiB)
		switch {
		case err != nil:
			logf("✗ %s (VM %d on %s): %v", vm.Machine, vm.VMID, vm.Node, err)
			failed = append(failed, vm.Machine)
		case !res.Added:
			logf("• %s (VM %d): already has %s = %s — left unchanged", vm.Machine, vm.VMID, proxmox.ExtraDiskSlot, res.Existing)
		case res.Pending:
			logf("✓ %s (VM %d): added %d GiB on %s as %s, but it could not be hot-plugged — it appears after the VM restarts", vm.Machine, vm.VMID, sizeGiB, storage, proxmox.ExtraDiskSlot)
		default:
			logf("✓ %s (VM %d): added %d GiB on %s as %s (/dev/sdb in the guest)", vm.Machine, vm.VMID, sizeGiB, storage, proxmox.ExtraDiskSlot)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not add the disk to %d worker(s): %s", len(failed), strings.Join(failed, ", "))
	}
	return nil
}

func workerDiskStep(dataDir, binDir, clusterName string, pve DiskEnsurer, storage string, sizeGiB int, retryHint string) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		vms, skipped, err := WorkerVMs(c, dataDir, binDir, clusterName)
		if err != nil {
			return err
		}
		for _, s := range skipped {
			c.Logf("skipping %s", s)
		}
		if len(vms) == 0 {
			c.Logf("No worker VMs to add a disk to.")
			return nil
		}
		c.Logf("Adding a %d GiB disk on storage %q to %d worker VM(s). Control-plane nodes are not touched.", sizeGiB, storage, len(vms))
		if err := ensureWorkerDisks(c, c.Logf, vms, pve, storage, sizeGiB); err != nil {
			return fmt.Errorf("%w%s", err, retryHint)
		}
		return nil
	}
}

// WorkerDiskSpec is the job behind the "Add data disk to workers" action.
func WorkerDiskSpec(dataDir, binDir, clusterName string, pve DiskEnsurer, storage string, sizeGiB int) *jobs.Spec {
	return jobs.NewSpec("cluster.worker_disk", fmt.Sprintf("Add %d GiB data disk to %s workers", sizeGiB, clusterName)).
		Step("Attach disk to worker VMs", workerDiskStep(dataDir, binDir, clusterName, pve, storage, sizeGiB, ""))
}

// AppendWorkerDiskStep adds the disk step to the end of an upgrade, resize or
// scale job, so replacement workers get the cluster's disk policy applied. A
// failure here does not undo the rollout, and the message says so.
func AppendWorkerDiskStep(spec *jobs.Spec, dataDir, binDir, clusterName string, pve DiskEnsurer, storage string, sizeGiB int) *jobs.Spec {
	return spec.Step("Attach data disk to new workers",
		workerDiskStep(dataDir, binDir, clusterName, pve, storage, sizeGiB,
			" — the operation itself completed; use \"Worker data disk\" on the cluster page to retry the disks"))
}
