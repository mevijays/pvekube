// Package server wires HTTP routes to the auth, job, prereq, and (later)
// proxmox/bootstrap/capi packages, and renders the embedded UI templates.
package server

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"pvekube/internal/auth"
	"pvekube/internal/jobs"
	"pvekube/internal/prereq"
	"pvekube/internal/secrets"
)

type Server struct {
	db       *sql.DB
	authMgr  *auth.Manager
	jobs     *jobs.Engine
	sealer   *secrets.Sealer
	redactor *secrets.Redactor
	binDir   string
	dataDir  string

	mu         sync.Mutex
	csrfBySess map[string]string // session token -> csrf token, so a leaked GET can't be used to forge POSTs
	lastChecks []prereq.Result
}

type Deps struct {
	DB       *sql.DB
	AuthMgr  *auth.Manager
	Jobs     *jobs.Engine
	Sealer   *secrets.Sealer
	Redactor *secrets.Redactor
	BinDir   string
	DataDir  string
}

func New(d Deps) *Server {
	return &Server{
		db:         d.DB,
		authMgr:    d.AuthMgr,
		jobs:       d.Jobs,
		sealer:     d.Sealer,
		redactor:   d.Redactor,
		binDir:     d.BinDir,
		dataDir:    d.DataDir,
		csrfBySess: make(map[string]string),
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /setup", s.handleSetupForm)
	mux.HandleFunc("POST /setup", s.handleSetupSubmit)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.requireAuth(s.handleLogout))

	mux.HandleFunc("GET /{$}", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("GET /prereqs", s.requireAuth(s.handlePrereqsPage))
	mux.HandleFunc("GET /prereqs/list", s.requireAuth(s.handlePrereqsList))
	mux.HandleFunc("POST /prereqs/refresh", s.requireAuth(s.handlePrereqsList))
	mux.HandleFunc("POST /prereqs/fix/{id}", s.requireAuth(s.handlePrereqsFix))
	mux.HandleFunc("GET /jobs/{id}/stream", s.requireAuth(s.handleJobStream))
	mux.HandleFunc("GET /management/kubeconfig", s.requireAuth(s.handleManagementKubeconfig))

	mux.HandleFunc("GET /proxmox", s.requireAuth(s.handleProxmoxPage))
	mux.HandleFunc("GET /proxmox/status", s.requireAuth(s.handleProxmoxStatus))
	mux.HandleFunc("POST /proxmox/connect", s.requireAuth(s.handleProxmoxConnect))
	mux.HandleFunc("POST /proxmox/refresh", s.requireAuth(s.handleProxmoxRefresh))
	mux.HandleFunc("POST /proxmox/disconnect", s.requireAuth(s.handleProxmoxDisconnect))

	mux.HandleFunc("GET /templates", s.requireAuth(s.handleTemplatesPage))
	mux.HandleFunc("GET /templates/panel", s.requireAuth(s.handleTemplatesPanel))
	mux.HandleFunc("POST /templates/validate", s.requireAuth(s.handleTemplatesValidate))
	mux.HandleFunc("POST /templates/build", s.requireAuth(s.handleTemplatesBuild))

	mux.HandleFunc("GET /clusters", s.requireAuth(s.handleClustersPage))
	mux.HandleFunc("GET /clusters/panel", s.requireAuth(s.handleClustersPanel))
	mux.HandleFunc("POST /clusters/check-ip", s.requireAuth(s.handleClustersCheckIP))
	mux.HandleFunc("POST /clusters/preview", s.requireAuth(s.handleClustersPreview))
	mux.HandleFunc("POST /clusters/apply", s.requireAuth(s.handleClustersApply))
	mux.HandleFunc("GET /clusters/{name}", s.requireAuth(s.handleClusterDetailPage))
	mux.HandleFunc("GET /clusters/{name}/status", s.requireAuth(s.handleClusterStatus))
	mux.HandleFunc("GET /clusters/{name}/kubeconfig", s.requireAuth(s.handleClusterKubeconfig))
	mux.HandleFunc("POST /clusters/{name}/scale-workers", s.requireAuth(s.handleClusterScaleWorkers))
	mux.HandleFunc("POST /clusters/{name}/scale-controlplane", s.requireAuth(s.handleClusterScaleControlPlane))
	mux.HandleFunc("POST /clusters/{name}/delete", s.requireAuth(s.handleClusterDelete))

	return withLogging(mux)
}

func withLogging(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		slog.Info("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
	})
}

// requireAuth redirects to /setup or /login as appropriate, otherwise calls next.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hasAdmin, err := s.authMgr.HasAdmin()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !hasAdmin {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		token := s.authMgr.SessionTokenFromRequest(r)
		if !s.authMgr.ValidSession(token) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), ctxSessionKey{}, token)
		next(w, r.WithContext(ctx))
	}
}

type ctxSessionKey struct{}

func (s *Server) csrfFor(session string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.csrfBySess[session]; ok {
		return t
	}
	t := auth.GenerateCSRFToken()
	s.csrfBySess[session] = t
	return t
}

func (s *Server) checkCSRF(r *http.Request, session string) bool {
	s.mu.Lock()
	want, ok := s.csrfBySess[session]
	s.mu.Unlock()
	if !ok {
		return false
	}
	got := r.FormValue("csrf")
	return auth.ConstantTimeEqual(got, want)
}
