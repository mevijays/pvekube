package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"sort"
)

// Snapshot is everything downstream screens (template builder, cluster
// designer) need, gathered in one pass and cached so those screens can
// populate dropdowns from real data instead of free-text fields a user
// could typo.
type Snapshot struct {
	Version  string    `json:"version"`
	Nodes    []Node    `json:"nodes"`
	Storage  []Storage `json:"storage"`
	Bridges  []Bridge  `json:"bridges"`
	NextVMID int       `json:"next_vmid"`
}

type Node struct {
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	CPUCores    int     `json:"cpu_cores"`
	CPUUsedPct  float64 `json:"cpu_used_pct"`
	MemTotalMB  int64   `json:"mem_total_mb"`
	MemUsedMB   int64   `json:"mem_used_mb"`
	DiskTotalGB int64   `json:"disk_total_gb"`
	DiskUsedGB  int64   `json:"disk_used_gb"`

	// MemAllocatedMB is the sum of every guest's CONFIGURED memory on this
	// node — not what they're actually using. MemReservableMB is what's left
	// for a new VM. These exist because Proxmox's own "free memory" is
	// actively misleading when planning a cluster: CAPMOX refuses to
	// overcommit, so a node can show tens of GB free while still rejecting a
	// new worker. See nodeAllocation for the derivation.
	MemAllocatedMB  int64 `json:"mem_allocated_mb"`
	MemReservableMB int64 `json:"mem_reservable_mb"`
	// CapacityKnown distinguishes "nothing allocated" from "this snapshot
	// predates capacity tracking". Discovery snapshots are cached in SQLite,
	// so an older cached blob decodes with the fields above at zero, which
	// would otherwise render as "everything is free" — the exact wrong
	// answer. The UI shows a refresh prompt instead when this is false.
	CapacityKnown bool `json:"capacity_known"`
}

// Storage describes one storage pool, resolved down to the disk format
// PVEKube should use with it: ZFS/LVM-backed pools don't support qcow2, so
// the template builder and cluster designer must pick "raw" automatically
// rather than making the user guess.
type Storage struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"` // dir, lvmthin, zfspool, nfs, ...
	ContentTypes []string `json:"content_types"`
	SupportsISO  bool     `json:"supports_iso"`
	SupportsVM   bool     `json:"supports_vm"`
	DiskFormat   string   `json:"disk_format"` // "qcow2" or "raw"
	Shared       bool     `json:"shared"`
}

type Bridge struct {
	Node      string `json:"node"`
	Name      string `json:"name"`
	Address   string `json:"address,omitempty"`
	VLANAware bool   `json:"vlan_aware"`
}

func (c *Client) Discover(ctx context.Context) (*Snapshot, error) {
	snap := &Snapshot{}

	version, err := c.Version(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking version (is the URL/token correct?): %w", err)
	}
	snap.Version = version

	nodes, err := c.nodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing nodes (needs Sys.Audit — see permission checklist): %w", err)
	}
	for i := range nodes {
		alloc, err := c.nodeAllocation(ctx, nodes[i].Name)
		if err != nil {
			// Capacity is advisory: a node that won't list its guests
			// shouldn't block discovery and lock the operator out of every
			// screen. Leave CapacityKnown false so the UI says "unknown"
			// rather than implying the node is empty.
			continue
		}
		nodes[i].MemAllocatedMB = alloc / 1024 / 1024
		if r := nodes[i].MemTotalMB - nodes[i].MemAllocatedMB; r > 0 {
			nodes[i].MemReservableMB = r
		}
		nodes[i].CapacityKnown = true
	}
	snap.Nodes = nodes

	storage, err := c.storage(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing storage (needs Datastore.Audit): %w", err)
	}
	snap.Storage = storage

	var bridges []Bridge
	for _, n := range nodes {
		bs, err := c.bridges(ctx, n.Name)
		if err != nil {
			return nil, fmt.Errorf("listing network on node %s (needs Sys.Audit): %w", n.Name, err)
		}
		bridges = append(bridges, bs...)
	}
	snap.Bridges = bridges

	nextID, err := c.nextVMID(ctx)
	if err != nil {
		return nil, fmt.Errorf("allocating next VMID: %w", err)
	}
	snap.NextVMID = nextID

	return snap, nil
}

