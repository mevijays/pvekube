package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"pvekube/internal/jobs"
	"pvekube/internal/prereq"
	"pvekube/internal/ui"
)

func (s *Server) handlePrereqsPage(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	ui.Render(w, "prereqs", map[string]any{"CSRF": s.csrfFor(session)})
}

func (s *Server) handlePrereqsList(w http.ResponseWriter, r *http.Request) {
	checks := prereq.Registry(s.binDir, s.dataDir)
	results := runChecksConcurrently(r.Context(), checks)

	s.mu.Lock()
	s.lastChecks = results
	s.mu.Unlock()

	type view struct {
		ID      string
		Name    string
		Status  string
		Detail  string
		Fixable bool
	}
	views := make([]view, 0, len(results))
	for _, res := range results {
		views = append(views, view{ID: res.ID, Name: res.Name, Status: string(res.Status), Detail: res.Detail, Fixable: res.Fixable})
	}
	ui.RenderPartial(w, "checklist", map[string]any{"Checks": views})
}

func runChecksConcurrently(ctx context.Context, checks []prereq.Check) []prereq.Result {
	results := make([]prereq.Result, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func(i int, c prereq.Check) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			results[i] = c.Run(cctx)
		}(i, c)
	}
	wg.Wait()
	return results
}

func (s *Server) handlePrereqsFix(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	spec := prereq.BuildFixSpec(id, s.binDir, s.dataDir)
	if spec == nil {
		http.Error(w, "no automated fix for "+id, http.StatusBadRequest)
		return
	}
	jobID, err := s.jobs.Start(spec, `{"check_id":"`+id+`"}`)
	if err != nil {
		http.Error(w, "starting job: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ui.RenderPartial(w, "job_progress", map[string]any{"JobID": jobID, "Title": spec.Title})
}

// handleJobCancel cancels a running job. Session-authenticated only (not
// CSRF-checked against the job_progress panel's own token) since the panel
// doesn't carry one — acceptable because it can only ever cancel a job the
// authenticated session already started or can see the ID of.
func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	jobID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad job id", http.StatusBadRequest)
		return
	}
	s.jobs.Cancel(jobID)
	w.WriteHeader(http.StatusNoContent)
}

// handleJobStream is a Server-Sent Events endpoint. It first replays every
// persisted log line from disk (so a browser that connects after the job
// already produced output — e.g. a refresh mid-build — still sees it all),
// then subscribes for live lines. Job/step status changes are sent as
// "STEP:<idx>:<status>" / "JOB:<status>" events on the same stream.
func (s *Server) handleJobStream(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	jobID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad job id", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Subscribe BEFORE replay so we never miss a line produced in the gap.
	ch := s.jobs.Subscribe(jobID)
	defer s.jobs.Unsubscribe(jobID, ch)

	writeEvent(w, flusher, "connected")

	// Replay persisted step state and log lines, so reconnecting — or just
	// reloading the page while a job runs — shows everything produced so
	// far instead of only what arrives from this moment on.
	//
	// The column is "seq"; job_steps has never had a "step_index" (see the
	// INSERT in jobs.go). This query named the wrong column, and because
	// the error was discarded into `_`, Query returned nil rows and the
	// whole replay block was skipped without a trace: every job's log pane
	// came up empty and only filled in from the next live line onward,
	// which reads exactly like "the UI isn't printing output". Keep the
	// error checked so the next schema drift fails loudly instead.
	rows, err := s.db.Query(`SELECT seq, status, COALESCE(log_path, '') FROM job_steps WHERE job_id = ? ORDER BY seq`, jobID)
	if err != nil {
		slog.Error("job stream: cannot read steps for replay", "job", jobID, "err", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var seq int
			var status, logPath string
			if err := rows.Scan(&seq, &status, &logPath); err != nil {
				slog.Error("job stream: cannot scan step row", "job", jobID, "err", err)
				continue
			}
			// A pending step hasn't started and may still be skipped
			// entirely; emitting it would render a placeholder tile for
			// work that never runs.
			if status != string(jobs.StatusPending) {
				writeEvent(w, flusher, fmt.Sprintf("STEP:%d:%s", seq, status))
			}
			if logPath == "" {
				continue
			}
			lines, err := jobs.ReadAllLines(logPath)
			if err != nil {
				slog.Warn("job stream: cannot read step log", "job", jobID, "path", logPath, "err", err)
				continue
			}
			for _, line := range lines {
				writeEvent(w, flusher, "LINE:"+line)
			}
		}
	}

	status, err := s.jobStatus(jobID)
	if err == nil && status != string(jobs.StatusRunning) && status != string(jobs.StatusPending) {
		// Already finished: report the final status.
		writeEvent(w, flusher, "JOB:"+status)
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-ch:
			if !ok {
				return
			}
			writeEvent(w, flusher, line)
		}
	}
}

func writeEvent(w http.ResponseWriter, f http.Flusher, data string) {
	w.Write([]byte("data: " + escapeSSE(data) + "\n\n"))
	f.Flush()
}

func escapeSSE(s string) string {
	// SSE "data:" lines can't contain a literal newline; our log lines are
	// already single-line (bufio.Scanner splits them), but guard anyway.
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, ' ')
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

func (s *Server) jobStatus(jobID int64) (string, error) {
	var status string
	err := s.db.QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status)
	return status, err
}
