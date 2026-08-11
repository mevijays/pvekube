package ui

import (
	"bytes"
	"strings"
	"testing"
)

// These partials contain forms, and a form that renders with a missing
// template variable is the exact failure mode that made "Delete cluster"
// silently do nothing: the csrf field rendered empty, every POST 403'd, and
// htmx doesn't swap error responses so the UI looked frozen. Nothing about
// that is visible until a human clicks the button.
//
// Each case below passes the SAME map its handler passes. If a handler grows
// a new template variable and one render path forgets it, this fails loudly
// at build time instead of silently at 3am.
type partialCase struct {
	name    string
	partial string
	data    map[string]any
	// mustContain guards against a variable resolving to empty rather than
	// erroring — Go templates render a missing map key as "" in an
	// attribute, which is precisely how the csrf bug hid.
	mustContain []string
	// mustNotContain catches the opposite failure: a value rendering into a
	// place it shouldn't (e.g. option 1 wrongly marked selected).
	mustNotContain []string
}

func defaultsStub() any {
	// Mirrors server.clusterDefaults' shape. Kept as an anonymous struct so
	// this package doesn't import server (which would be a cycle).
	return struct {
		VMSSHKeys        string
		RegistryHost     string
		RegistryCACert   string
		RegistryUsername string
		RegistryPassword string
	}{
		VMSSHKeys:      "ssh-ed25519 AAAAC3Nz",
		RegistryHost:   "registry.internal.lan:5000",
		RegistryCACert: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
	}
}

// nodesStub mirrors the subset of proxmox.Node the capacity table reads.
// capacityKnown=false models a discovery snapshot cached before capacity
// tracking existed, which must NOT render as "everything free".
func snapshotStub(capacityKnown bool) any {
	type node struct {
		Name            string
		MemTotalMB      int64
		MemAllocatedMB  int64
		MemReservableMB int64
		CapacityKnown   bool
	}
	// A map rather than a struct: the template also reaches for
	// .Snapshot.Bridges, and a map yields nil for keys it doesn't define
	// instead of failing to execute.
	return map[string]any{
		"Nodes": []node{
			// 128 GiB total, 96 GiB reserved, 32 GiB free.
			{Name: "host245", MemTotalMB: 131072, MemAllocatedMB: 98304, MemReservableMB: 32768, CapacityKnown: capacityKnown},
		},
		"Bridges": nil,
	}
}

// connStub mirrors server.storedConnection closely enough to exercise
// proxmox_list/proxmox_connected/proxmox_detail_error — it needs to be a
// named type (not an anonymous struct literal like the other stubs in this
// file) because those partials call .Conn.TokenUser, a method, and Go
// template execution invokes methods via reflection same as fields; an
// anonymous struct can't carry one.
type connStub struct {
	ID        int64
	URL       string
	TokenID   string
	IsPrimary bool
}

func (c connStub) TokenUser() string { return "capmox@pve" }

// permStub mirrors proxmox.PermissionResult (the .Perms range target in
// proxmox_connected.html).
type permStub struct {
	Name, Privilege, FixCommand string
	OK, Probed                  bool
}

// templatesStub mirrors server.templateOptionView.
func templatesStub() any {
	return []struct {
		ID         int64
		OSFlavor   string
		K8sVersion string
		Node       string
		VMID       int
	}{
		{ID: 1, OSFlavor: "ubuntu-2404", K8sVersion: "v1.32.1", Node: "host245", VMID: 115},
	}
}

