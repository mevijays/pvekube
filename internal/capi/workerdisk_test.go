package capi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"pvekube/internal/proxmox"
)

func pm(name, cluster string, labels map[string]any, vmid int, node string, deleting bool) obj {
	l := obj{"cluster.x-k8s.io/cluster-name": cluster}
	for k, v := range labels {
		l[k] = v
	}
	meta := obj{"name": name, "labels": l}
	if deleting {
		meta["deletionTimestamp"] = "2026-11-22T10:00:00Z"
	}
	st := obj{}
	if node != "" {
		st["proxmoxNode"] = node
	}
	spec := obj{}
	if vmid > 0 {
		spec["virtualMachineID"] = float64(vmid)
	}
	return obj{"metadata": meta, "spec": spec, "status": st}
}

func TestParseWorkerVMsNeverIncludesControlPlane(t *testing.T) {
	worker := map[string]any{labelDeployment: "dev-workers"}
	cp := map[string]any{labelControlPlane: "", "cluster.x-k8s.io/control-plane-name": "dev-control-plane"}
	list := obj{"items": []any{
		pm("dev-control-plane-a", "dev", cp, 107, "pve1", false),
		pm("dev-workers-b", "dev", worker, 108, "pve1", false),
		pm("dev-workers-a", "dev", worker, 109, "pve2", false),
		pm("other-workers-a", "other", worker, 110, "pve1", false),
		pm("dev-workers-gone", "dev", worker, 111, "pve1", true),
		pm("dev-workers-new", "dev", worker, 0, "", false),
		// A machine somehow carrying BOTH labels must still be excluded.
		pm("dev-weird", "dev", map[string]any{labelDeployment: "x", labelControlPlane: ""}, 112, "pve1", false),
		pm("dev-orphan", "dev", nil, 113, "pve1", false),
	}}
	vms, skipped := parseWorkerVMs("dev", list)
	if len(vms) != 2 || vms[0].Machine != "dev-workers-a" || vms[0].VMID != 109 || vms[0].Node != "pve2" || vms[1].Machine != "dev-workers-b" {
		t.Fatalf("workers = %+v", vms)
	}
	for _, v := range vms {
		if v.VMID == 107 || v.VMID == 112 {
			t.Fatalf("control-plane VM %d selected", v.VMID)
		}
	}
	joined := strings.Join(skipped, "|")
	for _, want := range []string{"dev-workers-gone: being deleted", "dev-workers-new: its VM is not created yet", "dev-orphan: not owned"} {
		if !strings.Contains(joined, want) {
			t.Errorf("skipped lacks %q: %s", want, joined)
		}
	}
}

type fakeEnsurer struct {
	calls []string
	fail  map[int]error
	res   proxmox.DiskResult
}

func (f *fakeEnsurer) EnsureExtraDisk(_ context.Context, node string, vmid int, storage string, size int) (proxmox.DiskResult, error) {
	f.calls = append(f.calls, fmt.Sprintf("%s/%d/%s/%d", node, vmid, storage, size))
	if err := f.fail[vmid]; err != nil {
		return proxmox.DiskResult{}, err
	}
	r := f.res
	if r.Existing == "" && !r.Added {
		r.Added = true
	}
	return r, nil
}

func TestEnsureWorkerDisksContinuesPastFailure(t *testing.T) {
	vms := []WorkerVM{{"w1", "pve1", 101}, {"w2", "pve1", 102}, {"w3", "pve2", 103}}
	f := &fakeEnsurer{fail: map[int]error{102: errors.New("boom")}}
	var logs []string
	logf := func(format string, a ...any) { logs = append(logs, fmt.Sprintf(format, a...)) }
	err := ensureWorkerDisks(context.Background(), logf, vms, f, "local-lvm", 100)
	if err == nil || !strings.Contains(err.Error(), "w2") || strings.Contains(err.Error(), "w1") {
		t.Fatalf("error should name only the failed worker, got %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("every worker must be attempted, calls=%v", f.calls)
	}
	if f.calls[2] != "pve2/103/local-lvm/100" {
		t.Fatalf("node/vmid/storage/size not passed through: %v", f.calls)
	}
}

func TestEnsureWorkerDisksRejectsInvalidBeforeAnyCall(t *testing.T) {
	f := &fakeEnsurer{}
	if err := ensureWorkerDisks(context.Background(), func(string, ...any) {}, []WorkerVM{{"w", "n", 1}}, f, "a,b", 10); err == nil {
		t.Fatal("expected validation error")
	}
	if len(f.calls) != 0 {
		t.Fatal("no Proxmox call may happen for invalid input")
	}
}

func TestEnsureWorkerDisksStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeEnsurer{}
	if err := ensureWorkerDisks(ctx, func(string, ...any) {}, []WorkerVM{{"w", "n", 1}}, f, "local", 10); err == nil {
		t.Fatal("expected cancellation error")
	}
	if len(f.calls) != 0 {
		t.Fatal("cancelled run must not call Proxmox")
	}
}
