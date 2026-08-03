package server

import (
	"net/http"

	"pvekube/internal/auth"
	"pvekube/internal/ui"
)

func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	hasAdmin, err := s.authMgr.HasAdmin()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if hasAdmin {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ui.Render(w, "setup", map[string]any{"CSRF": s.anonCSRF(w, r)})
}

func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	hasAdmin, err := s.authMgr.HasAdmin()
	if err != nil || hasAdmin {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	r.ParseForm()
	if !s.checkAnonCSRF(r) {
		ui.Render(w, "setup", map[string]any{"Error": "Session expired, please retry.", "CSRF": s.anonCSRF(w, r)})
		return
	}
	pw := r.FormValue("password")
	confirm := r.FormValue("password_confirm")
	if pw != confirm {
		ui.Render(w, "setup", map[string]any{"Error": "Passwords do not match.", "CSRF": s.anonCSRF(w, r)})
		return
	}
	if err := s.authMgr.CreateAdmin(pw); err != nil {
		ui.Render(w, "setup", map[string]any{"Error": err.Error(), "CSRF": s.anonCSRF(w, r)})
		return
	}
	token, expires, err := s.authMgr.CreateSession()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.authMgr.SetSessionCookie(w, token, expires)
	http.Redirect(w, r, "/prereqs", http.StatusSeeOther)
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	hasAdmin, _ := s.authMgr.HasAdmin()
	if !hasAdmin {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if s.authMgr.ValidSession(s.authMgr.SessionTokenFromRequest(r)) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	ui.Render(w, "login", map[string]any{"CSRF": s.anonCSRF(w, r)})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if !s.checkAnonCSRF(r) {
		ui.Render(w, "login", map[string]any{"Error": "Session expired, please retry.", "CSRF": s.anonCSRF(w, r)})
		return
	}
	pw := r.FormValue("password")
	if err := s.authMgr.VerifyPassword(pw); err != nil {
		msg := "Incorrect password."
		if err == auth.ErrNoAdmin {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		ui.Render(w, "login", map[string]any{"Error": msg, "CSRF": s.anonCSRF(w, r)})
		return
	}
	token, expires, err := s.authMgr.CreateSession()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.authMgr.SetSessionCookie(w, token, expires)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	s.authMgr.DeleteSession(session)
	s.authMgr.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- pre-session CSRF (setup/login forms, before a session cookie exists) ---
//
// We anchor the anonymous CSRF token to a lightweight, unauthenticated cookie
// rather than the real session (which doesn't exist yet pre-login).

const anonCSRFCookie = "pvekube_anon_csrf"

func (s *Server) anonCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(anonCSRFCookie); err == nil && c.Value != "" {
		return c.Value
	}
	t := auth.GenerateCSRFToken()
	http.SetCookie(w, &http.Cookie{Name: anonCSRFCookie, Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return t
}

func (s *Server) checkAnonCSRF(r *http.Request) bool {
	c, err := r.Cookie(anonCSRFCookie)
	if err != nil {
		return false
	}
	return auth.ConstantTimeEqual(r.FormValue("csrf"), c.Value)
}
