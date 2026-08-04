package proxmox

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ISO is one entry in a storage pool's ISO content listing.
type ISO struct {
	VolID    string // e.g. "local:iso/ubuntu-22.04.5-live-server-amd64.iso"
	Filename string // e.g. "ubuntu-22.04.5-live-server-amd64.iso"
	SizeMB   int64
}

// ListISOs returns every ISO already present in the given node/storage pool.
// Used to check whether a template build's ISO needs downloading at all —
// image-builder's Packer templates support pointing directly at an existing
// Proxmox-side ISO via the ISO_FILE env var, which avoids both the client
// download AND the client->Proxmox upload (the latter is what times out on
// slow/flaky links for multi-GB ISOs).
func (c *Client) ListISOs(ctx context.Context, node, storage string) ([]ISO, error) {
	var raw []struct {
		VolID string `json:"volid"`
		Size  int64  `json:"size"`
	}
	if err := c.get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content?content=iso", node, storage), &raw); err != nil {
		return nil, err
	}
	out := make([]ISO, 0, len(raw))
	for _, r := range raw {
		name := r.VolID
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			name = name[idx+1:]
		}
		out = append(out, ISO{VolID: r.VolID, Filename: name, SizeMB: r.Size / (1024 * 1024)})
	}
	return out, nil
}

// HasISO reports whether filename already exists in node/storage.
func (c *Client) HasISO(ctx context.Context, node, storage, filename string) (bool, error) {
	isos, err := c.ListISOs(ctx, node, storage)
	if err != nil {
		return false, err
	}
	for _, iso := range isos {
		if iso.Filename == filename {
			return true, nil
		}
	}
	return false, nil
}

// DownloadISOToStorage tells Proxmox itself to fetch url and store it as
// filename in node/storage — the download runs server-side on Proxmox, not
// through this client, so it isn't subject to the client<->Proxmox network
// path that a client-side upload would be. Returns the task's UPID for use
// with WaitTask. checksum/checksumAlgo may be empty to skip verification.
func (c *Client) DownloadISOToStorage(ctx context.Context, node, storage, isoURL, filename, checksum, checksumAlgo string) (string, error) {
	form := url.Values{
		"content":  {"iso"},
		"filename": {filename},
		"url":      {isoURL},
	}
	if checksum != "" && checksumAlgo != "" {
		form.Set("checksum", checksum)
		form.Set("checksum-algorithm", checksumAlgo)
	}
	var upid string
	err := c.post(ctx, fmt.Sprintf("/nodes/%s/storage/%s/download-url", node, storage), form, &upid)
	return upid, err
}

// TaskStatus is the subset of Proxmox's /nodes/{node}/tasks/{upid}/status
// response WaitTask needs to know whether a task is done and whether it
// succeeded.
type TaskStatus struct {
	Status     string `json:"status"`     // "running" | "stopped"
	ExitStatus string `json:"exitstatus"` // "OK" on success once stopped
}

// WaitTask polls a Proxmox task (e.g. from DownloadISOToStorage) until it
// stops, calling onLog with each newly available task log line as it's
// produced. Returns an error if the task ends with a non-OK exit status or
// the context is cancelled first.
func (c *Client) WaitTask(ctx context.Context, node, upid string, onLog func(string)) error {
	logStart := 0
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		if onLog != nil {
			lines, err := c.taskLog(ctx, node, upid, logStart)
			if err == nil {
				for _, l := range lines {
					onLog(l)
				}
				logStart += len(lines)
			}
		}

		var st TaskStatus
		if err := c.get(ctx, fmt.Sprintf("/nodes/%s/tasks/%s/status", node, url.PathEscape(upid)), &st); err != nil {
			return fmt.Errorf("polling task status: %w", err)
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" {
				return fmt.Errorf("proxmox task failed: %s", st.ExitStatus)
			}
			return nil
		}
	}
}

func (c *Client) taskLog(ctx context.Context, node, upid string, start int) ([]string, error) {
	var raw []struct {
		T string `json:"t"`
	}
	path := fmt.Sprintf("/nodes/%s/tasks/%s/log?start=%d", node, url.PathEscape(upid), start)
	if err := c.get(ctx, path, &raw); err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(raw))
	for _, r := range raw {
		lines = append(lines, r.T)
	}
	return lines, nil
}
