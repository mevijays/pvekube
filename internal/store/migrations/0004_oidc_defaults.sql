-- Remembered OIDC auth settings, same "sticky default" model as the
-- registry_* columns in 0002_cluster_defaults.sql: written after a cluster
-- is actually applied, pre-filled into the next creation form, always
-- editable there.
--
-- No sealed/secret column here unlike registry_password_sealed: kube-
-- apiserver's OIDC plugin only ever verifies ID tokens against the
-- issuer's public JWKS using issuer-url + client-id (the token audience) —
-- it never holds or needs a client secret. Every value here is meant to be
-- visible in the generated manifest already (see internal/capi/oidc.go).
ALTER TABLE cluster_defaults ADD COLUMN oidc_provider       TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_defaults ADD COLUMN oidc_issuer_url     TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_defaults ADD COLUMN oidc_client_id      TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_defaults ADD COLUMN oidc_username_claim TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_defaults ADD COLUMN oidc_groups_claim   TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_defaults ADD COLUMN oidc_ca_cert        TEXT NOT NULL DEFAULT '';
