// Private DNS for the workload cluster's CoreDNS.
//
// Nodes and pods resolve names through two completely separate paths, and
// configuring one does nothing for the other. PVEKube already sets the node
// path: ProxmoxCluster.spec.dnsServers (GenerateInput.DNSServers) becomes
// the VM's own resolvers, which is what containerd uses to pull images. Pods
// never see that list. They ask CoreDNS, and kubeadm's stock Corefile ends
// with:
//
//	forward . /etc/resolv.conf
//
// which hands every non-cluster name to whatever resolvers the node happens
// to have — on a systemd-resolved node that is /run/systemd/resolve/
// resolv.conf, verified live on a devkafka worker as:
//
//	nameserver 172.16.1.1
//	nameserver 1.1.1.1
//
// The forward plugin's default upstream policy is *random*, not "first
// working one". With one internal resolver and one public resolver in that
// list, roughly half of all lookups for an internal name are answered
// NXDOMAIN by the public resolver — and NXDOMAIN is a successful, cacheable
// answer, so nothing retries. That is not a hypothetical: it is exactly the
// failure that made ArgoCD's OIDC login bounce back to the login screen
// ~50% of the time against auth.mylab.lan, and it is why anything pulling
// from an internal Git host (Flux) or an internal registry over the pod
// network fails intermittently rather than cleanly.
//
// The fix is a per-domain server block, which CoreDNS matches ahead of the
// catch-all "." block, sending those names to the internal resolver only:
//
//	mylab.lan:53 {
//	    forward . 172.16.1.1
//	}
//
// This file writes that block into the coredns ConfigMap between markers so
// re-running is idempotent and an operator's own edits elsewhere in the
// Corefile survive untouched.
package capi

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"pvekube/internal/jobs"
	"pvekube/internal/runner"
)

const (
	// Markers delimit the block this file owns. Everything between them is
	// replaced wholesale on each run; everything outside is never touched,
	// so hand-added zones or plugin tweaks elsewhere in the Corefile
	// survive. CoreDNS treats "#" to end-of-line as a comment.
	privateDNSBeginMarker = "# BEGIN pvekube private DNS (managed — edits between these markers are overwritten)"
	privateDNSEndMarker   = "# END pvekube private DNS"

	corefileConfigMapName = "coredns"
	corefileNamespace     = "kube-system"
	corefileKey           = "Corefile"
)

// PrivateDNSConfig routes a set of internal domains to a set of internal
// resolvers. Zero value means "leave CoreDNS exactly as kubeadm wrote it".
type PrivateDNSConfig struct {
	// Domains are DNS suffixes served by the internal resolvers, e.g.
	// "mylab.lan". A query for anything at or below one of these lands in
	// that domain's server block.
	Domains []string
	// Servers are the resolvers to forward those domains to. Deliberately
	// separate from GenerateInput.DNSServers: that list is the nodes' own
	// resolvers and legitimately includes a public fallback, which is the
	// very thing that must not appear here.
	Servers []string
}

func (p PrivateDNSConfig) Enabled() bool { return len(p.Domains) > 0 }

// ParseDNSList splits a form field that may use commas, whitespace or
// newlines as separators, dropping empties. Operators type these lists by
// hand and are inconsistent about separators; accepting all three costs
// nothing and avoids a class of "it silently did nothing" bug reports.
func ParseDNSList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// Validate rejects input that would produce a Corefile CoreDNS refuses to
// load. A bad Corefile does not fail loudly at patch time — CoreDNS
// crash-loops afterwards and every name in the cluster stops resolving —
// so this is checked before the ConfigMap is written, not after.
func (p PrivateDNSConfig) Validate() error {
	if !p.Enabled() {
		if len(p.Servers) > 0 {
			return fmt.Errorf("private DNS servers were given but no private domain — add the domain (for example mylab.lan) or clear the servers")
		}
		return nil
	}
	if len(p.Servers) == 0 {
		return fmt.Errorf("private DNS domains were given but no resolver to send them to — add the internal DNS server's IP address")
	}
	for _, d := range p.Domains {
		if err := validateDNSDomain(d); err != nil {
			return err
		}
	}
	for _, s := range p.Servers {
		if err := validateDNSServer(s); err != nil {
			return err
		}
	}
	return nil
}

