package proxmox

import (
	"context"
)

// PermCheck reports whether the configured token can actually do one thing
// PVEKube needs, rather than trusting that "PVEVMAdmin" was set up
// correctly. Read-level checks (Sys.Audit, Datastore.Audit, VM.Allocate)
// are verified live; VM.Clone/VM.Config.* (needed by CAPMOX to actually
// clone and configure machines) can't be probed without side effects, so
// those are declared and the exact remediation command is shown instead.
type PermCheck struct {
	Name       string `json:"name"`
	Privilege  string `json:"privilege"`
	OK         bool   `json:"ok"`
	Detail     string `json:"detail"`
	FixCommand string `json:"fix_command,omitempty"`
	Probed     bool   `json:"probed"` // false = declared/assumed, not live-tested
}

// fixCommand builds the exact pveum command for a role grant against this
// token's own user (not a placeholder) so it's copy-paste ready. Proxmox API
// tokens default to privilege separation OFF (privsep=0 at creation time via
// the web UI's "Privilege Separation" checkbox unticked), in which case
// granting the underlying user is enough and the token inherits it
// automatically — but if privsep is ON, the same grant must be repeated
// against the token principal itself ("user@realm!tokenname"). We can't
// probe privsep without an extra API call per check, so the fix command
// always targets the user; the setup-instructions panel calls this out
// explicitly for anyone who enabled privsep.
func (c *Client) fixCommand(path, role string) string {
	if path == "" || path == "/" {
		return "pveum acl modify / -user " + c.TokenUser() + " -role " + role
	}
	return "pveum acl modify " + path + " -user " + c.TokenUser() + " -role " + role
}

