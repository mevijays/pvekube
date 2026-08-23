package store

import (
	"path/filepath"
	"testing"
)

func TestOpenAppliesActiveJobLockMigration(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "pvekube.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.DB.Exec(`INSERT INTO jobs (kind, title, status, lock_key) VALUES ('cluster.scale', 'first', 'running', 'cluster:demo')`); err != nil {
		t.Fatalf("lock_key column was not migrated: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO jobs (kind, title, status, lock_key) VALUES ('cluster.delete', 'second', 'pending', 'cluster:demo')`); err == nil {
		t.Fatal("partial unique index allowed two active jobs for one lock key")
	}
	if _, err := st.DB.Exec(`UPDATE jobs SET status = 'succeeded' WHERE lock_key = 'cluster:demo'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`INSERT INTO jobs (kind, title, status, lock_key) VALUES ('cluster.delete', 'after', 'pending', 'cluster:demo')`); err != nil {
		t.Fatalf("terminal job continued to hold the lock: %v", err)
	}
}
