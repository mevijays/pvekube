package proxmox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestValidateExtraDisk(t *testing.T) {
	ok := []struct {
		s string
		n int
	}{{"local-lvm", 100}, {"ceph_pool.1", 1}, {"zfs", MaxExtraDiskGiB}}
	for _, c := range ok {
		if err := ValidateExtraDisk(c.s, c.n); err != nil {
			t.Errorf("%q/%d should be valid: %v", c.s, c.n, err)
		}
	}
	bad := []struct {
		s string
		n int
	}{
		{"", 10}, {"local,lvm", 10}, {"a b", 10}, {"-x", 10}, {"x:y", 10}, {"x\n", 10},
		{"local-lvm", 0}, {"local-lvm", -5}, {"local-lvm", MaxExtraDiskGiB + 1},
	}
	for _, c := range bad {
		if err := ValidateExtraDisk(c.s, c.n); err == nil {
			t.Errorf("%q/%d should be rejected", c.s, c.n)
		}
	}
}

func TestExtraDiskValue(t *testing.T) {
	got := extraDiskValue("local-lvm", 100)
	if got != "local-lvm:100,discard=on,backup=0" {
		t.Fatalf("unexpected disk value %q", got)
	}
}

func TestSlotPending(t *testing.T) {
	entries := []map[string]any{
		{"key": "memory", "value": 4096, "pending": 8192},
		{"key": "scsi1", "pending": "local-lvm:100"},
	}
	if !slotPending(entries, "scsi1") {
		t.Fatal("scsi1 pending should be detected")
	}
	if slotPending([]map[string]any{{"key": "scsi1", "value": "x"}}, "scsi1") {
		t.Fatal("an applied value is not pending")
	}
	if slotPending(entries, "scsi2") {
		t.Fatal("a different slot is not pending")
	}
}

// fakePVE serves the handful of endpoints EnsureExtraDisk touches and records
// the POST bodies it receives.
type fakePVE struct {
	mu      sync.Mutex
	cfg     map[string]any
	pending []map[string]any
	posts   []string
	taskOK  bool
}

func (f *fakePVE) handler(t *testing.T) http.Handler {
	reply := func(w http.ResponseWriter, v any) {
		json.NewEncoder(w).Encode(map[string]any{"data": v})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/api2/json")
		switch {
		case r.Method == http.MethodGet && p == "/nodes/pve1/qemu/101/config":
			reply(w, f.cfg)
		case r.Method == http.MethodPost && p == "/nodes/pve1/qemu/101/config":
			r.ParseForm()
			f.posts = append(f.posts, r.Form.Encode())
			reply(w, "UPID:pve1:0001:0002:0003:qmconfig:101:root@pam!x:")
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/status"):
			exit := "OK"
			if !f.taskOK {
				exit = "storage 'nope' does not exist"
			}
			reply(w, map[string]any{"status": "stopped", "exitstatus": exit})
		case r.Method == http.MethodGet && p == "/nodes/pve1/qemu/101/pending":
			reply(w, f.pending)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	})
}

func newFake(t *testing.T, f *fakePVE) *Client {
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return New(Config{URL: srv.URL, TokenID: "u@pve!t", Secret: "s"})
}

func TestEnsureExtraDiskAddsWhenSlotFree(t *testing.T) {
	f := &fakePVE{cfg: map[string]any{"scsi0": "local-lvm:vm-101-disk-0,size=100G"}, taskOK: true}
	c := newFake(t, f)
	res, err := c.EnsureExtraDisk(t.Context(), "pve1", 101, "local-lvm", 200)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Added || res.Pending {
		t.Fatalf("unexpected result %+v", res)
	}
	if len(f.posts) != 1 {
		t.Fatalf("expected exactly one config POST, got %v", f.posts)
	}
	want := "scsi1=local-lvm%3A200%2Cdiscard%3Don%2Cbackup%3D0"
	if f.posts[0] != want {
		t.Fatalf("POST body = %q, want %q", f.posts[0], want)
	}
}

// Re-running (after a retry, or on the next rollout) must not touch a VM that
// already has the disk.
func TestEnsureExtraDiskIsIdempotent(t *testing.T) {
	f := &fakePVE{cfg: map[string]any{"scsi0": "a", "scsi1": "local-lvm:vm-101-disk-1,size=200G"}, taskOK: true}
	c := newFake(t, f)
	res, err := c.EnsureExtraDisk(t.Context(), "pve1", 101, "local-lvm", 200)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added || !strings.Contains(res.Existing, "vm-101-disk-1") {
		t.Fatalf("unexpected result %+v", res)
	}
	if len(f.posts) != 0 {
		t.Fatalf("an existing disk must not be modified, got POSTs %v", f.posts)
	}
}

func TestEnsureExtraDiskReportsPending(t *testing.T) {
	f := &fakePVE{cfg: map[string]any{"scsi0": "a"}, taskOK: true,
		pending: []map[string]any{{"key": "scsi1", "pending": "local-lvm:vm-101-disk-1"}}}
	c := newFake(t, f)
	res, err := c.EnsureExtraDisk(t.Context(), "pve1", 101, "local-lvm", 50)
	if err != nil || !res.Added || !res.Pending {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestEnsureExtraDiskSurfacesTaskFailure(t *testing.T) {
	f := &fakePVE{cfg: map[string]any{"scsi0": "a"}, taskOK: false}
	c := newFake(t, f)
	if _, err := c.EnsureExtraDisk(t.Context(), "pve1", 101, "nope", 50); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected the Proxmox failure to surface, got %v", err)
	}
}

func TestEnsureExtraDiskRejectsBadInputBeforeAnyCall(t *testing.T) {
	f := &fakePVE{cfg: map[string]any{}, taskOK: true}
	c := newFake(t, f)
	if _, err := c.EnsureExtraDisk(t.Context(), "pve1", 101, "bad,id", 50); err == nil {
		t.Fatal("expected validation error")
	}
	if len(f.posts) != 0 {
		t.Fatal("no request should be made for invalid input")
	}
}
