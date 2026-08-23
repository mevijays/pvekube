-- A non-empty lock_key identifies a resource that may have only one active
-- mutating job. The partial unique index makes the check atomic across
-- simultaneous HTTP requests while allowing completed job history to retain
-- the same key.
ALTER TABLE jobs ADD COLUMN lock_key TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX IF NOT EXISTS jobs_one_active_lock
ON jobs(lock_key)
WHERE lock_key <> '' AND status IN ('pending', 'running');