// VerifyPermissions probes each capability PVEKube needs and reports pass/fail
// with the exact pveum command to run if something is missing. It never
// stops at the first failure — a non-technical user needs the whole list.
func (c *Client) VerifyPermissions(ctx context.Context) []PermCheck {
	var checks []PermCheck

	if _, err := c.Version(ctx); err != nil {
		checks = append(checks, PermCheck{
			Name: "Authentication", Privilege: "(valid token)", OK: false, Probed: true,
			Detail: "Could not authenticate at all: " + err.Error(),
		})
		// Nothing else will work without a valid token; stop here.
		return checks
	}
	checks = append(checks, PermCheck{Name: "Authentication", Privilege: "(valid token)", OK: true, Probed: true, Detail: "Token accepted."})

	fixAuditor := c.fixCommand("/", "PVEAuditor")
	fixVMAdmin := c.fixCommand("/", "PVEVMAdmin")
	fixDatastoreAdmin := c.fixCommand("/", "PVEDatastoreAdmin")
	fixSDN := c.fixCommand("/sdn", "PVEAdmin")

	if _, err := c.nodes(ctx); err != nil {
		checks = append(checks, PermCheck{
			Name: "List cluster nodes", Privilege: "Sys.Audit", OK: false, Probed: true,
			Detail: describeAuthErr(err), FixCommand: fixAuditor,
		})
	} else {
		checks = append(checks, PermCheck{Name: "List cluster nodes", Privilege: "Sys.Audit", OK: true, Probed: true, Detail: "OK"})
	}

	if _, err := c.storage(ctx); err != nil {
		checks = append(checks, PermCheck{
			Name: "List storage pools", Privilege: "Datastore.Audit", OK: false, Probed: true,
			Detail: describeAuthErr(err), FixCommand: fixAuditor,
		})
	} else {
		checks = append(checks, PermCheck{Name: "List storage pools", Privilege: "Datastore.Audit", OK: true, Probed: true, Detail: "OK"})
	}

	nodeList, nodeErr := c.nodes(ctx)
	if nodeErr == nil && len(nodeList) > 0 {
		if _, err := c.bridges(ctx, nodeList[0].Name); err != nil {
			checks = append(checks, PermCheck{
				Name: "List network bridges", Privilege: "Sys.Audit", OK: false, Probed: true,
				Detail: describeAuthErr(err), FixCommand: fixAuditor,
			})
		} else {
			checks = append(checks, PermCheck{Name: "List network bridges", Privilege: "Sys.Audit", OK: true, Probed: true, Detail: "OK"})
		}
	}

	if _, err := c.nextVMID(ctx); err != nil {
		checks = append(checks, PermCheck{
			Name: "Allocate VM IDs", Privilege: "VM.Allocate", OK: false, Probed: true,
			Detail: describeAuthErr(err), FixCommand: fixVMAdmin,
		})
	} else {
		checks = append(checks, PermCheck{Name: "Allocate VM IDs", Privilege: "VM.Allocate", OK: true, Probed: true, Detail: "OK"})
	}

	// Everything below can't be probed without a real side effect (creating
	// a VM, allocating disk space, attaching to a network zone), so these
	// are declared based on expected role assignment rather than live-tested.
	// All three were missing permissions we hit one at a time against a real
	// cluster before landing on this exact set — listing them up front here
	// means a user sees every gap at once instead of discovering them
	// one failed build at a time.
	checks = append(checks, PermCheck{
		Name: "Create, clone, and configure VMs", Privilege: "PVEVMAdmin role",
		OK: true, Probed: false, FixCommand: fixVMAdmin,
		Detail: "Not live-tested (would require creating a VM) — assumed present. If template builds or cluster launches fail with a 403 on VM creation, run the fix command.",
	})
	checks = append(checks, PermCheck{
		Name: "Allocate disk space for VM disks and ISOs", Privilege: "Datastore.AllocateSpace",
		OK: true, Probed: false, FixCommand: fixDatastoreAdmin,
		Detail: "Not live-tested — assumed present. PVEVMAdmin does NOT include this; without it, VM creation fails with \"Permission check failed (/storage/<pool>, Datastore.AllocateSpace)\".",
	})
	checks = append(checks, PermCheck{
		Name: "Attach VMs to network bridges/SDN zones", Privilege: "SDN.Use",
		OK: true, Probed: false, FixCommand: fixSDN,
		Detail: "Not live-tested — assumed present. Only required on Proxmox installs using SDN zones (common on 8.x/9.x default installs); without it, VM creation fails with \"Permission check failed (/sdn/zones/.../<bridge>, SDN.Use)\".",
	})

	// This one IS live-probeable and catches a failure mode with no error
	// message at all: a datacenter-wide firewall with no matching rule for
	// the actual build network silently drops a build VM's outbound HTTP
	// callback to Packer's autoinstall server. The VM just sits on its
	// installer's interactive language-selection screen forever — no 403,
	// no timeout message pointing at the cause, nothing — until Packer's own
	// 2-hour ssh_timeout eventually gives up. Found and fixed live against a
	// real cluster; worth surfacing before anyone else loses hours to it.
	fwEnabled, fwErr := c.ClusterFirewallEnabled(ctx)
	switch {
	case fwErr != nil:
		checks = append(checks, PermCheck{
			Name: "Datacenter firewall not silently blocking builds", Privilege: "(informational)",
			OK: true, Probed: false,
			Detail: "Could not check /cluster/firewall/options (" + fwErr.Error() + ") — not fatal, just unverified.",
		})
	case fwEnabled:
		checks = append(checks, PermCheck{
			Name: "Datacenter firewall not silently blocking builds", Privilege: "(informational)",
			OK: false, Probed: true,
			Detail: "Proxmox's datacenter-wide firewall is ENABLED. If it has no rule permitting traffic on the network your build VMs use, template builds hang indefinitely at the installer's language-selection screen with no error — the VM's callback to Packer's autoinstall server is silently dropped. Check /etc/pve/firewall/cluster.fw for a rule covering your actual subnet, or disable it if you don't rely on it.",
			FixCommand: "pvesh set /cluster/firewall/options --enable 0",
		})
	default:
		checks = append(checks, PermCheck{
			Name: "Datacenter firewall not silently blocking builds", Privilege: "(informational)",
			OK: true, Probed: true, Detail: "Disabled — won't interfere with build VM networking.",
		})
	}

	return checks
}

func describeAuthErr(err error) string {
	if IsAuthError(err) {
		return "Permission denied: " + err.Error()
	}
	return err.Error()
}

// ClusterFirewallEnabled reports whether Proxmox's datacenter-wide firewall
// is turned on (GET /cluster/firewall/options -> {"enable": 0|1}).
func (c *Client) ClusterFirewallEnabled(ctx context.Context) (bool, error) {
	var opts struct {
		Enable int `json:"enable"`
	}
	if err := c.get(ctx, "/cluster/firewall/options", &opts); err != nil {
		return false, err
	}
	return opts.Enable == 1, nil
}

// AllOK reports whether every probed (live-tested) check passed. Declared
// (unprobed) checks are informational and don't block progress on their own.
func AllOK(checks []PermCheck) bool {
	for _, c := range checks {
		if c.Probed && !c.OK {
			return false
		}
	}
	return true
}
