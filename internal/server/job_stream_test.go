package server

import (
	"database/sql"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pvekube/internal/jobs"
	"pvekube/internal/store"
)

// migratedDB opens a real, fully migrated database instead of a hand-written
// schema, and that distinction is the entire point of the test below: the
// replay query in handleJobStream used to select a "step_index" column that
// job_steps has never had. A test carrying its own CREATE TABLE would have
// reproduced the same wrong name and passed while the app stayed broken.
func migratedDB(t *testing.T) *sql.DB {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st.DB
}

// The failure this pins down was silent and total: because the bad query's
// error went into `_`, Query returned nil rows, the replay block was skipped
// entirely, and every job's log pane rendered empty on load — no error in the
// UI, none in the server log. Only lines that happened to arrive live, after
// the browser connected, ever showed up.
func TestJobStreamReplaysPersistedStepLogs(t *testing.T) {
	db := migratedDB(t)
	logDir := t.TempDir()

	logPath := filepath.Join(logDir, "job-1-step-0.log")
	if err := os.WriteFile(logPath, []byte("first line\nsecond line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Status 'succeeded' so the handler replays, reports the final status and
	// returns, rather than blocking on the live channel forever.
	if _, err := db.Exec(
		`INSERT INTO jobs (id, kind, title, status) VALUES (1, 'cluster.apply', 'Apply cluster devdemo', 'succeeded')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO job_steps (job_id, seq, title, status, log_path) VALUES
		(1, 0, 'kubectl apply', 'succeeded', ?),
		(1, 1, 'Install CNI', 'pending', NULL)`, logPath); err != nil {
		t.Fatal(err)
	}

	s := &Server{db: db, jobs: jobs.NewEngine(db, logDir, nil)}

	req := httptest.NewRequest("GET", "/jobs/1/stream", nil)
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	s.handleJobStream(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		"data: connected",
		"data: STEP:0:succeeded",
		"data: LINE:first line",
		"data: LINE:second line",
		"data: JOB:succeeded",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream is missing %q\n--- got ---\n%s", want, body)
		}
	}

	// A step that never started must not render a placeholder tile — it may
	// still be skipped entirely.
	if strings.Contains(body, "STEP:1:pending") {
		t.Errorf("pending step should not be replayed\n--- got ---\n%s", body)
	}
}

// A NULL log_path (every step starts that way, before the runner fills it in)
// must not abort the replay of the steps around it.
func TestJobStreamReplaySurvivesNullLogPath(t *testing.T) {
	db := migratedDB(t)
	logDir := t.TempDir()

	logPath := filepath.Join(logDir, "job-2-step-1.log")
	if err := os.WriteFile(logPath, []byte("later step ran\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO jobs (id, kind, title, status) VALUES (2, 'template.build', 'Build template', 'failed')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO job_steps (job_id, seq, title, status, log_path) VALUES
		(2, 0, 'Stage ISO', 'skipped', NULL),
		(2, 1, 'Run packer', 'failed', ?)`, logPath); err != nil {
		t.Fatal(err)
	}

	s := &Server{db: db, jobs: jobs.NewEngine(db, logDir, nil)}

	req := httptest.NewRequest("GET", "/jobs/2/stream", nil)
	req.SetPathValue("id", "2")
	rec := httptest.NewRecorder()
	s.handleJobStream(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		"data: STEP:0:skipped",
		"data: STEP:1:failed",
		"data: LINE:later step ran",
		"data: JOB:failed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream is missing %q\n--- got ---\n%s", want, body)
		}
	}
}
