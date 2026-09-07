package capi

import (
	"strings"
	"testing"
)

// stockCorefile is the real Corefile kubeadm wrote for the devkafka cluster,
// read straight off the running cluster. Using the genuine article rather
// than a hand-typed approximation is what makes the "everything else is
// preserved" assertions below mean anything.
const stockCorefile = `.:53 {
    errors
    health {
       lameduck 5s
    }
    ready
    kubernetes cluster.local in-addr.arpa ip6.arpa {
       pods insecure
       fallthrough in-addr.arpa ip6.arpa
       ttl 30
    }
    prometheus :9153
    forward . /etc/resolv.conf {
       max_concurrent 1000
    }
    cache 30 {
       disable success cluster.local
       disable denial cluster.local
    }
    loop
    reload
    loadbalance
}
`

func TestParseDNSListAcceptsMixedSeparators(t *testing.T) {
	got := ParseDNSList(" mylab.lan, internal.example ;  lab.test\nfoo.bar\t")
	want := []string{"mylab.lan", "internal.example", "lab.test", "foo.bar"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if n := len(ParseDNSList("   ,,  ; ")); n != 0 {
		t.Errorf("a separator-only string should yield no entries, got %d", n)
	}
}

func TestPrivateDNSValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     PrivateDNSConfig
		wantErr string
	}{
		{"disabled is fine", PrivateDNSConfig{}, ""},
		{"valid", PrivateDNSConfig{Domains: []string{"mylab.lan"}, Servers: []string{"172.16.1.1"}}, ""},
		{"trailing dot is fine", PrivateDNSConfig{Domains: []string{"mylab.lan."}, Servers: []string{"172.16.1.1"}}, ""},
		{"server with port", PrivateDNSConfig{Domains: []string{"mylab.lan"}, Servers: []string{"172.16.1.1:5353"}}, ""},
		{"domain without server", PrivateDNSConfig{Domains: []string{"mylab.lan"}}, "no resolver"},
		{"server without domain", PrivateDNSConfig{Servers: []string{"172.16.1.1"}}, "no private domain"},
		{"server is a name", PrivateDNSConfig{Domains: []string{"mylab.lan"}, Servers: []string{"dns.mylab.lan"}}, "not a valid IP"},
		// The two total-outage cases: both load fine and break everything.
		{"cluster.local", PrivateDNSConfig{Domains: []string{"cluster.local"}, Servers: []string{"172.16.1.1"}}, "shadow"},
		{"sub of cluster.local", PrivateDNSConfig{Domains: []string{"svc.cluster.local"}, Servers: []string{"172.16.1.1"}}, "shadow"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestApplyPrivateDNSAddsZoneAndKeepsStockBlock(t *testing.T) {
	cfg := PrivateDNSConfig{Domains: []string{"mylab.lan"}, Servers: []string{"172.16.1.1"}}
	got := applyPrivateDNSToCorefile(stockCorefile, cfg)

	if !strings.Contains(got, "mylab.lan:53 {") {
		t.Errorf("no server block for the private domain:\n%s", got)
	}
	if !strings.Contains(got, "forward . 172.16.1.1") {
		t.Errorf("private zone does not forward to the internal resolver:\n%s", got)
	}
	// The catch-all block must survive verbatim — losing it would take out
	// every external name and the kubernetes plugin along with it.
	if !strings.Contains(got, "kubernetes cluster.local in-addr.arpa ip6.arpa {") {
		t.Errorf("the stock .:53 block was damaged:\n%s", got)
	}
	if !strings.Contains(got, "forward . /etc/resolv.conf {") {
		t.Errorf("the stock catch-all forward was removed:\n%s", got)
	}
	// The private zone has to be its own block, not spliced into ".:53".
	if strings.Index(got, "mylab.lan:53 {") < strings.Index(got, "loadbalance") {
		t.Errorf("private zone landed inside the stock block:\n%s", got)
	}
}

// Idempotency is the property the whole marker scheme exists for: this step
// runs on every apply and again after an upgrade, so a second run must be a
// no-op rather than stacking another copy of the zone.
func TestApplyPrivateDNSIsIdempotent(t *testing.T) {
	cfg := PrivateDNSConfig{Domains: []string{"mylab.lan"}, Servers: []string{"172.16.1.1"}}
	once := applyPrivateDNSToCorefile(stockCorefile, cfg)
	twice := applyPrivateDNSToCorefile(once, cfg)
	if once != twice {
		t.Fatalf("second application changed the Corefile.\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	if n := strings.Count(twice, "mylab.lan:53 {"); n != 1 {
		t.Fatalf("expected exactly 1 private zone after two runs, got %d:\n%s", n, twice)
	}
	if n := strings.Count(twice, privateDNSBeginMarker); n != 1 {
		t.Fatalf("expected exactly 1 managed block, got %d", n)
	}
}

// Changing the configuration must replace the old zone, not add a second
// one — otherwise CoreDNS keeps answering from the previous resolver.
func TestApplyPrivateDNSReplacesPreviousConfiguration(t *testing.T) {
	first := applyPrivateDNSToCorefile(stockCorefile,
		PrivateDNSConfig{Domains: []string{"old.lan"}, Servers: []string{"10.0.0.1"}})
	second := applyPrivateDNSToCorefile(first,
		PrivateDNSConfig{Domains: []string{"mylab.lan"}, Servers: []string{"172.16.1.1"}})

	if strings.Contains(second, "old.lan") || strings.Contains(second, "10.0.0.1") {
		t.Fatalf("the previous private zone survived:\n%s", second)
	}
	if !strings.Contains(second, "mylab.lan:53 {") {
		t.Fatalf("the new private zone is missing:\n%s", second)
	}
}

func TestApplyPrivateDNSDisabledRemovesTheBlock(t *testing.T) {
	with := applyPrivateDNSToCorefile(stockCorefile,
		PrivateDNSConfig{Domains: []string{"mylab.lan"}, Servers: []string{"172.16.1.1"}})
	without := applyPrivateDNSToCorefile(with, PrivateDNSConfig{})

	if strings.Contains(without, "mylab.lan") || strings.Contains(without, privateDNSBeginMarker) {
		t.Fatalf("disabling did not remove the managed block:\n%s", without)
	}
	if strings.TrimSpace(without) != strings.TrimSpace(stockCorefile) {
		t.Fatalf("removing the block did not restore the original Corefile.\n--- got ---\n%s\n--- want ---\n%s", without, stockCorefile)
	}
}

// An operator's own additions live outside the markers and must be left
// alone — this is the difference between "manages a block" and "owns the
// file".
func TestApplyPrivateDNSPreservesHandWrittenZones(t *testing.T) {
	handEdited := stockCorefile + `
example.internal:53 {
    forward . 10.9.9.9
}
`
	got := applyPrivateDNSToCorefile(handEdited,
		PrivateDNSConfig{Domains: []string{"mylab.lan"}, Servers: []string{"172.16.1.1"}})
	if !strings.Contains(got, "example.internal:53 {") || !strings.Contains(got, "forward . 10.9.9.9") {
		t.Fatalf("a hand-written zone outside the markers was lost:\n%s", got)
	}

	// ...and still survives when the managed block is later removed.
	cleared := applyPrivateDNSToCorefile(got, PrivateDNSConfig{})
	if !strings.Contains(cleared, "example.internal:53 {") {
		t.Fatalf("hand-written zone lost when the managed block was removed:\n%s", cleared)
	}
}

func TestApplyPrivateDNSMultipleDomainsAndServers(t *testing.T) {
	got := applyPrivateDNSToCorefile(stockCorefile, PrivateDNSConfig{
		Domains: []string{"mylab.lan", "corp.internal"},
		Servers: []string{"172.16.1.1", "172.16.1.2"},
	})
	for _, want := range []string{"mylab.lan:53 {", "corp.internal:53 {", "forward . 172.16.1.1 172.16.1.2"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// A block truncated by an interrupted write must not leave a dangling "{"
// that stops CoreDNS loading the file entirely.
func TestStripPrivateDNSBlockHandlesTruncatedBlock(t *testing.T) {
	broken := stockCorefile + "\n" + privateDNSBeginMarker + "\nmylab.lan:53 {\n    forward . 172.16.1.1\n"
	got := stripPrivateDNSBlock(broken)
	if strings.Contains(got, "mylab.lan") || strings.Contains(got, privateDNSBeginMarker) {
		t.Fatalf("truncated block was not fully removed:\n%s", got)
	}
	if !strings.Contains(got, "loadbalance") {
		t.Fatalf("stripping a truncated block damaged the stock Corefile:\n%s", got)
	}
}

func TestStripPrivateDNSBlockCollapsesDuplicates(t *testing.T) {
	cfg := PrivateDNSConfig{Domains: []string{"mylab.lan"}, Servers: []string{"172.16.1.1"}}
	block := privateDNSBlock(cfg)
	doubled := stockCorefile + "\n" + block + "\n" + block + "\n"
	got := applyPrivateDNSToCorefile(doubled, cfg)
	if n := strings.Count(got, privateDNSBeginMarker); n != 1 {
		t.Fatalf("expected duplicates to collapse to 1 block, got %d:\n%s", n, got)
	}
}
