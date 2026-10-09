package capi

import (
	"strings"
	"testing"
)

// kcpDoc / mdDoc carry the infrastructureRefs InjectMachineSizing follows to
// find each role's template.
const sizingRefs = `apiVersion: controlplane.cluster.x-k8s.io/v1beta2
kind: KubeadmControlPlane
metadata:
  name: demo-control-plane
spec:
  machineTemplate:
    spec:
      infrastructureRef:
        kind: ProxmoxMachineTemplate
        name: demo-control-plane
---
apiVersion: cluster.x-k8s.io/v1beta2
kind: MachineDeployment
metadata:
  name: demo-workers
spec:
  template:
    spec:
      infrastructureRef:
        kind: ProxmoxMachineTemplate
        name: demo-worker
---
`

// What the default and calico flavors generate: sizes and a boot volume are
// present on BOTH templates, filled from the one set of clusterctl variables.
const sizedTemplates = `apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
kind: ProxmoxMachineTemplate
metadata:
  name: demo-control-plane
spec:
  template:
    spec:
      sourceNode: pve1
      numSockets: 1
      numCores: 2
      memoryMiB: 4096
      disks:
        bootVolume:
          disk: scsi0
          sizeGb: 100
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
kind: ProxmoxMachineTemplate
metadata:
  name: demo-worker
spec:
  template:
    spec:
      sourceNode: pve1
      numSockets: 1
      numCores: 2
      memoryMiB: 4096
      disks:
        bootVolume:
          disk: scsi0
          sizeGb: 100
`

// What the cilium flavor generates, copied from a real failed apply: neither
// template has any size or disk field. The clusterctl variables are silently
// dropped, so without explicit injection every VM is the source template's
// size — and adding only a worker bootVolume.sizeGb (an earlier version of this
// code) produced a bootVolume with no "disk", which the CRD rejects.
const bareTemplates = `apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
kind: ProxmoxMachineTemplate
metadata:
  name: demo-control-plane
spec:
  template:
    spec:
      full: false
      network:
        networkDevices:
          - bridge: vmbr0
            model: virtio
            name: net0
      sourceNode: pve1
      templateID: 100
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
kind: ProxmoxMachineTemplate
metadata:
  name: demo-worker
spec:
  template:
    spec:
      full: false
      network:
        networkDevices:
          - bridge: vmbr0
            model: virtio
            name: net0
      sourceNode: pve1
      templateID: 100
`

var testCP = MachineSizing{NumSockets: 1, NumCores: 2, MemoryMiB: 4096, BootVolumeSizeGB: 100}

func machineTemplateSpec(t *testing.T, manifest, name string) map[string]any {
	t.Helper()
	for _, d := range decodeDocs(t, manifest) {
		if d["kind"] == "ProxmoxMachineTemplate" && nested(t, d, "metadata", "name") == name {
			return nested(t, d, "spec", "template", "spec").(map[string]any)
		}
	}
	t.Fatalf("no ProxmoxMachineTemplate %q in output", name)
	return nil
}

func bootVolume(t *testing.T, spec map[string]any) map[string]any {
	t.Helper()
	disks, ok := spec["disks"].(map[string]any)
	if !ok {
		t.Fatalf("template has no disks block: %v", spec)
	}
	boot, ok := disks["bootVolume"].(map[string]any)
	if !ok {
		t.Fatalf("template has no bootVolume: %v", disks)
	}
	return boot
}

func assertSize(t *testing.T, who string, spec map[string]any, want MachineSizing, wantDisk string) {
	t.Helper()
	// Integers must stay integers — a quoted "8" is rejected by the CRD.
	for k, w := range map[string]int{"numSockets": want.NumSockets, "numCores": want.NumCores, "memoryMiB": want.MemoryMiB} {
		if got, ok := spec[k].(int); !ok || got != w {
			t.Errorf("%s %s = %v (%T), want int %d", who, k, spec[k], spec[k], w)
		}
	}
	boot := bootVolume(t, spec)
	if got, ok := boot["sizeGb"].(int); !ok || got != want.BootVolumeSizeGB {
		t.Errorf("%s sizeGb = %v, want %d", who, boot["sizeGb"], want.BootVolumeSizeGB)
	}
	// CAPMOX requires bootVolume.disk; a size without it fails the apply.
	if boot["disk"] != wantDisk {
		t.Errorf("%s bootVolume.disk = %v, want %q", who, boot["disk"], wantDisk)
	}
}

