// Package ipplan validates the network plan for a workload cluster before
// it's handed to CAPMOX. CAPMOX has no DHCP path for nodes — every mistake
// here (VIP inside the node range, range too small, wrong subnet) becomes a
// cluster that hangs at "provisioning" with no obvious error, so this
// validation is arithmetic-first and runs before anything touches Proxmox.
package ipplan

import (
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type Severity string

const (
	SeverityError Severity = "error"
	SeverityWarn  Severity = "warn"
)

type Issue struct {
	Field    string
	Severity Severity
	Message  string
}

// Plan is the network configuration a cluster designer form collects,
// mirroring exactly the CAPMOX variables that consume it (see
// docs/Usage.md: CONTROL_PLANE_ENDPOINT_IP, NODE_IP_RANGES, GATEWAY,
// IP_PREFIX, DNS_SERVERS).
type Plan struct {
	Gateway              string
	PrefixLen            int
	DNSServers           []string
	NodeIPRange          string // "start-end", e.g. "10.10.10.5-10.10.10.50"
	ControlPlaneEndpoint string
	MachineCount         int // control plane + worker count, for range-size check
}

// Validate runs every arithmetic check that can be done without touching
// the network, plus a live reachability probe (see probeLive) on the two
// addresses that matter most: the gateway and the proposed VIP. A full
// sweep of the node range is deliberately NOT done here — pinging
// potentially hundreds of addresses would make every form edit slow: the
// two addresses checked are the ones a collision with is fatal.
func Validate(p Plan) []Issue {
	var issues []Issue
	add := func(field string, sev Severity, format string, args ...any) {
		issues = append(issues, Issue{Field: field, Severity: sev, Message: fmt.Sprintf(format, args...)})
	}

	gw := net.ParseIP(p.Gateway)
	if gw == nil {
		add("gateway", SeverityError, "%q is not a valid IP address", p.Gateway)
	}
	if p.PrefixLen < 1 || p.PrefixLen > 30 {
		add("ip_prefix", SeverityError, "prefix /%d is out of range (expected 1-30)", p.PrefixLen)
	}

	var network *net.IPNet
	if gw != nil && p.PrefixLen >= 1 && p.PrefixLen <= 30 {
		_, network, _ = net.ParseCIDR(fmt.Sprintf("%s/%d", gw.String(), p.PrefixLen))
	}

	vip := net.ParseIP(p.ControlPlaneEndpoint)
	if p.ControlPlaneEndpoint == "" {
		add("control_plane_endpoint_ip", SeverityError, "control plane endpoint IP is required — kube-vip needs it")
	} else if vip == nil {
		add("control_plane_endpoint_ip", SeverityError, "%q is not a valid IP address", p.ControlPlaneEndpoint)
	} else if network != nil && !network.Contains(vip) {
		add("control_plane_endpoint_ip", SeverityError, "%s is not inside %s — it must be on the same subnet as the control plane machines", vip, network)
	}

	start, end, err := ParseRange(p.NodeIPRange)
	if err != nil {
		add("node_ip_ranges", SeverityError, "%v", err)
	} else {
		if network != nil {
			if !network.Contains(start) || !network.Contains(end) {
				add("node_ip_ranges", SeverityError, "range %s is not entirely inside subnet %s", p.NodeIPRange, network)
			}
		}
		size := rangeSize(start, end)
		if p.MachineCount > 0 && size < p.MachineCount {
			add("node_ip_ranges", SeverityError, "range has %d addresses but the cluster needs %d (control plane + workers)", size, p.MachineCount)
		}
		if vip != nil && ipInRange(vip, start, end) {
			add("control_plane_endpoint_ip", SeverityError, "%s falls inside the node range %s — pick an address outside it", vip, p.NodeIPRange)
		}
		if gw != nil && ipInRange(gw, start, end) {
			add("node_ip_ranges", SeverityError, "gateway %s falls inside the node range — pick a range that excludes it", gw)
		}
	}

	if len(p.DNSServers) == 0 {
		add("dns_servers", SeverityWarn, "no DNS servers set — node name resolution may fail")
	}
	for _, d := range p.DNSServers {
		if net.ParseIP(strings.TrimSpace(d)) == nil {
			add("dns_servers", SeverityError, "%q is not a valid IP address", d)
		}
	}

	// Live checks — cheap, bounded, and only for the two addresses whose
	// occupancy would actually break the cluster (a used node-range address
	// just fails one machine; a used VIP or gateway breaks everything).
	// Run in parallel so a validate click waits on one probe timeout, not two.
	type probeResult struct{ occupied, checked bool }
	gwCh, vipCh := make(chan probeResult, 1), make(chan probeResult, 1)
	go func() {
		if gw == nil {
			gwCh <- probeResult{}
			return
		}
		o, c := probeLive(gw.String())
		gwCh <- probeResult{o, c}
	}()
	go func() {
		if vip == nil {
			vipCh <- probeResult{}
			return
		}
		o, c := probeLive(vip.String())
		vipCh <- probeResult{o, c}
	}()
	if r := <-gwCh; r.checked && r.occupied {
		add("gateway", SeverityWarn, "%s responded to a ping — expected for a gateway, but confirm this is really your router", gw)
	}
	if r := <-vipCh; r.checked && r.occupied {
		add("control_plane_endpoint_ip", SeverityError, "%s is already responding on the network — pick a free address, this one will conflict with kube-vip", vip)
	}

	return issues
}

// ParseRange parses "10.10.10.5-10.10.10.50" into start/end IPs.
func ParseRange(s string) (net.IP, net.IP, error) {
	s = strings.TrimSpace(strings.Trim(s, "[]"))
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("%q must look like start-end, e.g. 10.10.10.5-10.10.10.50", s)
	}
	start := net.ParseIP(strings.TrimSpace(parts[0])).To4()
	end := net.ParseIP(strings.TrimSpace(parts[1])).To4()
	if start == nil || end == nil {
		return nil, nil, fmt.Errorf("%q contains an invalid IP address", s)
	}
	if ipToUint32(start) > ipToUint32(end) {
		return nil, nil, fmt.Errorf("range start %s is after end %s", start, end)
	}
	return start, end, nil
}

