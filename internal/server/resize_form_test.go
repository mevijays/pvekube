package server

import (
	"net/url"
	"strings"
	"testing"

	"pvekube/internal/capi"
)

func form(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func TestParseResizeFormConvertsGiBToMiB(t *testing.T) {
	in, err := parseResizeForm("prodk8s", form("w_memory_gib", "25", "ack", "on"))
	if err != nil {
		t.Fatal(err)
	}
	want := capi.ResizeInput{ClusterName: "prodk8s", Workers: capi.ResizeRequest{MemoryMiB: 25600}}
	if in != want {
		t.Fatalf("got %+v want %+v", in, want)
	}
	// decimals, both roles, all fields
	in, err = parseResizeForm("c", form("cp_memory_gib", "12.5", "cp_cores", "4", "cp_sockets", "2", "w_cores", "16", "ack", "on"))
	if err != nil {
		t.Fatal(err)
	}
	if in.ControlPlane != (capi.ResizeRequest{MemoryMiB: 12800, NumCores: 4, NumSockets: 2}) || in.Workers != (capi.ResizeRequest{NumCores: 16}) {
		t.Fatalf("got %+v", in)
	}
}

func TestParseResizeFormBlankMeansUnchanged(t *testing.T) {
	in, err := parseResizeForm("c", form("w_memory_gib", "8", "cp_memory_gib", "  ", "cp_cores", "", "ack", "on"))
	if err != nil {
		t.Fatal(err)
	}
	if !in.ControlPlane.IsZero() {
		t.Fatalf("blank control plane fields requested a change: %+v", in.ControlPlane)
	}
}

// The destructive part must be acknowledged on the SERVER: the UI checkbox is
// only a convenience, a hand-made POST must not skip it.
func TestParseResizeFormRequiresAcknowledgement(t *testing.T) {
	for _, ack := range []string{"", "off", "true", "1"} {
		_, err := parseResizeForm("c", form("w_memory_gib", "8", "ack", ack))
		if err == nil || !strings.Contains(err.Error(), "confirm") {
			t.Errorf("ack=%q accepted or wrong error: %v", ack, err)
		}
	}
}

func TestParseResizeFormRejectsBadInput(t *testing.T) {
	// Zero must be refused for what it is, not fall through to "nothing entered".
	if _, err := parseResizeForm("c", form("w_memory_gib", "0", "ack", "on")); err == nil || !strings.Contains(err.Error(), "greater than 0") {
		t.Errorf("zero RAM: err = %v", err)
	}
	cases := map[string]url.Values{
		"nothing entered":       form("ack", "on"),
		"text in RAM":           form("w_memory_gib", "lots", "ack", "on"),
		"zero RAM":              form("w_memory_gib", "0", "ack", "on"),
		"negative cores":        form("cp_cores", "-2", "ack", "on"),
		"fractional cores":      form("w_cores", "2.5", "ack", "on"),
		"NaN RAM":               form("w_memory_gib", "NaN", "ack", "on"),
		"infinite RAM":          form("w_memory_gib", "Inf", "ack", "on"),
		"RAM in MiB by mistake": form("w_memory_gib", "25600", "ack", "on"), // 25 TiB
		"RAM too small":         form("cp_memory_gib", "0.5", "ack", "on"),
	}
	for name, f := range cases {
		if _, err := parseResizeForm("c", f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNewResizeRoleView(t *testing.T) {
	v := newResizeRoleView("w", "Workers", capi.CurrentSize{TemplateName: "t", Replicas: 3, NumSockets: 1, NumCores: 8, MemoryMiB: 16384}, form("w_memory_gib", "25"))
	if v.MemoryGiB != "16" || v.InMemory != "25" || v.Prefix != "w" || !strings.Contains(v.Summary, "1 socket(s) × 8 core(s)") || !strings.Contains(v.Summary, "16 GiB RAM") {
		t.Fatalf("view = %+v", v)
	}
	// A template that sets no sizing must say so, not show zeros.
	u := newResizeRoleView("cp", "Control plane", capi.CurrentSize{Replicas: 1}, nil)
	if !strings.Contains(u.Summary, "Proxmox template") || u.MemoryGiB != "" {
		t.Fatalf("unsized view = %+v", u)
	}
}
