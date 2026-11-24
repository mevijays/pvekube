-- Per-cluster extra data disk for WORKER nodes (e.g. a Ceph/Rook OSD disk).
-- CAPMOX cannot declare a second disk, so PVEKube attaches it through the
-- Proxmox API and re-applies it after upgrades/resizes/scale-ups replace the
-- worker VMs. One row per cluster; absence means "no extra disk wanted".
CREATE TABLE IF NOT EXISTS cluster_worker_disk (
    cluster_name TEXT PRIMARY KEY,
    storage      TEXT NOT NULL,
    size_gib     INTEGER NOT NULL,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