func TestPartialsRenderWithHandlerData(t *testing.T) {
	cases := []partialCase{
		{
			// With at least one template the creation form actually renders,
			// which is the case that exercises Defaults and csrf.
			name:    "clusters_panel/full",
			partial: "clusters_panel",
			data: map[string]any{
				"Error":     "",
				"CSRF":      "csrf-token-here",
				"Snapshot":  snapshotStub(true),
				"Templates": templatesStub(),
				"Defaults":  defaultsStub(),
				// A missing map key here silently renders as an empty
				// string rather than erroring — confirmed by direct
				// inspection, not assumption — so these fields MUST be set
				// explicitly for this test to actually verify the hidden
				// conn field and host <select>, not just fail to crash.
				"Connections":  []connStub{{ID: 1, URL: "https://172.16.1.101:8006", IsPrimary: true}, {ID: 2, URL: "https://172.16.1.101:8006", IsPrimary: false}},
				"SelectedConn": connStub{ID: 2, URL: "https://172.16.1.101:8006", IsPrimary: false},
			},
			mustContain: []string{
				"csrf-token-here",
				// Defaults must reach the form fields, or "remember my CA"
				// silently does nothing.
				"registry.internal.lan:5000",
				"ssh-ed25519 AAAAC3Nz",
				"BEGIN CERTIFICATE",
				// Capacity table: 131072 MiB -> 128.0, 98304 -> 96.0,
				// 32768 -> 32.0. Guards both the gib filter and the wiring.
				"128.0 GiB", "96.0 GiB", "32.0 GiB",
				// The create form must submit against the SAME connection
				// selected in the dropdown, not a stale/default one.
				`name="conn" value="2"`,
				`value="2" selected`,
			},
		},
		{
			// A snapshot cached before capacity tracking must say so rather
			// than reporting 0 reserved / everything free.
			name:    "clusters_panel/capacity-unknown",
			partial: "clusters_panel",
			data: map[string]any{
				"Error": "", "CSRF": "csrf-token-here",
				"Snapshot":     snapshotStub(false),
				"Templates":    templatesStub(),
				"Defaults":     defaultsStub(),
				"Connections":  []connStub{{ID: 1, URL: "https://172.16.1.101:8006", IsPrimary: true}},
				"SelectedConn": connStub{ID: 1, URL: "https://172.16.1.101:8006", IsPrimary: true},
			},
			mustContain:    []string{"Capacity not in this cached snapshot"},
			mustNotContain: []string{"128.0 GiB", "0.0 GiB"},
		},
		{
			name:    "clusters_panel/discovery-failed",
			partial: "clusters_panel",
			data: map[string]any{
				"Error":    "Discovery failed: boom",
				"CSRF":     "csrf-token-here",
				"Defaults": defaultsStub(),
			},
			mustContain: []string{"Discovery failed: boom"},
		},
		{
			name:    "proxmox_list/multiple-hosts",
			partial: "proxmox_list",
			data: map[string]any{
				"CSRF": "csrf-token-here",
				"Connections": []connStub{
					{ID: 1, URL: "https://172.16.1.101:8006", TokenID: "capmox@pve!capi", IsPrimary: true},
					{ID: 2, URL: "https://172.16.1.101:8006", TokenID: "capmox@pve!second", IsPrimary: false},
				},
			},
			mustContain: []string{
				"csrf-token-here",
				"primary", // badge on connection 1
				`/proxmox/1/disconnect`, `/proxmox/2/disconnect`,
				`/proxmox/1/detail`, `/proxmox/2/detail`,
				"conn-detail-1", "conn-detail-2",
			},
		},
		{
			// Empty state must still render the Add-host form — this is
			// the very first screen a fresh install shows.
			name:        "proxmox_list/no-connections",
			partial:     "proxmox_list",
			data:        map[string]any{"CSRF": "csrf-token-here", "Connections": nil},
			mustContain: []string{"No Proxmox hosts connected yet", "csrf-token-here", "Add Proxmox host"},
		},
		{
			name:    "proxmox_connected/ok",
			partial: "proxmox_connected",
			data: map[string]any{
				"Conn": connStub{ID: 7, URL: "https://172.16.1.101:8006", TokenID: "capmox@pve!capi", IsPrimary: true},
				"CSRF": "csrf-token-here",
				"Snapshot": map[string]any{
					"Version": "9.1.4", "Nodes": nil, "Bridges": nil, "Storage": nil, "NextVMID": 115,
				},
				"Perms": []permStub{{Name: "Authentication", Privilege: "(valid token)", OK: true, Probed: true}},
			},
			mustContain: []string{
				"csrf-token-here",
				`/proxmox/7/refresh`, `#conn-detail-7`,
				`/proxmox/7/disconnect`,
				// The primary-specific disconnect warning must actually be
				// present for a primary connection, not just the generic one.
				"PRIMARY connection",
			},
		},
		{
			// Same partial, non-primary connection: the sharper warning
			// must NOT appear — asserting its absence is what would have
			// caught a copy-paste "always show this" mistake.
			name:    "proxmox_connected/non-primary",
			partial: "proxmox_connected",
			data: map[string]any{
				"Conn": connStub{ID: 8, URL: "https://172.16.1.101:8006", TokenID: "capmox@pve!second", IsPrimary: false},
				"CSRF": "csrf-token-here",
				"Snapshot": map[string]any{
					"Version": "9.1.4", "Nodes": nil, "Bridges": nil, "Storage": nil, "NextVMID": 116,
				},
				"Perms": []permStub{{Name: "Authentication", Privilege: "(valid token)", OK: true, Probed: true}},
			},
			mustContain:    []string{"csrf-token-here", `/proxmox/8/refresh`, `/proxmox/8/disconnect`},
			mustNotContain: []string{"PRIMARY connection"},
		},
		{
			// templates_panel had zero test coverage before this — exactly
			// the kind of gap where a nil SelectedConn or a forgotten field
			// would render "<no value>" or panic on nil-pointer field
			// access, undetected until someone actually opened the page.
			name:    "templates_panel/multi-host",
			partial: "templates_panel",
			data: map[string]any{
				"CSRF":    "csrf-token-here",
				"Flavors": []struct{ ID, Label string }{{ID: "ubuntu-2404", Label: "Ubuntu 24.04"}},
				"Snapshot": map[string]any{
					"Nodes": nil, "Bridges": nil, "Storage": nil,
				},
				"Built":        nil,
				"LinuxOK":      true,
				"HostOS":       "linux",
				"Connections":  []connStub{{ID: 1, URL: "https://172.16.1.101:8006", IsPrimary: true}, {ID: 2, URL: "https://172.16.1.101:8006", IsPrimary: false}},
				"SelectedConn": connStub{ID: 2, URL: "https://172.16.1.101:8006", IsPrimary: false},
				"BuildBusy":    false,
			},
			mustContain: []string{
				"csrf-token-here",
				`name="conn" value="2"`, // the hidden field must carry SelectedConn, not just any connection
				`value="2" selected`,    // and the dropdown must show it as the chosen option
			},
			mustNotContain: []string{"already running", "<no value>"},
		},
		{
			// BuildBusy must actually disable the button and explain why —
			// this is the guard against the imagebuilder concurrent-build
			// race (see templateBuildInProgress's doc comment).
			name:    "templates_panel/build-busy",
			partial: "templates_panel",
			data: map[string]any{
				"CSRF": "csrf-token-here", "Flavors": []struct{ ID, Label string }{},
				"Snapshot":     map[string]any{},
				"Connections":  []connStub{{ID: 1, URL: "https://172.16.1.101:8006", IsPrimary: true}},
				"SelectedConn": connStub{ID: 1, URL: "https://172.16.1.101:8006", IsPrimary: true},
				"BuildBusy":    true,
			},
			mustContain: []string{"already running", "disabled"},
		},
		{
			name:    "proxmox_detail_error/ok",
			partial: "proxmox_detail_error",
			data: map[string]any{
				"Conn":  connStub{ID: 3, URL: "https://172.16.1.101:8006"},
				"Error": "Connected before, but discovery failed now: dial tcp: timeout",
			},
			mustContain: []string{"dial tcp: timeout", `/proxmox/3/refresh`, `#conn-detail-3`},
		},
		{
			name:    "cluster_preview/ok",
			partial: "cluster_preview",
			data: map[string]any{
				"ClusterName": "demo", "TemplateID": int64(1), "YAML": "apiVersion: v1",
				"CSRF": "csrf-token-here", "CNI": "calico",
				"InstallMetricsServer": true, "InstallIstio": false,
				"InstallMetalLB": true, "MetalLBIPPool": "10.0.0.1-10.0.0.9",
				"RegistryHost": "registry.internal.lan:5000", "RegistryCACert": "PEM",
				"RegistryUsername": "u", "RegistryPassword": "p",
				"RegistryInsecure": false,
				"VMSSHKeys":        "ssh-ed25519 AAAAC3Nz",
				"ConnID":           int64(5),
			},
			// The apply form must carry the SAME connection the manifest was
			// generated against — see formConnection's doc comment
			// (handlers_clusters.go) for the race this prevents.
			mustContain: []string{"csrf-token-here", "registry.internal.lan:5000", `name="conn" value="5"`},
		},
		{
			name:        "cluster_preview/error",
			partial:     "cluster_preview",
			data:        map[string]any{"Error": "generate failed"},
			mustContain: []string{"generate failed"},
		},
		{
			name:    "cluster_status/found",
			partial: "cluster_status",
			data: map[string]any{
				"ClusterName": "demo",
				"CSRF":        "csrf-token-here",
				"Status": map[string]any{
					"Found": true, "Phase": "Provisioned",
					"ControlPlaneReady": true, "InfrastructureReady": true,
					"WorkerReplicas": 2, "ControlPlaneReplicas": 3,
				},
				"KubeconfigReady": true,
			},
			mustContain: []string{
				// The delete/scale forms must carry a real csrf value.
				`name="csrf" value="csrf-token-here"`,
				// Scale inputs need a stable id + hx-preserve so the "every
				// 5s" poll above can't silently revert a value the operator
				// is mid-typing — see cluster_status.html's comment. A
				// scale-workers submit that reached the server asking for
				// "no change" (root-caused by this bug) is what prompted
				// this test.
				`id="scale-workers-replicas" hx-preserve="true"`,
				`id="scale-controlplane-replicas" hx-preserve="true"`,
				// ControlPlaneReplicas must actually drive which <option>
				// is selected, not just render 1 unconditionally.
				`<option value="3" selected>3</option>`,
			},
			mustNotContain: []string{`<option value="1" selected>`},
		},
		{
			name:    "cluster_status/not-found",
			partial: "cluster_status",
			data: map[string]any{
				"ClusterName": "demo",
				"CSRF":        "csrf-token-here",
				"Status":      map[string]any{"Found": false},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := RenderPartial(&buf, tc.partial, tc.data); err != nil {
				t.Fatalf("rendering %s: %v", tc.partial, err)
			}
			out := buf.String()
			if strings.Contains(out, "<no value>") {
				t.Errorf("%s rendered a missing template variable as <no value>", tc.partial)
			}
			for _, want := range tc.mustContain {
				if !strings.Contains(out, want) {
					t.Errorf("%s output missing %q", tc.partial, want)
				}
			}
			for _, unwanted := range tc.mustNotContain {
				if strings.Contains(out, unwanted) {
					t.Errorf("%s output should not contain %q", tc.partial, unwanted)
				}
			}
		})
	}
}

// Every page template must at least execute against a minimal context, so a
// typo'd field name in a rarely-visited page is caught here rather than by
// whoever opens that page next.
func TestPagesExecute(t *testing.T) {
	base := map[string]any{
		"CSRF": "t", "ClusterName": "demo", "Error": "",
	}
	for name := range pageFiles {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Render(&buf, name, base); err != nil {
				t.Fatalf("rendering page %s: %v", name, err)
			}
		})
	}
}