// CAPMOXRangeSyntax formats a range the way CAPMOX's cluster templates
// expect it substituted: a bracketed YAML flow-sequence, e.g.
// "[10.10.10.5-10.10.10.50]" (verified against the real cluster-template.yaml
// — NODE_IP_RANGES is substituted directly into `addresses: ${NODE_IP_RANGES}`).
func CAPMOXRangeSyntax(rangeStr string) string {
	r := strings.TrimSpace(rangeStr)
	if !strings.HasPrefix(r, "[") {
		r = "[" + r + "]"
	}
	return r
}

func rangeSize(start, end net.IP) int {
	return int(ipToUint32(end)-ipToUint32(start)) + 1
}

func ipInRange(ip, start, end net.IP) bool {
	v := ipToUint32(ip.To4())
	return v >= ipToUint32(start) && v <= ipToUint32(end)
}

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

// probeLive shells out to the system `ping` (a single, 1-second-bounded
// probe) rather than opening a raw ICMP socket, which would need elevated
// privileges on most OSes. checked=false means the probe itself couldn't
// run (e.g. no ping binary) — callers should treat that as "unknown", not
// "free".
func probeLive(ip string) (occupied bool, checked bool) {
	path, err := exec.LookPath("ping")
	if err != nil {
		return false, false
	}
	args := pingArgs(ip)
	cmd := exec.Command(path, args...)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return false, false
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err == nil, true
	case <-time.After(2 * time.Second):
		cmd.Process.Kill()
		return false, true
	}
}

func pingArgs(ip string) []string {
	// -W's unit differs by OS: BSD/macOS takes milliseconds, Linux (iputils)
	// takes seconds. Using the Linux value (1) on macOS would time out
	// after 1ms — effectively never getting a reply — so this branches
	// rather than picking one value for both.
	if runtime.GOOS == "darwin" {
		return []string{"-c", "1", "-W", "1000", ip}
	}
	return []string{"-c", "1", "-W", "1", ip}
}
