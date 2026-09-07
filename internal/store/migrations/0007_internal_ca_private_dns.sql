-- Internal CA and private DNS, same sticky-default model as the registry,
-- OIDC and GitOps columns: written after a cluster is applied, pre-filled
-- into the next creation form, always editable there.
--
-- internal_ca_cert is deliberately separate from registry_ca_cert even
-- though a homelab usually signs both with the same CA. registry_ca_cert
-- only ever reached containerd and the node trust store, and only when a
-- registry host was also configured; this one is about any internal HTTPS
-- endpoint the nodes or the pods talk to, so it has to be settable with no
-- registry in play at all. None of these are secrets — a CA certificate is
-- public by construction and a resolver address is not sensitive — so
-- unlike registry_password_sealed / gitops_token_sealed they are stored
-- plain.
ALTER TABLE cluster_defaults ADD COLUMN internal_ca_cert     TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_defaults ADD COLUMN private_dns_domains  TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_defaults ADD COLUMN private_dns_servers  TEXT NOT NULL DEFAULT '';
