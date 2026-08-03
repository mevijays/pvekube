package ipplan

import "testing"

func TestValidate(t *testing.T) {
	// A correct plan should have zero issues (ping probes aside — 8.8.8.8-style
	// addresses used here are private/unreachable so probes should be quiet).
	p := Plan{
		Gateway: "10.10.10.1", PrefixLen: 24, DNSServers: []string{"8.8.8.8"},
		NodeIPRange: "10.10.10.50-10.10.10.60", ControlPlaneEndpoint: "10.10.10.10",
		MachineCount: 4,
	}
	issues := Validate(p)
	for _, i := range issues {
		if i.Severity == SeverityError {
			t.Errorf("unexpected error on valid plan: %s: %s", i.Field, i.Message)
		}
	}

	// VIP inside node range must be flagged.
	p2 := p
	p2.ControlPlaneEndpoint = "10.10.10.55"
	issues2 := Validate(p2)
	found := false
	for _, i := range issues2 {
		if i.Field == "control_plane_endpoint_ip" && i.Severity == SeverityError {
			found = true
		}
	}
	if !found {
		t.Error("expected error: VIP inside node range")
	}

	// Range too small for machine count.
	p3 := p
	p3.NodeIPRange = "10.10.10.50-10.10.10.51"
	p3.MachineCount = 5
	issues3 := Validate(p3)
	found = false
	for _, i := range issues3 {
		if i.Field == "node_ip_ranges" && i.Severity == SeverityError {
			found = true
		}
	}
	if !found {
		t.Error("expected error: range too small")
	}

	// VIP outside subnet.
	p4 := p
	p4.ControlPlaneEndpoint = "192.168.1.1"
	issues4 := Validate(p4)
	found = false
	for _, i := range issues4 {
		if i.Field == "control_plane_endpoint_ip" && i.Severity == SeverityError {
			found = true
		}
	}
	if !found {
		t.Error("expected error: VIP outside subnet")
	}
}

func TestParseRange(t *testing.T) {
	if _, _, err := ParseRange("not-a-range-at-all"); err == nil {
		t.Error("expected error for garbage input")
	}
	s, e, err := ParseRange("[10.0.0.5-10.0.0.10]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.String() != "10.0.0.5" || e.String() != "10.0.0.10" {
		t.Errorf("got %s-%s", s, e)
	}
	if _, _, err := ParseRange("10.0.0.10-10.0.0.5"); err == nil {
		t.Error("expected error: start after end")
	}
}

func TestCAPMOXRangeSyntax(t *testing.T) {
	if got := CAPMOXRangeSyntax("10.0.0.5-10.0.0.10"); got != "[10.0.0.5-10.0.0.10]" {
		t.Errorf("got %q", got)
	}
	if got := CAPMOXRangeSyntax("[10.0.0.5-10.0.0.10]"); got != "[10.0.0.5-10.0.0.10]" {
		t.Errorf("got %q", got)
	}
}
