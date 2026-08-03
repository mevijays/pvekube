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

const (
	fixAuditor = "pveum aclmod / -user <tokenuser> -role PVEAuditor"
	fixVMAdmin = "pveum aclmod / -user <tokenuser> -role PVEVMAdmin"
)

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

	// Can't be probed without creating/cloning a real VM, so these are
	// declared based on the token's role assignment rather than tested.
	// CAPMOX and the template builder both need full VM lifecycle control.
	checks = append(checks, PermCheck{
		Name: "Create, clone, and configure VMs", Privilege: "PVEVMAdmin role",
		OK: true, Probed: false, FixCommand: fixVMAdmin,
		Detail: "Not live-tested (would require creating a VM) — assumed present. If template builds or cluster launches fail with a permission error, run the fix command.",
	})

	return checks
}

func describeAuthErr(err error) string {
	if IsAuthError(err) {
		return "Permission denied: " + err.Error()
	}
	return err.Error()
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
