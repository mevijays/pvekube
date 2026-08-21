-- Remembered "default users group" — the LDAP/OIDC group that
-- automatically gets the built-in "view" ClusterRole (read-only,
-- cluster-wide) via InstallOIDCDefaultGroupRBACStep once a cluster is up.
-- Same sticky-default model as every other cluster_defaults column.
ALTER TABLE cluster_defaults ADD COLUMN oidc_default_users_group TEXT NOT NULL DEFAULT '';
