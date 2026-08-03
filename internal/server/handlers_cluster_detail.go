package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"pvekube/internal/capi"
	"pvekube/internal/jobs"
	"pvekube/internal/ui"
)

func (s *Server) handleClusterDetailPage(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	name := r.PathValue("name")
	ui.Render(w, "cluster_detail", map[string]any{"CSRF": s.csrfFor(session), "ClusterName": name})
}

func (s *Server) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	status, err := capi.GetStatus(ctx, s.dataDir, s.binDir, name)
	if err != nil {
		ui.RenderPartial(w, "cluster_status", map[string]any{
			"ClusterName": name,
			"Status":      &capi.ClusterStatus{Found: false},
		})
		return
	}

	kubeconfigReady := false
	if status.Found {
		if _, err := capi.GetWorkloadKubeconfig(ctx, s.dataDir, s.binDir, name); err == nil {
			kubeconfigReady = true
		}
	}

	// Reflect the observed phase back into our own record — the DB row is
	// created with status='provisioning' at apply time and otherwise never
	// updated, so the clusters list would show a stale status forever.
	if status.Found && status.Phase != "" {
		s.db.Exec(`UPDATE clusters SET status = ? WHERE name = ?`, status.Phase, name)
	}

	ui.RenderPartial(w, "cluster_status", map[string]any{
		"ClusterName":     name,
		"Status":          status,
		"KubeconfigReady": kubeconfigReady,
	})
}

// runLifecycleJob starts a job and, unlike other job-progress renders in
// PVEKube, reloads back into the cluster status panel (not a list) since
// that's the page the operator is already looking at.
func (s *Server) runLifecycleJob(w http.ResponseWriter, name string, spec *jobs.Spec) {
	jobID, err := s.jobs.Start(spec, `{"cluster":"`+name+`"}`)
	if err != nil {
		http.Error(w, "starting job: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ui.RenderPartial(w, "job_progress", map[string]any{
		"JobID": jobID, "Title": spec.Title,
		"WrapperID": "cluster-status", "ReloadURL": "/clusters/" + name + "/status", "ReloadTarget": "#cluster-status",
	})
}

func (s *Server) handleClusterScaleWorkers(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	name := r.PathValue("name")
	r.ParseForm()
	replicas, err := strconv.Atoi(r.FormValue("replicas"))
	if err != nil || replicas < 0 {
		http.Error(w, "invalid replica count", http.StatusBadRequest)
		return
	}
	s.runLifecycleJob(w, name, capi.ScaleWorkersSpec(s.dataDir, s.binDir, name, replicas))
}

func (s *Server) handleClusterScaleControlPlane(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	name := r.PathValue("name")
	r.ParseForm()
	replicas, err := strconv.Atoi(r.FormValue("replicas"))
	if err != nil {
		http.Error(w, "invalid replica count", http.StatusBadRequest)
		return
	}
	s.runLifecycleJob(w, name, capi.ScaleControlPlaneSpec(s.dataDir, s.binDir, name, replicas))
}

func (s *Server) handleClusterDelete(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	name := r.PathValue("name")
	s.db.Exec(`UPDATE clusters SET status = 'deleting' WHERE name = ?`, name)
	s.runLifecycleJob(w, name, capi.DeleteClusterSpec(s.dataDir, s.binDir, name))
}

func (s *Server) handleClusterKubeconfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	kc, err := capi.GetWorkloadKubeconfig(ctx, s.dataDir, s.binDir, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`-kubeconfig.yaml"`)
	w.Write(kc)
}
