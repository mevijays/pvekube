package jobs

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openJobsTestDB(t *testing.T, stepConstraint string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "jobs.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)

	schema := `
		CREATE TABLE jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL,
			title TEXT NOT NULL,
			status TEXT NOT NULL,
			params_json TEXT NOT NULL DEFAULT '{}',
			lock_key TEXT NOT NULL DEFAULT '',
			error TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			started_at TIMESTAMP,
			ended_at TIMESTAMP
		);
		CREATE UNIQUE INDEX jobs_one_active_lock ON jobs(lock_key)
		WHERE lock_key <> '' AND status IN ('pending', 'running');
		CREATE TABLE job_steps (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
			seq INTEGER NOT NULL ` + stepConstraint + `,
			title TEXT NOT NULL,
			status TEXT NOT NULL,
			log_path TEXT,
			started_at TIMESTAMP,
			ended_at TIMESTAMP,
			UNIQUE(job_id, seq)
		);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestStartRollsBackJobWhenStepInsertFails(t *testing.T) {
	db := openJobsTestDB(t, "CHECK (seq < 1)")
	e := NewEngine(db, t.TempDir(), nil)
	spec := NewSpec("test", "transactional start").
		Step("first", func(*Ctx) error { return nil }).
		Step("second", func(*Ctx) error { return nil })

	if _, err := e.Start(spec, `{}`); err == nil {
		t.Fatal("Start succeeded despite the second step violating its CHECK constraint")
	}
	for _, table := range []string{"jobs", "job_steps"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s has %d rows after failed Start; want a full rollback", table, count)
		}
	}
}

func TestStartExclusiveRejectsConcurrentResourceOperation(t *testing.T) {
	db := openJobsTestDB(t, "")
	e := NewEngine(db, t.TempDir(), nil)
	started := make(chan struct{})
	release := make(chan struct{})
	spec := NewSpec("cluster.scale", "first operation").Step("block", func(c *Ctx) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-c.Done():
			return c.Err()
		}
	})

	jobID, err := e.StartExclusive(spec, `{"cluster":"dev"}`, "cluster:dev")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first job did not start")
	}

	_, err = e.StartExclusive(NewSpec("cluster.delete", "conflict"), `{"cluster":"dev"}`, "cluster:dev")
	if !errors.Is(err, ErrResourceBusy) {
		t.Fatalf("second StartExclusive error = %v, want ErrResourceBusy", err)
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		var status string
		if err := db.QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == string(StatusSucceeded) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first job status = %s; want succeeded", status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := e.StartExclusive(NewSpec("cluster.delete", "after completion"), `{"cluster":"dev"}`, "cluster:dev"); err != nil {
		t.Fatalf("lock was not released by terminal status: %v", err)
	}
}
