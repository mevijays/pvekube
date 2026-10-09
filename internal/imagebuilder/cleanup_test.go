package imagebuilder

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeProxmox serves just the endpoints CleanupBuild touches, and records the
// mutating calls so a test can assert what was (and was not) destroyed.
type fakeProxmox struct {
	mu       sync.Mutex
	exists   bool
	running  bool
	template bool
	calls    []string
}

func (f *fakeProxmox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	reply := func(data string) { fmt.Fprintf(w, `{"data":%s}`, data) }
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/status/current"):
		if !f.exists {
			http.Error(w, "Configuration file 'nodes/pve2/qemu-server/100.conf' does not exist", http.StatusInternalServerError)
			return
		}
		st := "stopped"
		if f.running {
			st = "running"
		}
		reply(fmt.Sprintf(`{"status":%q,"name":"capmox-build"}`, st))
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/config"):
		if f.template {
			reply(`{"template":1}`)
		} else {
			reply(`{}`)
		}
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/status/stop"):
		f.calls = append(f.calls, "stop")
		f.running = false
		reply(`"UPID:stop"`)
	case r.Method == http.MethodDelete:
		f.calls = append(f.calls, "delete")
		if f.running {
			http.Error(w, "VM is running", http.StatusInternalServerError)
			return
		}
		f.exists = false
		reply(`"UPID:delete"`)
	case strings.Contains(path, "/tasks/"):
		reply(`{"status":"stopped","exitstatus":"OK"}`)
	default:
		http.NotFound(w, r)
	}
}

func cleanupEnv(srv *httptest.Server) ConnEnv {
	return ConnEnv{URL: srv.URL, TokenID: "t@pve!x", Secret: "s", Node: "pve2"}
}

func TestCleanupBuildStopsAndDeletesRunningBuildVM(t *testing.T) {
	f := &fakeProxmox{exists: true, running: true}
	srv := httptest.NewServer(f)
	defer srv.Close()

	var log []string
	if err := CleanupBuild(func(format string, a ...any) { log = append(log, fmt.Sprintf(format, a...)) }, cleanupEnv(srv), 100); err != nil {
		t.Fatalf("CleanupBuild: %v\n%v", err, log)
	}
	// Proxmox refuses to delete a running VM, so the stop must come first.
	if got := strings.Join(f.calls, ","); got != "stop,delete" {
		t.Errorf("calls = %s, want stop,delete", got)
	}
	if f.exists {
		t.Error("VM still exists after clean-up")
	}
}

// A VM already converted to a template is the build's finished output (e.g.
// only "Record template" failed afterwards) and must never be destroyed.
func TestCleanupBuildLeavesFinishedTemplateAlone(t *testing.T) {
	f := &fakeProxmox{exists: true, template: true}
	srv := httptest.NewServer(f)
	defer srv.Close()

	if err := CleanupBuild(func(string, ...any) {}, cleanupEnv(srv), 100); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 || !f.exists {
		t.Errorf("template was touched: calls=%v exists=%v", f.calls, f.exists)
	}
}

func TestCleanupBuildIsANoOpWhenNothingIsLeft(t *testing.T) {
	f := &fakeProxmox{}
	srv := httptest.NewServer(f)
	defer srv.Close()

	if err := CleanupBuild(func(string, ...any) {}, cleanupEnv(srv), 100); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("unexpected calls: %v", f.calls)
	}
}

// An unreachable Proxmox must be reported as a failure, never as "cleaned".
func TestCleanupBuildReportsProxmoxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	if err := CleanupBuild(func(string, ...any) {}, cleanupEnv(srv), 100); err == nil {
		t.Fatal("expected an error when Proxmox cannot be queried")
	}
}
