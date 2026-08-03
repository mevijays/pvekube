-- Core schema for PVEKube. SQLite, pure-Go driver (modernc.org/sqlite).

CREATE TABLE IF NOT EXISTS app_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Single-row admin account. Non-technical-user product, single operator.
CREATE TABLE IF NOT EXISTS admin_user (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS sessions (
    token      TEXT PRIMARY KEY,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMP NOT NULL
);

-- Proxmox connection profile. secret_sealed is AES-GCM sealed with the app key.
CREATE TABLE IF NOT EXISTS proxmox_connections (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT NOT NULL,
    url           TEXT NOT NULL,
    token_id      TEXT NOT NULL,
    secret_sealed BLOB NOT NULL,
    insecure_tls  INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Cached discovery snapshot (JSON blob) so downstream screens populate instantly
-- without re-querying Proxmox on every page load. Refreshed on demand.
CREATE TABLE IF NOT EXISTS proxmox_discovery (
    connection_id INTEGER PRIMARY KEY REFERENCES proxmox_connections(id) ON DELETE CASCADE,
    snapshot_json TEXT NOT NULL,
    refreshed_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS jobs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL,           -- e.g. "prereq.install_docker", "template.build"
    title       TEXT NOT NULL,
    status      TEXT NOT NULL,           -- pending|running|succeeded|failed|cancelled|interrupted
    params_json TEXT NOT NULL DEFAULT '{}',
    error       TEXT,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at  TIMESTAMP,
    ended_at    TIMESTAMP
);

CREATE TABLE IF NOT EXISTS job_steps (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id      INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL,
    title       TEXT NOT NULL,
    status      TEXT NOT NULL,           -- pending|running|succeeded|failed|skipped
    log_path    TEXT,
    started_at  TIMESTAMP,
    ended_at    TIMESTAMP,
    UNIQUE(job_id, seq)
);

CREATE TABLE IF NOT EXISTS templates (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    connection_id INTEGER NOT NULL REFERENCES proxmox_connections(id) ON DELETE CASCADE,
    os_flavor    TEXT NOT NULL,          -- ubuntu-2404, ubuntu-2604-efi, rockylinux-9, flatcar...
    k8s_version  TEXT NOT NULL,
    node         TEXT NOT NULL,
    vmid         INTEGER NOT NULL,
    build_job_id INTEGER REFERENCES jobs(id),
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS clusters (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL UNIQUE,
    connection_id INTEGER NOT NULL REFERENCES proxmox_connections(id) ON DELETE CASCADE,
    template_id  INTEGER NOT NULL REFERENCES templates(id),
    manifest_yaml TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'provisioning',
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
