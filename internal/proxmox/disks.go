package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// ExtraDiskSlot is the SCSI slot PVEKube uses for a worker's extra data disk.
// CAPMOX only ever creates scsi0 (the boot volume), so scsi1 is free and shows
// up in the guest as /dev/sdb — which is exactly what the Rook-Ceph
// `deviceFilter: ^sdb$` expects. A fixed slot is also what makes the operation
// idempotent: "does scsi1 exist" is the whole question.
const ExtraDiskSlot = "scsi1"

// Bounds for a requested disk. The floor keeps out typos (a "2" meant as TB);
// the ceiling is far above any homelab disk but still catches a pasted number.
const (
	MinExtraDiskGiB = 1
	MaxExtraDiskGiB = 16384
)

var storageIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidateExtraDisk checks a storage id and size before they are interpolated
// into a Proxmox disk string. The storage id is validated strictly because
// the disk string is comma-separated options: an id containing "," would let a
// caller smuggle extra options into the volume definition.
func ValidateExtraDisk(storage string, sizeGiB int) error {
	if !storageIDPattern.MatchString(storage) {
		return fmt.Errorf("%q is not a valid Proxmox storage id", storage)
	}
	if sizeGiB < MinExtraDiskGiB || sizeGiB > MaxExtraDiskGiB {
		return fmt.Errorf("disk size must be between %d and %d GiB, got %d", MinExtraDiskGiB, MaxExtraDiskGiB, sizeGiB)
	}
	return nil
}

// extraDiskValue is the value of the scsiN config key. "storage:N" asks
// Proxmox to allocate a new N GiB volume. discard=on lets thin-provisioned
// pools reclaim space; backup=0 keeps a Ceph OSD's data disk out of VM
// backups (an OSD is replicated by Ceph itself, and a stale block-level copy of
// one is not something to restore).
func extraDiskValue(storage string, sizeGiB int) string {
	return fmt.Sprintf("%s:%d,discard=on,backup=0", storage, sizeGiB)
}

// VMConfig returns a VM's configuration (pending changes already applied).
func (c *Client) VMConfig(ctx context.Context, node string, vmid int) (map[string]any, error) {
	var cfg map[string]any
	if err := c.get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// DiskResult reports what EnsureExtraDisk did.
type DiskResult struct {
	// Added is true when this call created the disk; false when the slot was
	// already populated and nothing was changed.
	Added bool
	// Existing is the current value of the slot when it was already populated.
	Existing string
	// Pending is true when Proxmox accepted the change but could not hot-plug
	// it, so the disk only appears in the guest after the VM is restarted.
	Pending bool
}

// EnsureExtraDisk attaches a new disk on `storage` of sizeGiB to the VM in
// ExtraDiskSlot, unless that slot is already in use. It never resizes, replaces
// or removes an existing disk.
func (c *Client) EnsureExtraDisk(ctx context.Context, node string, vmid int, storage string, sizeGiB int) (DiskResult, error) {
	if err := ValidateExtraDisk(storage, sizeGiB); err != nil {
		return DiskResult{}, err
	}
	cfg, err := c.VMConfig(ctx, node, vmid)
	if err != nil {
		return DiskResult{}, fmt.Errorf("reading VM %d config: %w", vmid, err)
	}
	if v, ok := cfg[ExtraDiskSlot]; ok {
		return DiskResult{Existing: fmt.Sprint(v)}, nil
	}

	// POST (not PUT) is Proxmox's asynchronous config update: it answers with a
	// task id, so the disk allocation is waited on rather than assumed.
	form := url.Values{ExtraDiskSlot: {extraDiskValue(storage, sizeGiB)}}
	var upid string
	if err := c.post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), form, &upid); err != nil {
		return DiskResult{}, fmt.Errorf("adding %s to VM %d: %w", ExtraDiskSlot, vmid, err)
	}
	if strings.HasPrefix(upid, "UPID:") {
		if err := c.WaitTask(ctx, node, upid, nil); err != nil {
			return DiskResult{}, fmt.Errorf("adding %s to VM %d: %w", ExtraDiskSlot, vmid, err)
		}
	}

	res := DiskResult{Added: true}
	var pending []map[string]any
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/pending", node, vmid), nil, &pending); err == nil {
		res.Pending = slotPending(pending, ExtraDiskSlot)
	}
	return res, nil
}

// slotPending reports whether a key has a change queued that is not yet live.
func slotPending(entries []map[string]any, key string) bool {
	for _, e := range entries {
		if e["key"] == key {
			if p, ok := e["pending"]; ok && p != nil {
				return true
			}
		}
	}
	return false
}