func validateDNSDomain(d string) error {
	trimmed := strings.TrimSuffix(d, ".")
	if trimmed == "" {
		return fmt.Errorf("private DNS domain is empty")
	}
	// A bare "." would replace the catch-all forward and blackhole every
	// external name, and "cluster.local" would shadow the kubernetes plugin
	// so no Service would resolve. Both are silent, total outages.
	if trimmed == "cluster.local" || strings.HasSuffix(trimmed, ".cluster.local") {
		return fmt.Errorf("private DNS domain %q would shadow Kubernetes' own cluster.local zone and stop Services resolving", d)
	}
	if strings.ContainsAny(trimmed, " \t{}#") {
		return fmt.Errorf("private DNS domain %q contains a character that is not valid in a domain name", d)
	}
	for _, label := range strings.Split(trimmed, ".") {
		if label == "" {
			return fmt.Errorf("private DNS domain %q has an empty label", d)
		}
	}
	return nil
}

func validateDNSServer(s string) error {
	host := s
	// Allow an explicit port, which CoreDNS accepts as host:port.
	if h, _, err := net.SplitHostPort(s); err == nil {
		host = h
	}
	if net.ParseIP(host) == nil {
		return fmt.Errorf("private DNS server %q is not a valid IP address — CoreDNS forwards to addresses, not names (a name here could not be resolved without the very DNS this is configuring)", s)
	}
	return nil
}

// privateDNSBlock renders the managed section, or "" when disabled.
func privateDNSBlock(p PrivateDNSConfig) string {
	if !p.Enabled() {
		return ""
	}
	var b strings.Builder
	b.WriteString(privateDNSBeginMarker)
	b.WriteString("\n")
	for _, d := range p.Domains {
		zone := strings.TrimSuffix(d, ".")
		fmt.Fprintf(&b, "%s:53 {\n", zone)
		b.WriteString("    errors\n")
		// Without an explicit cache the zone gets none: the "cache" in the
		// stock "." block belongs to that block only.
		b.WriteString("    cache 30\n")
		fmt.Fprintf(&b, "    forward . %s\n", strings.Join(p.Servers, " "))
		b.WriteString("}\n")
	}
	b.WriteString(privateDNSEndMarker)
	return b.String()
}

// applyPrivateDNSToCorefile returns corefile with the managed block replaced
// by p's (removed entirely when p is disabled). Pure and total: any input
// that has been through it once is unchanged by running it again, which is
// what makes the step safe to re-run on every apply and after an upgrade.
func applyPrivateDNSToCorefile(corefile string, p PrivateDNSConfig) string {
	stripped := stripPrivateDNSBlock(corefile)
	block := privateDNSBlock(p)
	if block == "" {
		return stripped
	}
	if strings.TrimSpace(stripped) == "" {
		return block + "\n"
	}
	return strings.TrimRight(stripped, "\n") + "\n\n" + block + "\n"
}

// stripPrivateDNSBlock removes every marked block. Loops rather than
// handling a single occurrence so a Corefile that somehow accumulated two
// (an interrupted write, a hand-edited copy) converges to one instead of
// keeping a stale zone forever.
func stripPrivateDNSBlock(corefile string) string {
	out := corefile
	for {
		start := strings.Index(out, privateDNSBeginMarker)
		if start < 0 {
			break
		}
		end := strings.Index(out[start:], privateDNSEndMarker)
		if end < 0 {
			// Truncated block with no terminator: drop from the marker on,
			// rather than leaving a half block that breaks the Corefile.
			out = out[:start]
			break
		}
		tail := start + end + len(privateDNSEndMarker)
		// Swallow the newline the block ends with so repeated runs don't
		// grow a stack of blank lines.
		if tail < len(out) && out[tail] == '\n' {
			tail++
		}
		out = out[:start] + out[tail:]
	}
	return strings.TrimRight(out, " \t\n") + "\n"
}

