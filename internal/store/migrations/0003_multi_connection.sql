-- Multi-Proxmox-host support. Until now PVEKube assumed exactly one
-- proxmox_connections row (getConnection() always fetched "whatever was
-- added most recently", and connecting a new host deleted the old one).
-- is_primary marks whichever connection existing pre-migration clusters
-- depend on: their ProxmoxCluster objects have no spec.credentialsRef, so
-- they resolve credentials through CAPMOX's single global fallback Secret,
-- which internal/capi.EnsureCredentialsStep keeps synced only for the
-- primary connection. Every cluster created after this migration gets its
-- own dedicated credentials Secret + credentialsRef instead (see
-- internal/capi/generate.go, internal/capi/registry.go) and never depends
-- on is_primary at all — this column exists purely for backward
-- compatibility with clusters that predate it.
ALTER TABLE proxmox_connections ADD COLUMN is_primary INTEGER NOT NULL DEFAULT 0;

-- Backfill: whatever connection already exists becomes primary, so
-- pre-migration clusters keep resolving credentials exactly as before with
-- no manual step. If more than one row somehow exists already (shouldn't,
-- given the single-connection model this migration replaces), the oldest
-- one wins — it's the one most likely to have clusters/templates recorded
-- against it.
UPDATE proxmox_connections SET is_primary = 1
WHERE id = (SELECT id FROM proxmox_connections ORDER BY id ASC LIMIT 1);
