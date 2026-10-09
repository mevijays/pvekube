package server

import (
	"database/sql"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// A buffering reverse proxy holds SSE events until its buffer fills, which
// looks like a job that prints nothing for minutes. The stream has to opt out
// and flush a padded preamble so the first chunk always gets through.
func TestJobStreamDisablesProxyBuffering(t *testing.T) {
	db := migratedDB(t)
	if _, err := db.Exec(`INSERT INTO jobs (id, kind, title, status) VALUES (1, 'x', 'x', 'succeeded')`); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, jobs: jobs.NewEngine(db, t.TempDir(), nil)}
	req := httptest.NewRequest("GET", "/jobs/1/stream", nil)
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	s.handleJobStream(rec, req)

	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-transform") {
		t.Errorf("Cache-Control = %q, want no-transform", got)
	}
	body := rec.Body.String()
	if i := strings.Index(body, "data: connected"); i < 4096 {
		t.Errorf("preamble padding missing: \"connected\" at byte %d", i)
	}
}

func TestBuildLeftoversListsOnlyUncleanedBuildsThatReachedPacker(t *testing.T) {
	db := migratedDB(t)
	params := func(vmid int, conn int, node string) string {
		return `{"kind":"build","vmid":` + strconv.Itoa(vmid) + `,"conn":` + strconv.Itoa(conn) + `,"node":"` + node + `"}`
	}
	add := func(id int, status, p, buildStepStarted string, cleaned bool) {
		t.Helper()
		var cleanedAt any
		if cleaned {
			cleanedAt = "2026-10-04 15:00:00"
		}
		if _, err := db.Exec(`INSERT INTO jobs (id, kind, title, status, params_json, cleaned_at) VALUES (?, 'template.build', 'b', ?, ?, ?)`, id, status, p, cleanedAt); err != nil {
			t.Fatal(err)
		}
		var started any
		if buildStepStarted != "" {
			started = buildStepStarted
		}
		if _, err := db.Exec(`INSERT INTO job_steps (job_id, seq, title, status, started_at) VALUES (?, 4, 'packer build (20-35 minutes)', ?, ?)`, id, status, started); err != nil {
			t.Fatal(err)
		}
	}
	add(1, "failed", params(100, 1, "pve2"), "2026-10-04 14:00:00", false)        // listed
	add(2, "cancelled", params(101, 1, "pve2"), "2026-10-04 14:00:00", false)     // listed
	add(3, "failed", params(102, 1, "pve2"), "", false)                           // Packer never started: no VM
	add(4, "failed", params(103, 1, "pve2"), "2026-10-04 14:00:00", true)         // already cleaned
	add(5, "failed", params(104, 2, "pve2"), "2026-10-04 14:00:00", false)        // other host
	add(6, "failed", `{"kind":"build","vmid":105}`, "2026-10-04 14:00:00", false) // legacy params: node unknown
	add(7, "running", params(106, 1, "pve2"), "2026-10-04 14:00:00", false)       // still running

	s := &Server{db: db}
	got, err := s.buildLeftovers(1)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, v := range got {
		ids = append(ids, v.JobID)
	}
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 1 {
		t.Errorf("leftover jobs = %v, want [2 1]", ids)
	}
}