// ConfigurePrivateDNSStep points the workload cluster's CoreDNS at the
// internal resolvers for the operator's internal domains.
//
// Ordering matters and is asserted by the caller, not here: this must run
// before any addon that talks to an internal host from inside the pod
// network (Flux against an internal Git server, most obviously), because
// those resolve through CoreDNS and would otherwise hit the random-upstream
// coin flip described at the top of this file.
func ConfigurePrivateDNSStep(dataDir, binDir, clusterName string, p PrivateDNSConfig) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		if !p.Enabled() {
			c.Logf("No private DNS domains configured — leaving CoreDNS as kubeadm wrote it")
			return nil
		}
		kcPath, cleanup, err := waitForWorkloadKubeconfig(c, dataDir, binDir, clusterName, "configuring private DNS")
		if err != nil {
			return err
		}
		defer cleanup()

		kubectlBin := filepath.Join(binDir, "kubectl")
		current, err := kubectlOut(c, kubectlBin, "--kubeconfig", kcPath,
			"get", "configmap", corefileConfigMapName, "-n", corefileNamespace,
			"-o", "jsonpath={.data."+corefileKey+"}")
		if err != nil {
			return fmt.Errorf("reading the cluster's CoreDNS configuration: %w", err)
		}
		if strings.TrimSpace(current) == "" {
			return fmt.Errorf("the cluster's coredns ConfigMap has no %s entry — refusing to write one from scratch, since that would discard whatever CoreDNS is actually running on", corefileKey)
		}

		updated := applyPrivateDNSToCorefile(current, p)
		c.Logf("Routing %s to %s via a CoreDNS server block",
			strings.Join(p.Domains, ", "), strings.Join(p.Servers, ", "))
		if updated == current {
			c.Logf("CoreDNS already has exactly this configuration — nothing to change")
			return nil
		}

		// A merge patch carries the Corefile as a single JSON string, so
		// newlines and quoting survive without shell or YAML escaping.
		payload, err := json.Marshal(map[string]any{
			"data": map[string]string{corefileKey: updated},
		})
		if err != nil {
			return fmt.Errorf("building the CoreDNS patch: %w", err)
		}
		if _, err := kubectlOut(c, kubectlBin, "--kubeconfig", kcPath,
			"patch", "configmap", corefileConfigMapName, "-n", corefileNamespace,
			"--type=merge", "-p", string(payload)); err != nil {
			return fmt.Errorf("updating the CoreDNS configuration: %w", err)
		}
		c.Logf("coredns ConfigMap updated")

		// CoreDNS's reload plugin only notices once kubelet propagates the
		// ConfigMap to the pod's volume, which can take a minute or more.
		// Restarting makes the change effective before this step reports
		// success, so a later step that depends on internal DNS is not
		// racing an unfinished rollout.
		c.Logf("Restarting CoreDNS so the new configuration takes effect now rather than on kubelet's next ConfigMap sync")
		if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
			"rollout", "restart", "deployment/coredns", "-n", corefileNamespace); err != nil {
			return fmt.Errorf("restarting CoreDNS: %w", err)
		}
		if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
			"rollout", "status", "deployment/coredns", "-n", corefileNamespace, "--timeout=180s"); err != nil {
			return fmt.Errorf("waiting for CoreDNS to come back after the restart: %w", err)
		}
		c.Logf("✓ CoreDNS is serving %s from %s", strings.Join(p.Domains, ", "), strings.Join(p.Servers, ", "))
		return nil
	}
}