func (c *Client) nodes(ctx context.Context) ([]Node, error) {
	var raw []struct {
		Node    string  `json:"node"`
		Status  string  `json:"status"`
		MaxCPU  int     `json:"maxcpu"`
		CPU     float64 `json:"cpu"`
		MaxMem  int64   `json:"maxmem"`
		Mem     int64   `json:"mem"`
		MaxDisk int64   `json:"maxdisk"`
		Disk    int64   `json:"disk"`
	}
	if err := c.get(ctx, "/nodes", &raw); err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(raw))
	for _, n := range raw {
		out = append(out, Node{
			Name: n.Node, Status: n.Status, CPUCores: n.MaxCPU, CPUUsedPct: n.CPU * 100,
			MemTotalMB: n.MaxMem / 1024 / 1024, MemUsedMB: n.Mem / 1024 / 1024,
			DiskTotalGB: n.MaxDisk / 1024 / 1024 / 1024, DiskUsedGB: n.Disk / 1024 / 1024 / 1024,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// nodeAllocation returns the total configured memory (bytes) of every guest
// on a node, which is what CAPMOX subtracts from the node's total when
// deciding whether a new VM fits.
//
// This is a deliberate port of CAPMOX v0.9.0's
// APIClient.GetReservableMemoryBytes (pkg/proxmox/goproxmox/api_client.go) —
// read from its source, not inferred from behaviour, after two attempts to
// derive the rule from observed numbers gave wrong answers. The rules that
// matter and are easy to get wrong:
//
//   - It uses each guest's maxmem (configured size), NOT current usage. A
//     node showing 60 GB "free" in `free -h` can still reject a new VM.
//   - STOPPED guests count in full. Powering a VM off frees nothing as far
//     as scheduling is concerned; only reducing or deleting it does.
//   - VM templates are skipped (they can never start), but there is no
//     equivalent skip for containers.
//   - Both QEMU VMs and LXC containers count.
//
// Note CAPMOX also scales the node total by schedulerHints.memoryAdjustment
// (default 100 = no overcommit). PVEKube never sets that field, so the
// default applies and this reports the un-adjusted figure.
func (c *Client) nodeAllocation(ctx context.Context, node string) (int64, error) {
	var vms []struct {
		VMID     int   `json:"vmid"`
		MaxMem   int64 `json:"maxmem"`
		Template int   `json:"template"`
	}
	if err := c.get(ctx, "/nodes/"+node+"/qemu", &vms); err != nil {
		return 0, err
	}
	var total int64
	for _, vm := range vms {
		if vm.Template != 0 {
			continue
		}
		total += vm.MaxMem
	}

	var cts []struct {
		VMID   int   `json:"vmid"`
		MaxMem int64 `json:"maxmem"`
	}
	if err := c.get(ctx, "/nodes/"+node+"/lxc", &cts); err != nil {
		return 0, err
	}
	for _, ct := range cts {
		total += ct.MaxMem
	}
	return total, nil
}

func (c *Client) storage(ctx context.Context) ([]Storage, error) {
	var raw []struct {
		Storage string `json:"storage"`
		Type    string `json:"type"`
		Content string `json:"content"`
		Shared  int    `json:"shared"`
	}
	if err := c.get(ctx, "/storage", &raw); err != nil {
		return nil, err
	}
	out := make([]Storage, 0, len(raw))
	for _, s := range raw {
		contents := splitCSV(s.Content)
		diskFormat := "qcow2"
		// Block-based storage backends (LVM-thin, ZFS, Ceph RBD) don't
		// support qcow2 files — only raw block images. This is one of
		// the "silent failure at build time" traps called out in PLAN.md.
		switch s.Type {
		case "lvmthin", "lvm", "zfspool", "rbd", "iscsi":
			diskFormat = "raw"
		}
		out = append(out, Storage{
			ID: s.Storage, Type: s.Type, ContentTypes: contents,
			SupportsISO: containsStr(contents, "iso"),
			SupportsVM:  containsStr(contents, "images"),
			DiskFormat:  diskFormat,
			Shared:      s.Shared == 1,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (c *Client) bridges(ctx context.Context, node string) ([]Bridge, error) {
	var raw []struct {
		Iface      string `json:"iface"`
		Type       string `json:"type"`
		Address    string `json:"address"`
		BridgeVLAN int    `json:"bridge_vlan_aware"`
	}
	if err := c.get(ctx, "/nodes/"+node+"/network", &raw); err != nil {
		return nil, err
	}
	var out []Bridge
	for _, n := range raw {
		if n.Type != "bridge" {
			continue
		}
		out = append(out, Bridge{Node: node, Name: n.Iface, Address: n.Address, VLANAware: n.BridgeVLAN == 1})
	}
	return out, nil
}

// NextVMID allocates the next free VMID from Proxmox. Exported so callers
// (e.g. the template builder) can pre-allocate a deterministic VMID before
// starting a build, rather than letting Packer pick one implicitly.
func (c *Client) NextVMID(ctx context.Context) (int, error) {
	return c.nextVMID(ctx)
}

func (c *Client) nextVMID(ctx context.Context) (int, error) {
	// Proxmox returns this as a JSON string ("115"), not a number.
	var idStr string
	if err := c.get(ctx, "/cluster/nextid", &idStr); err != nil {
		return 0, err
	}
	var id int
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		return 0, fmt.Errorf("parsing nextid %q: %w", idStr, err)
	}
	return id, nil
}

// DeleteVM deletes a VM or template by VMID (DELETE /nodes/{node}/qemu/{vmid})
// and waits for the deletion task to finish. Used by the template builder to
// let an operator remove a template they no longer need directly from
// PVEKube instead of going to the Proxmox UI.
func (c *Client) DeleteVM(ctx context.Context, node string, vmid int) error {
	var upid string
	if err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/nodes/%s/qemu/%d", node, vmid), nil, &upid); err != nil {
		return err
	}
	return c.WaitTask(ctx, node, upid, nil)
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
