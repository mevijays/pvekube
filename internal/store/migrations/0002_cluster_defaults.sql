-- Remembered cluster-creation inputs: the values an operator would otherwise
-- retype identically for every cluster (a multi-line CA PEM especially).
-- Written after a cluster is actually applied, then pre-filled into the next
-- creation form, where they stay editable — a default, never a lock.
--
-- Single row (id = 1), same shape as admin_user: these describe the one
-- environment this PVEKube instance manages, not a per-connection setting.
-- The registry password is sealed with the app key, exactly like
-- proxmox_connections.secret_sealed, so it is never at rest in plaintext.
CREATE TABLE IF NOT EXISTS cluster_defaults (
    id                       INTEGER PRIMARY KEY CHECK (id = 1),
    vm_ssh_keys              TEXT NOT NULL DEFAULT '',
    registry_host            TEXT NOT NULL DEFAULT '',
    registry_ca_cert         TEXT NOT NULL DEFAULT '',
    registry_username        TEXT NOT NULL DEFAULT '',
    registry_password_sealed BLOB,
    updated_at               TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
