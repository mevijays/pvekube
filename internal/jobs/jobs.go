// Package jobs implements the long-running-operation engine every screen in
// PVEKube is built on: prereq remediation, template builds, cluster apply,
// etc. all run as a Job made of ordered Steps.
//
// Design constraints that shaped this:
//   - A Packer build can take 30+ minutes. The browser WILL be refreshed or
//     closed mid-build. Logs are persisted to disk, not held in memory, and
//     SSE subscribers replay from the current offset then tail.
//   - The app itself may restart mid-job (crash, upgrade). On startup any
//     job left "running" is marked "interrupted" rather than silently lost,
//     so the UI can tell the user what happened instead of showing nothing.
//   - Steps run in-process as Go closures (not necessarily shell commands —
//     a step might call the Proxmox API, or shell out via runner.Run).
package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type Status string

const (
	StatusPending     Status = "pending"
	StatusRunning     Status = "running"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusCancelled   Status = "cancelled"
	StatusInterrupted Status = "interrupted"
	StatusSkipped     Status = "skipped"
)

// StepFunc does the actual work of one step. It receives a Ctx providing
// cancellation and a line-oriented log writer. Returning an error fails the
// step (and the job); the job then stops running further steps.
type StepFunc func(*Ctx) error

// Ctx is handed to every step function.
type Ctx struct {
	context.Context
	JobID  int64
	StepID int64
	log    *stepLogger
}

func (c *Ctx) Logf(format string, args ...any) {
	c.log.Logf(format, args...)
}

type stepDef struct {
	title string
	fn    StepFunc
}

// Spec describes a job before it starts running.
type Spec struct {
	Kind  string
	Title string
	Steps []stepDef
}

func NewSpec(kind, title string) *Spec {
	return &Spec{Kind: kind, Title: title}
}

func (s *Spec) Step(title string, fn StepFunc) *Spec {
	s.Steps = append(s.Steps, stepDef{title: title, fn: fn})
	return s
}

type Engine struct {
	db        *sql.DB
	logDir    string
	redact    func(string) string
	mu        sync.Mutex
	broadcast map[int64][]chan string // jobID -> subscriber channels (log lines, "STEP:<n>:<status>", "JOB:<status>")
	cancels   map[int64]context.CancelFunc
}

func NewEngine(db *sql.DB, logDir string, redact func(string) string) *Engine {
	if redact == nil {
		redact = func(s string) string { return s }
	}
	return &Engine{
		db:        db,
		logDir:    logDir,
		redact:    redact,
		broadcast: make(map[int64][]chan string),
		cancels:   make(map[int64]context.CancelFunc),
	}
}

// ReconcileOnStartup marks any job left "running" from a previous process as
// "interrupted" so the UI can surface it honestly instead of showing a stale
// spinner forever.
func (e *Engine) ReconcileOnStartup() error {
	_, err := e.db.Exec(`UPDATE jobs SET status = ?, ended_at = CURRENT_TIMESTAMP, error = 'application restarted mid-job' WHERE status = ?`,
		StatusInterrupted, StatusRunning)
	if err != nil {
		return err
	}
	_, err = e.db.Exec(`UPDATE job_steps SET status = ? WHERE status = ?`, StatusInterrupted, StatusRunning)
	return err
}

