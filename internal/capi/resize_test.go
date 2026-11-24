package capi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// machineTemplate decodes a ProxmoxMachineTemplate the way kubectl -o json
// returns it.
func machineTemplate(t *testing.T, doc string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A live worker template as PVEKube creates it: sizes, a boot volume, the
// template VM, and the Flatcar cloud-init check — all of which a resize must
// carry over untouched except the size actually requested.
const liveWorker = `{
  "apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha2",
  "kind": "ProxmoxMachineTemplate",
  "metadata": {"name": "prodk8s-worker", "namespace": "default", "uid": "abc", "resourceVersion": "9", "ownerReferences": [{"name": "x"}]},
  "spec": {"template": {"spec": {
    "sourceNode": "pve2", "templateID": 100, "numSockets": 1, "numCores": 8, "memoryMiB": 16384,
    "disks": {"bootVolume": {"disk": "scsi0", "sizeGb": 100}},
    "checks": {"skipCloudInitStatus": true},
    "network": {"default": {"bridge": "vmbr0", "model": "virtio"}}
  }}}
}`

func TestResizedTemplateChangesOnlyTheRequestedSize(t *testing.T) {
	src := machineTemplate(t, liveWorker)
	name, obj, changed, err := resizedTemplate(src, "prodk8s", "worker", ResizeRequest{MemoryMiB: 25600})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	spec := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if got := jsonInt(spec["memoryMiB"]); got != 25600 {
		t.Fatalf("memoryMiB = %d", got)
	}
	// Everything else must be exactly as it was.
	if jsonInt(spec["numCores"]) != 8 || jsonInt(spec["numSockets"]) != 1 || jsonInt(spec["templateID"]) != 100 || spec["sourceNode"] != "pve2" {
		t.Fatalf("size/template fields drifted: %v", spec)
	}
	for _, key := range []string{"disks", "checks", "network"} {
		if !reflect.DeepEqual(spec[key], src["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)[key]) {
			t.Fatalf("%s was not preserved: %v", key, spec[key])
		}
	}
	// The new object is a clean one: no server-set metadata copied across.
	meta := obj["metadata"].(map[string]any)
	if meta["name"] != name || meta["namespace"] != "default" || len(meta) != 2 {
		t.Fatalf("metadata = %v", meta)
	}
	if obj["apiVersion"] != "infrastructure.cluster.x-k8s.io/v1alpha2" || obj["kind"] != "ProxmoxMachineTemplate" {
		t.Fatalf("apiVersion/kind not carried over: %v %v", obj["apiVersion"], obj["kind"])
	}
}

func TestResizedTemplateAppliesCoresAndSockets(t *testing.T) {
	src := machineTemplate(t, liveWorker)
	_, obj, changed, err := resizedTemplate(src, "c", "worker", ResizeRequest{NumCores: 4, NumSockets: 2})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	spec := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if jsonInt(spec["numCores"]) != 4 || jsonInt(spec["numSockets"]) != 2 {
		t.Fatalf("cores/sockets = %v/%v", spec["numCores"], spec["numSockets"])
	}
	if jsonInt(spec["memoryMiB"]) != 16384 {
		t.Fatalf("RAM was changed although only CPU was requested: %v", spec["memoryMiB"])
	}
}

func TestResizedTemplateDoesNotMutateItsSource(t *testing.T) {
	src := machineTemplate(t, liveWorker)
	before, _ := json.Marshal(src)
	if _, _, _, err := resizedTemplate(src, "c", "worker", ResizeRequest{MemoryMiB: 4096, NumCores: 2, NumSockets: 2}); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(src)
	if string(before) != string(after) {
		t.Fatal("resizedTemplate modified the live template it was given")
	}
}

func TestResizedTemplateNoOpWhenSizeAlreadyMatches(t *testing.T) {
	src := machineTemplate(t, liveWorker)
	for _, req := range []ResizeRequest{
		{MemoryMiB: 16384}, // same RAM
		{NumCores: 8, NumSockets: 1, MemoryMiB: 16384}, // everything the same
		{}, // nothing asked
	} {
		if name, obj, changed, err := resizedTemplate(src, "c", "worker", req); err != nil || changed || obj != nil || name != "" {
			t.Fatalf("req %+v: name=%q changed=%v err=%v", req, name, changed, err)
		}
	}
}

func TestResizedTemplateNameIsDeterministicAndDistinct(t *testing.T) {
	src := machineTemplate(t, liveWorker)
	a1, _, _, _ := resizedTemplate(src, "prodk8s", "worker", ResizeRequest{MemoryMiB: 25600})
	a2, _, _, _ := resizedTemplate(src, "prodk8s", "worker", ResizeRequest{MemoryMiB: 25600})
	b, _, _, _ := resizedTemplate(src, "prodk8s", "worker", ResizeRequest{MemoryMiB: 32768})
	c, _, _, _ := resizedTemplate(src, "prodk8s", "worker", ResizeRequest{MemoryMiB: 25600, NumCores: 4})
	if a1 != a2 {
		t.Fatalf("same size must give the same name (idempotent apply): %q vs %q", a1, a2)
	}
	if a1 == b || a1 == c || b == c {
		t.Fatalf("different specs must never share a name (immutable templates): %q %q %q", a1, b, c)
	}
	if !strings.HasPrefix(a1, "prodk8s-worker-rs-") || len(a1) > 63 {
		t.Fatalf("name %q is not DNS-label shaped", a1)
	}
}

// A different source (say after an upgrade moved templateID) with the same
// requested size is a different object and must get a different name.
func TestResizedTemplateNameDependsOnTheWholeSpec(t *testing.T) {
	src := machineTemplate(t, liveWorker)
	other := machineTemplate(t, strings.Replace(liveWorker, `"templateID": 100`, `"templateID": 107`, 1))
	a, _, _, _ := resizedTemplate(src, "c", "worker", ResizeRequest{MemoryMiB: 25600})
	b, _, _, _ := resizedTemplate(other, "c", "worker", ResizeRequest{MemoryMiB: 25600})
	if a == b {
		t.Fatalf("a template on a different Proxmox template VM shares name %q", a)
	}
}

// The cilium flavor's machine templates carry no sizing at all: setting RAM
// must add just that field, and any explicit value counts as a change.
func TestResizedTemplateOnTemplateWithoutSizing(t *testing.T) {
	src := machineTemplate(t, `{"apiVersion":"infrastructure.cluster.x-k8s.io/v1alpha2","kind":"ProxmoxMachineTemplate",
	  "metadata":{"name":"devk8s-control-plane","namespace":"default"},
	  "spec":{"template":{"spec":{"sourceNode":"pve2","templateID":100}}}}`)
	_, obj, changed, err := resizedTemplate(src, "devk8s", "control-plane", ResizeRequest{MemoryMiB: 8192})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	spec := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if jsonInt(spec["memoryMiB"]) != 8192 {
		t.Fatalf("memoryMiB = %v", spec["memoryMiB"])
	}
	if _, has := spec["numCores"]; has {
		t.Fatal("a size nobody asked for must stay unset")
	}
}

func TestResizedTemplateRejectsMalformedSource(t *testing.T) {
	if _, _, _, err := resizedTemplate(map[string]any{"kind": "x"}, "c", "worker", ResizeRequest{MemoryMiB: 4096}); err == nil {
		t.Fatal("a template without spec.template.spec was accepted")
	}
}

func TestSizeFromTemplate(t *testing.T) {
	got := sizeFromTemplate(machineTemplate(t, liveWorker))
	want := CurrentSize{TemplateName: "prodk8s-worker", NumSockets: 1, NumCores: 8, MemoryMiB: 16384, BootGB: 100}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if (sizeFromTemplate(map[string]any{}) != CurrentSize{}) {
		t.Fatal("an empty object must give the zero size, not panic")
	}
}

func TestValidateResize(t *testing.T) {
	cases := []struct {
		name    string
		in      ResizeInput
		wantErr string
	}{
		{"nothing requested", ResizeInput{}, "at least one"},
		{"workers RAM only", ResizeInput{Workers: ResizeRequest{MemoryMiB: 25600}}, ""},
		{"both roles", ResizeInput{ControlPlane: ResizeRequest{NumCores: 4}, Workers: ResizeRequest{MemoryMiB: 8192}}, ""},
		{"RAM too small (MiB typed as GiB?)", ResizeInput{Workers: ResizeRequest{MemoryMiB: 512}}, "RAM must be between"},
		{"RAM absurd", ResizeInput{Workers: ResizeRequest{MemoryMiB: maxResizeMemoryMiB + 1}}, "RAM must be between"},
		{"too many cores", ResizeInput{ControlPlane: ResizeRequest{NumCores: 129}}, "cores must be between"},
		{"too many sockets", ResizeInput{Workers: ResizeRequest{NumSockets: 9}}, "sockets must be between"},
		{"negative", ResizeInput{Workers: ResizeRequest{NumCores: -1}}, "cores must be between"},
		{"boundary RAM", ResizeInput{Workers: ResizeRequest{MemoryMiB: minResizeMemoryMiB}}, ""},
	}
	for _, c := range cases {
		err := ValidateResize(c.in)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.wantErr)
		}
	}
}

func TestFormatMiBAndDescribe(t *testing.T) {
	if FormatMiB(25600) != "25 GiB" || FormatMiB(16392) != "16392 MiB" || FormatMiB(0) != "0 MiB" {
		t.Fatalf("FormatMiB: %q %q %q", FormatMiB(25600), FormatMiB(16392), FormatMiB(0))
	}
	if got := (ResizeRequest{MemoryMiB: 25600, NumCores: 8}).Describe(); got != "25 GiB RAM, 8 cores" {
		t.Fatalf("Describe = %q", got)
	}
}
