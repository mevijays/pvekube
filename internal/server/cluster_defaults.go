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

	// OIDC auth settings — no secret among these (see 0004_oidc_defaults.sql's
	// comment for why), so they need none of the sealing RegistryPassword does.
	OIDCProvider      string
	OIDCIssuerURL     string
	OIDCClientID      string
	OIDCUsernameClaim string
	OIDCGroupsClaim   string
	OIDCCACert        string
	// OIDCDefaultUsersGroup is the group InstallOIDCDefaultGroupRBACStep
	// grants the built-in "view" ClusterRole to automatically — see its doc
	// comment (internal/capi/oidc.go) for why this exists.
	OIDCDefaultUsersGroup string

	// GitOps (Flux) settings. GitOpsToken is a real secret and is sealed at
	// rest like RegistryPassword; the rest are plain.
	GitOpsRepoURL  string
	GitOpsBranch   string
	GitOpsPath     string
	GitOpsUsername string
	GitOpsToken    string
	GitOpsCACert   string

	// InternalCACert is the organisation's private CA. Reaches further than
	// RegistryCACert ever did — node trust stores and a per-namespace
	// ConfigMap for pods — and is settable without a registry. See
	// internal/capi/trust.go.
	InternalCACert string
	// PrivateDNS* are stored as the operator typed them (comma-separated)
	// rather than as parsed lists, so the form round-trips their formatting
	// unchanged. capi.ParseDNSList does the splitting on the way in.
	PrivateDNSDomains string
	PrivateDNSServers string
}