// Start creates the job row + step rows and runs it in the background.
// Returns the job ID immediately; use Subscribe or the DB to observe progress.
func (e *Engine) Start(spec *Spec, paramsJSON string) (int64, error) {
	res, err := e.db.Exec(`INSERT INTO jobs (kind, title, status, params_json) VALUES (?, ?, ?, ?)`,
		spec.Kind, spec.Title, StatusPending, paramsJSON)
	if err != nil {
		return 0, err
	}
	jobID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for i, st := range spec.Steps {
		if _, err := e.db.Exec(`INSERT INTO job_steps (job_id, seq, title, status) VALUES (?, ?, ?, ?)`,
			jobID, i, st.title, StatusPending); err != nil {
			return 0, err
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.cancels[jobID] = cancel
	e.mu.Unlock()

	go e.run(ctx, jobID, spec)
	return jobID, nil
}

func (e *Engine) Cancel(jobID int64) {
	e.mu.Lock()
	cancel, ok := e.cancels[jobID]
	e.mu.Unlock()
	if ok {
		cancel()
	}
}

func (e *Engine) run(ctx context.Context, jobID int64, spec *Spec) {
	defer func() {
		e.mu.Lock()
		delete(e.cancels, jobID)
		e.mu.Unlock()
	}()

	e.db.Exec(`UPDATE jobs SET status = ?, started_at = CURRENT_TIMESTAMP WHERE id = ?`, StatusRunning, jobID)
	e.publish(jobID, "JOB:running")

	var rows *sql.Rows
	rows, err := e.db.Query(`SELECT id FROM job_steps WHERE job_id = ? ORDER BY seq`, jobID)
	if err != nil {
		e.failJob(jobID, err)
		return
	}
	var stepIDs []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		stepIDs = append(stepIDs, id)
	}
	rows.Close()

	finalStatus := StatusSucceeded
	var jobErr error

	for i, st := range spec.Steps {
		select {
		case <-ctx.Done():
			e.db.Exec(`UPDATE job_steps SET status = ? WHERE id = ?`, StatusSkipped, stepIDs[i])
			finalStatus = StatusCancelled
			jobErr = fmt.Errorf("cancelled")
			continue
		default:
		}
		if finalStatus == StatusFailed || finalStatus == StatusCancelled {
			e.db.Exec(`UPDATE job_steps SET status = ? WHERE id = ?`, StatusSkipped, stepIDs[i])
			continue
		}

		stepID := stepIDs[i]
		logPath := fmt.Sprintf("%s/job-%d-step-%d.log", e.logDir, jobID, i)
		e.db.Exec(`UPDATE job_steps SET status = ?, started_at = CURRENT_TIMESTAMP, log_path = ? WHERE id = ?`,
			StatusRunning, logPath, stepID)
		e.publish(jobID, fmt.Sprintf("STEP:%d:running", i))

		logger, err := newStepLogger(logPath, func(line string) {
			e.publish(jobID, "LINE:"+e.redact(line))
		}, e.redact)
		if err != nil {
			e.failJob(jobID, err)
			return
		}

		sctx := &Ctx{Context: ctx, JobID: jobID, StepID: stepID, log: logger}
		err = st.fn(sctx)
		logger.Close()

		if err != nil {
			e.db.Exec(`UPDATE job_steps SET status = ?, ended_at = CURRENT_TIMESTAMP WHERE id = ?`, StatusFailed, stepID)
			e.publish(jobID, fmt.Sprintf("STEP:%d:failed", i))
			finalStatus = StatusFailed
			jobErr = err
			continue
		}
		e.db.Exec(`UPDATE job_steps SET status = ?, ended_at = CURRENT_TIMESTAMP WHERE id = ?`, StatusSucceeded, stepID)
		e.publish(jobID, fmt.Sprintf("STEP:%d:succeeded", i))
	}

	errText := ""
	if jobErr != nil {
		errText = jobErr.Error()
	}
	e.db.Exec(`UPDATE jobs SET status = ?, ended_at = CURRENT_TIMESTAMP, error = ? WHERE id = ?`, finalStatus, errText, jobID)
	e.publish(jobID, "JOB:"+string(finalStatus))
	e.closeSubscribers(jobID)
}

func (e *Engine) failJob(jobID int64, err error) {
	e.db.Exec(`UPDATE jobs SET status = ?, ended_at = CURRENT_TIMESTAMP, error = ? WHERE id = ?`, StatusFailed, err.Error(), jobID)
	e.publish(jobID, "JOB:failed")
	e.closeSubscribers(jobID)
	slog.Error("job failed", "job_id", jobID, "error", err)
}

// Subscribe returns a channel of event lines for a running job: "LINE:...",
// "STEP:<idx>:<status>", "JOB:<status>". The channel is closed when the job
// finishes or the caller should stop (context done).
func (e *Engine) Subscribe(jobID int64) <-chan string {
	ch := make(chan string, 256)
	e.mu.Lock()
	e.broadcast[jobID] = append(e.broadcast[jobID], ch)
	e.mu.Unlock()
	return ch
}

func (e *Engine) Unsubscribe(jobID int64, ch <-chan string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	subs := e.broadcast[jobID]
	for i, c := range subs {
		if c == ch {
			e.broadcast[jobID] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
}

func (e *Engine) publish(jobID int64, line string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ch := range e.broadcast[jobID] {
		select {
		case ch <- line:
		default:
			// Slow subscriber; drop rather than block the job.
		}
	}
}

func (e *Engine) closeSubscribers(jobID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ch := range e.broadcast[jobID] {
		close(ch)
	}
	delete(e.broadcast, jobID)
}

// --- step logger: writes to disk immediately, also fans out to a callback ---

type stepLogger struct {
	mu     sync.Mutex
	onLine func(string)
	redact func(string) string
	file   *lineFile
}

func newStepLogger(path string, onLine func(string), redact func(string) string) (*stepLogger, error) {
	lf, err := openLineFile(path)
	if err != nil {
		return nil, err
	}
	return &stepLogger{onLine: onLine, redact: redact, file: lf}, nil
}

func (l *stepLogger) Logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := time.Now().Format("15:04:05")
	full := fmt.Sprintf("[%s] %s", ts, l.redact(line))
	l.file.WriteLine(full)
	if l.onLine != nil {
		l.onLine(full)
	}
}

func (l *stepLogger) Close() { l.file.Close() }
