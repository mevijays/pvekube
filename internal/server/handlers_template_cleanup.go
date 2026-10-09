package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"pvekube/internal/imagebuilder"
	"pvekube/internal/proxmox"
	"pvekube/internal/ui"
)

// buildJobParams is what a template.build job records about itself, so a
// later clean-up can locate the build VM without any in-memory state.
type buildJobParams struct {
	Kind string `json:"kind"`
	VMID int    `json:"vmid"`
	Conn int64  `json:"conn,omitempty"`
	Node string `json:"node,omitempty"`
}

// leftoverBuildView is one finished-badly build the operator can clean up.
type leftoverBuildView struct {
	JobID  int64
	Title  string
	Status string
	VMID   int
	Node   string
	Ended  string
	Reason string
}

// maxLeftoverBuilds bounds how much history the Templates page lists; older
// debris is still reachable by VMID in Proxmox itself.
const maxLeftoverBuilds = 5

// buildLeftovers lists recent template builds on conn that ended without
// succeeding after the Packer step had actually started (earlier failures,
// such as a rejected ISO download, never created a VM) and whose leftovers
// have not been confirmed removed.
func (s *Server) buildLeftovers(connID int64) ([]leftoverBuildView, error) {
	rows, err := s.db.Query(`
		SELECT j.id, j.title, j.status, j.params_json, COALESCE(j.ended_at, j.started_at, j.created_at), COALESCE(j.error, '')
		FROM jobs j
		WHERE j.kind = 'template.build'
		  AND j.status IN ('failed', 'cancelled', 'interrupted')
		  AND j.cleaned_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM templates t WHERE t.build_job_id = j.id)
		  AND EXISTS (SELECT 1 FROM job_steps st WHERE st.job_id = j.id AND st.title LIKE 'packer build%' AND st.started_at IS NOT NULL)
		ORDER BY j.id DESC
		LIMIT 25`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []leftoverBuildView
	for rows.Next() {
		var v leftoverBuildView
		var paramsJSON, errText string
		if err := rows.Scan(&v.JobID, &v.Title, &v.Status, &paramsJSON, &v.Ended, &errText); err != nil {
			return nil, err
		}
		var p buildJobParams
		// Builds started before conn/node were recorded cannot be located,
		// so they are not offered; they have to be removed in Proxmox.
		if json.Unmarshal([]byte(paramsJSON), &p) != nil || p.Conn != connID || p.Node == "" || p.VMID == 0 {
			continue
		}
		v.VMID, v.Node = p.VMID, p.Node
		if i := strings.IndexByte(errText, '\n'); i >= 0 {
			errText = errText[:i]
		}
		v.Reason = errText
		out = append(out, v)
		if len(out) == maxLeftoverBuilds {
			break
		}
	}
	return out, rows.Err()
}

// markBuildCleaned records that a build's leftovers are gone.
func (s *Server) markBuildCleaned(jobID int64) {
	if _, err := s.db.Exec(`UPDATE jobs SET cleaned_at = CURRENT_TIMESTAMP WHERE id = ?`, jobID); err != nil {
		slog.Warn("could not record build clean-up", "job", jobID, "err", err)
	}
}

// handleTemplatesCleanup removes the Packer container and build VM a failed,
// cancelled or interrupted template build left behind. Synchronous like
// template deletion: it is a stop-and-delete, a few seconds of Proxmox work.
func (s *Server) handleTemplatesCleanup(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	r.ParseForm()
	conn, err := s.formConnection(r)
	if err != nil {
		ui.RenderPartial(w, "templates_not_connected", nil)
		return
	}
	fail := func(msg string) { s.renderTemplatesPanel(w, r.Context(), session, conn, msg) }

	jobID, err := strconv.ParseInt(r.FormValue("job"), 10, 64)
	if err != nil {
		fail("bad job id")
		return
	}
	var kind, status, paramsJSON string
	if err := s.db.QueryRow(`SELECT kind, status, params_json FROM jobs WHERE id = ?`, jobID).Scan(&kind, &status, &paramsJSON); err != nil {
		fail("that build no longer exists")
		return
	}
	var p buildJobParams
	if kind != "template.build" || json.Unmarshal([]byte(paramsJSON), &p) != nil || p.Conn != conn.ID || p.Node == "" || p.VMID == 0 {
		fail("that job is not a template build on this Proxmox host")
		return
	}
	if status == "pending" || status == "running" || status == "succeeded" {
		fail(fmt.Sprintf("build job %d is %s — only a failed, cancelled or interrupted build can be cleaned up", jobID, status))
		return
	}
	// VMIDs are recycled once a VM is gone, so a newer build may now own this
	// one. Never reach for a VM while a build could be using it.
	if busy, _ := s.templateBuildInProgress(); busy {
		fail("a template build is running — wait for it to finish before cleaning up an old one")
		return
	}

	secret, err := s.sealer.Open(conn.SecretSeal)
	if err != nil {
		fail(err.Error())
		return
	}
	s.redactor.Track(secret)
	env := imagebuilder.ConnEnv{
		URL: proxmox.NormalizeURL(conn.URL), TokenID: conn.TokenID, Secret: secret, InsecureTLS: conn.InsecureTLS, Node: p.Node,
	}

	var log []string
	logf := func(format string, args ...any) { log = append(log, fmt.Sprintf(format, args...)) }
	if err := imagebuilder.CleanupBuild(logf, env, p.VMID); err != nil {
		fail(fmt.Sprintf("clean-up of build job %d did not finish: %s (%s)", jobID, err, strings.Join(log, " · ")))
		return
	}
	s.markBuildCleaned(jobID)
	s.renderTemplatesPanelWith(w, r.Context(), session, conn, "", fmt.Sprintf("Cleaned up build job %d: %s", jobID, strings.Join(log, " ")))
}