func TestInjectMachineSizingWorkersDifferFromControlPlane(t *testing.T) {
	out, err := InjectMachineSizing(sizingRefs+sizedTemplates, testCP, WorkerSizing{NumCores: 8, MemoryMiB: 16384, BootVolumeSizeGB: 200}, "scsi0")
	if err != nil {
		t.Fatal(err)
	}
	assertSize(t, "control plane", machineTemplateSpec(t, out, "demo-control-plane"), testCP, "scsi0")
	// Sockets were not overridden, so the workers inherit the control plane's.
	assertSize(t, "workers", machineTemplateSpec(t, out, "demo-worker"),
		MachineSizing{NumSockets: 1, NumCores: 8, MemoryMiB: 16384, BootVolumeSizeGB: 200}, "scsi0")
	if strings.Count(out, "numCores:") != 2 {
		t.Errorf("expected exactly one numCores per template (no duplicate keys):\n%s", out)
	}
}

// The regression: the cilium flavor's templates carry no sizing at all.
func TestInjectMachineSizingAddsEverythingWhenTheFlavorOmitsIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    WorkerSizing
		want MachineSizing
	}{
		{"no worker override", WorkerSizing{}, testCP},
		{"worker override", WorkerSizing{NumCores: 8, MemoryMiB: 16384}, MachineSizing{NumSockets: 1, NumCores: 8, MemoryMiB: 16384, BootVolumeSizeGB: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := InjectMachineSizing(sizingRefs+bareTemplates, testCP, tc.w, "scsi0")
			if err != nil {
				t.Fatal(err)
			}
			cp := machineTemplateSpec(t, out, "demo-control-plane")
			assertSize(t, "control plane", cp, testCP, "scsi0")
			assertSize(t, "workers", machineTemplateSpec(t, out, "demo-worker"), tc.want, "scsi0")
			// What the flavor did provide must be untouched.
			if cp["sourceNode"] != "pve1" || cp["templateID"] != 100 || cp["full"] != false {
				t.Errorf("unrelated control-plane fields changed: %v", cp)
			}
		})
	}
}

// An existing disk name is the clone's real device; it must not be overwritten.
func TestInjectMachineSizingKeepsExistingBootDisk(t *testing.T) {
	m := strings.ReplaceAll(sizingRefs+sizedTemplates, "disk: scsi0", "disk: virtio0")
	out, err := InjectMachineSizing(m, testCP, WorkerSizing{}, "scsi0")
	if err != nil {
		t.Fatal(err)
	}
	assertSize(t, "control plane", machineTemplateSpec(t, out, "demo-control-plane"), testCP, "virtio0")
}

// KubeadmControlPlane used to carry the reference directly under
// machineTemplate; accept that shape too.
func TestInjectMachineSizingAcceptsOlderControlPlaneShape(t *testing.T) {
	old := strings.Replace(sizingRefs, "  machineTemplate:\n    spec:\n      infrastructureRef:\n        kind: ProxmoxMachineTemplate\n        name: demo-control-plane\n",
		"  machineTemplate:\n    infrastructureRef:\n      kind: ProxmoxMachineTemplate\n      name: demo-control-plane\n", 1)
	if _, err := InjectMachineSizing(old+bareTemplates, testCP, WorkerSizing{}, "scsi0"); err != nil {
		t.Fatal(err)
	}
}

// If a template cannot be identified, say so — applying a manifest whose
// machines silently ignore the requested size is exactly the bug being fixed.
func TestInjectMachineSizingErrorsWhenTemplatesCannotBeFound(t *testing.T) {
	if _, err := InjectMachineSizing(bareTemplates, testCP, WorkerSizing{}, "scsi0"); err == nil {
		t.Fatal("expected an error when there is no KubeadmControlPlane/MachineDeployment")
	}
	missing := sizingRefs + strings.SplitN(bareTemplates, "---\n", 2)[0] // worker template absent
	if _, err := InjectMachineSizing(missing, testCP, WorkerSizing{}, "scsi0"); err == nil {
		t.Fatal("expected an error when the worker ProxmoxMachineTemplate is missing")
	}
}

func TestWorkerSizingOver(t *testing.T) {
	got := WorkerSizing{MemoryMiB: 8192}.Over(testCP)
	if got != (MachineSizing{NumSockets: 1, NumCores: 2, MemoryMiB: 8192, BootVolumeSizeGB: 100}) {
		t.Errorf("partial override = %+v", got)
	}
	if !(WorkerSizing{}).IsZero() || (WorkerSizing{NumCores: 1}).IsZero() {
		t.Error("IsZero wrong")
	}
}
