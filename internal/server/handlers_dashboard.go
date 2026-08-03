package server

import (
	"net/http"

	"pvekube/internal/ui"
)

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	ui.Render(w, "dashboard", map[string]any{"CSRF": s.csrfFor(session)})
}

func (s *Server) handleProxmoxPage(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	ui.Render(w, "proxmox", map[string]any{"CSRF": s.csrfFor(session)})
}