// loadClusterDefaults reads the remembered inputs. A missing row (nothing
// created yet) is not an error — it just yields empty defaults, which render
// as an empty form exactly as before this feature existed.
func (s *Server) loadClusterDefaults() clusterDefaults {
	var d clusterDefaults
	var sealed, gitopsSealed []byte
	row := s.db.QueryRow(`SELECT vm_ssh_keys, registry_host, registry_ca_cert, registry_username, registry_password_sealed,
	                              oidc_provider, oidc_issuer_url, oidc_client_id, oidc_username_claim, oidc_groups_claim, oidc_ca_cert, oidc_default_users_group,
	                              gitops_repo_url, gitops_branch, gitops_path, gitops_username, gitops_token_sealed, gitops_ca_cert,
	                              internal_ca_cert, private_dns_domains, private_dns_servers
	                        FROM cluster_defaults WHERE id = 1`)
	if err := row.Scan(&d.VMSSHKeys, &d.RegistryHost, &d.RegistryCACert, &d.RegistryUsername, &sealed,
		&d.OIDCProvider, &d.OIDCIssuerURL, &d.OIDCClientID, &d.OIDCUsernameClaim, &d.OIDCGroupsClaim, &d.OIDCCACert, &d.OIDCDefaultUsersGroup,
		&d.GitOpsRepoURL, &d.GitOpsBranch, &d.GitOpsPath, &d.GitOpsUsername, &gitopsSealed, &d.GitOpsCACert,
		&d.InternalCACert, &d.PrivateDNSDomains, &d.PrivateDNSServers); err != nil {
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
	if len(gitopsSealed) > 0 {
		tok, err := s.sealer.Open(gitopsSealed)
		if err != nil {
			slog.Warn("unsealing remembered GitOps token", "err", err)
		} else {
			d.GitOpsToken = tok
			s.redactor.Track(tok)
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

		OIDCProvider:          firstNonEmpty(d.OIDCProvider, cur.OIDCProvider),
		OIDCIssuerURL:         firstNonEmpty(d.OIDCIssuerURL, cur.OIDCIssuerURL),
		OIDCClientID:          firstNonEmpty(d.OIDCClientID, cur.OIDCClientID),
		OIDCUsernameClaim:     firstNonEmpty(d.OIDCUsernameClaim, cur.OIDCUsernameClaim),
		OIDCGroupsClaim:       firstNonEmpty(d.OIDCGroupsClaim, cur.OIDCGroupsClaim),
		OIDCCACert:            firstNonEmpty(d.OIDCCACert, cur.OIDCCACert),
		OIDCDefaultUsersGroup: firstNonEmpty(d.OIDCDefaultUsersGroup, cur.OIDCDefaultUsersGroup),

		GitOpsRepoURL:  firstNonEmpty(d.GitOpsRepoURL, cur.GitOpsRepoURL),
		GitOpsBranch:   firstNonEmpty(d.GitOpsBranch, cur.GitOpsBranch),
		GitOpsPath:     firstNonEmpty(d.GitOpsPath, cur.GitOpsPath),
		GitOpsUsername: firstNonEmpty(d.GitOpsUsername, cur.GitOpsUsername),
		GitOpsToken:    firstNonEmpty(d.GitOpsToken, cur.GitOpsToken),
		GitOpsCACert:   firstNonEmpty(d.GitOpsCACert, cur.GitOpsCACert),

		InternalCACert:    firstNonEmpty(d.InternalCACert, cur.InternalCACert),
		PrivateDNSDomains: firstNonEmpty(d.PrivateDNSDomains, cur.PrivateDNSDomains),
		PrivateDNSServers: firstNonEmpty(d.PrivateDNSServers, cur.PrivateDNSServers),
	}

	var gitopsSealed []byte
	if merged.GitOpsToken != "" {
		b, err := s.sealer.Seal(merged.GitOpsToken)
		if err != nil {
			slog.Warn("sealing GitOps token for defaults", "err", err)
		} else {
			gitopsSealed = b
		}
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
		INSERT INTO cluster_defaults (id, vm_ssh_keys, registry_host, registry_ca_cert, registry_username, registry_password_sealed,
		                              oidc_provider, oidc_issuer_url, oidc_client_id, oidc_username_claim, oidc_groups_claim, oidc_ca_cert, oidc_default_users_group,
		                              gitops_repo_url, gitops_branch, gitops_path, gitops_username, gitops_token_sealed, gitops_ca_cert,
		                              internal_ca_cert, private_dns_domains, private_dns_servers, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			vm_ssh_keys = excluded.vm_ssh_keys,
			registry_host = excluded.registry_host,
			registry_ca_cert = excluded.registry_ca_cert,
			registry_username = excluded.registry_username,
			registry_password_sealed = excluded.registry_password_sealed,
			oidc_provider = excluded.oidc_provider,
			oidc_issuer_url = excluded.oidc_issuer_url,
			oidc_client_id = excluded.oidc_client_id,
			oidc_username_claim = excluded.oidc_username_claim,
			oidc_groups_claim = excluded.oidc_groups_claim,
			oidc_ca_cert = excluded.oidc_ca_cert,
			oidc_default_users_group = excluded.oidc_default_users_group,
			gitops_repo_url = excluded.gitops_repo_url,
			gitops_branch = excluded.gitops_branch,
			gitops_path = excluded.gitops_path,
			gitops_username = excluded.gitops_username,
			gitops_token_sealed = excluded.gitops_token_sealed,
			gitops_ca_cert = excluded.gitops_ca_cert,
			internal_ca_cert = excluded.internal_ca_cert,
			private_dns_domains = excluded.private_dns_domains,
			private_dns_servers = excluded.private_dns_servers,
			updated_at = CURRENT_TIMESTAMP`,
		merged.VMSSHKeys, merged.RegistryHost, merged.RegistryCACert, merged.RegistryUsername, sealed,
		merged.OIDCProvider, merged.OIDCIssuerURL, merged.OIDCClientID, merged.OIDCUsernameClaim, merged.OIDCGroupsClaim, merged.OIDCCACert, merged.OIDCDefaultUsersGroup,
		merged.GitOpsRepoURL, merged.GitOpsBranch, merged.GitOpsPath, merged.GitOpsUsername, gitopsSealed, merged.GitOpsCACert,
		merged.InternalCACert, merged.PrivateDNSDomains, merged.PrivateDNSServers); err != nil {
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
