-- Remembered GitOps (Flux) settings, same sticky-default model as the
-- registry and OIDC columns: written after a cluster is applied, pre-filled
-- into the next creation form, always editable there.
--
-- The token IS a secret and is sealed with the app key exactly like
-- registry_password_sealed — unlike the OIDC settings, which have no secret
-- among them. It is never written into the cluster manifest either: GitOps
-- is a post-provision step, so the token only ever reaches the workload
-- cluster's own Secret.
ALTER TABLE cluster_defaults ADD COLUMN gitops_repo_url     TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_defaults ADD COLUMN gitops_branch       TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_defaults ADD COLUMN gitops_path         TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_defaults ADD COLUMN gitops_username     TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_defaults ADD COLUMN gitops_token_sealed BLOB;
ALTER TABLE cluster_defaults ADD COLUMN gitops_ca_cert      TEXT NOT NULL DEFAULT '';
