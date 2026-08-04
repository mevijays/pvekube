package server

import (
	"database/sql"
	"log/slog"
	"strings"
)

// clusterDefaults are the previously-used cluster-creation inputs that get
// pre-filled into the creation form. Only values that are genuinely identical
// between clusters in one environment belong here — the registry the whole
// homelab pulls from, the SSH key that should land on every node. Per-cluster
// values (name, IPs, replica counts) deliberately stay blank so nobody
// accidentally creates two clusters on the same control plane endpoint.
type clusterDefaults struct {
	VMSSHKeys        string
	RegistryHost     string
	RegistryCACert   string
	RegistryUsername string
	RegistryPassword string
}

// loadClusterDefaults reads the remembered inputs. A missing row (nothing
// created yet) is not an error — it just yields empty defaults, which render
// as an empty form exactly as before this feature existed.
func (s *Server) loadClusterDefaults() clusterDefaults {
	var d clusterDefaults
	var sealed []byte
	row := s.db.QueryRow(`SELECT vm_ssh_keys, registry_host, registry_ca_cert, registry_username, registry_password_sealed
	                        FROM cluster_defaults WHERE id = 1`)
	if err := row.Scan(&d.VMSSHKeys, &d.RegistryHost, &d.RegistryCACert, &d.RegistryUsername, &sealed); err != nil {
		if err != sql.ErrNoRows {
			slog.Warn("loading cluster defaults", "err", err)
		}
		return clusterDefaults{}
	}
	if len(sealed) > 0 {
		pw, err := s.sealer.Open(sealed)
		if err != nil {
			// A key change or corrupt blob shouldn't block cluster creation —
			// drop the password and let the operator retype it.
			slog.Warn("unsealing remembered registry password", "err", err)
		} else {
			d.RegistryPassword = pw
			s.redactor.Track(pw)
		}
	}
	return d
}

// saveClusterDefaults records the inputs from a cluster that was actually
// applied. Called on the apply path rather than preview so that abandoning a
// half-filled form never changes what the next one is seeded with.
//
// Each field is only overwritten when the new value is non-empty: building
// one cluster without a registry shouldn't wipe a CA the operator still wants
// next time. That stickiness means forgetting has to be explicit, which is
// what clearClusterDefaults / the "Forget" button are for.
func (s *Server) saveClusterDefaults(d clusterDefaults) {
	cur := s.loadClusterDefaults()
	merged := clusterDefaults{
		VMSSHKeys:        firstNonEmpty(d.VMSSHKeys, cur.VMSSHKeys),
		RegistryHost:     firstNonEmpty(d.RegistryHost, cur.RegistryHost),
		RegistryCACert:   firstNonEmpty(d.RegistryCACert, cur.RegistryCACert),
		RegistryUsername: firstNonEmpty(d.RegistryUsername, cur.RegistryUsername),
		RegistryPassword: firstNonEmpty(d.RegistryPassword, cur.RegistryPassword),
	}

	var sealed []byte
	if merged.RegistryPassword != "" {
		b, err := s.sealer.Seal(merged.RegistryPassword)
		if err != nil {
			slog.Warn("sealing registry password for defaults", "err", err)
		} else {
			sealed = b
		}
	}

	if _, err := s.db.Exec(`
		INSERT INTO cluster_defaults (id, vm_ssh_keys, registry_host, registry_ca_cert, registry_username, registry_password_sealed, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			vm_ssh_keys = excluded.vm_ssh_keys,
			registry_host = excluded.registry_host,
			registry_ca_cert = excluded.registry_ca_cert,
			registry_username = excluded.registry_username,
			registry_password_sealed = excluded.registry_password_sealed,
			updated_at = CURRENT_TIMESTAMP`,
		merged.VMSSHKeys, merged.RegistryHost, merged.RegistryCACert, merged.RegistryUsername, sealed); err != nil {
		// Never fail the cluster launch over a convenience feature.
		slog.Warn("saving cluster defaults", "err", err)
	}
}

// clearClusterDefaults forgets the remembered inputs entirely. Without this
// the sticky merge above would be a one-way door: a CA or SSH key, once
// saved, could never be removed from the form except by editing the database.
func (s *Server) clearClusterDefaults() {
	if _, err := s.db.Exec(`DELETE FROM cluster_defaults WHERE id = 1`); err != nil {
		slog.Warn("clearing cluster defaults", "err", err)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
