package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"pvekube/internal/proxmox"
	"pvekube/internal/ui"
)

// storedConnection mirrors the proxmox_connections row, with the secret
// still sealed (decrypted only right before use, never held longer than needed).
type storedConnection struct {
	ID          int64
	Name        string
	URL         string
	TokenID     string
	SecretSeal  []byte
	InsecureTLS bool
}

// TokenUser returns the "user@realm" portion of TokenID (stripping
// "!tokenname"), for rendering copy-paste-ready pveum commands in the UI.
func (c *storedConnection) TokenUser() string {
	if idx := strings.Index(c.TokenID, "!"); idx >= 0 {
		return c.TokenID[:idx]
	}
	return c.TokenID
}

func (s *Server) getConnection() (*storedConnection, error) {
	row := s.db.QueryRow(`SELECT id, name, url, token_id, secret_sealed, insecure_tls FROM proxmox_connections ORDER BY id DESC LIMIT 1`)
	var c storedConnection
	var insecure int
	if err := row.Scan(&c.ID, &c.Name, &c.URL, &c.TokenID, &c.SecretSeal, &insecure); err != nil {
		return nil, err
	}
	c.InsecureTLS = insecure == 1
	return &c, nil
}

func (s *Server) proxmoxClientFor(c *storedConnection) (*proxmox.Client, error) {
	secret, err := s.sealer.Open(c.SecretSeal)
	if err != nil {
		return nil, err
	}
	s.redactor.Track(secret)
	return proxmox.New(proxmox.Config{
		URL: c.URL, TokenID: c.TokenID, Secret: secret, InsecureSkipVerify: c.InsecureTLS,
	}), nil
}

// handleProxmoxStatus is the hx-get="/proxmox/status" target: shows the
// connection form if none exists yet, otherwise the live connected view.
func (s *Server) handleProxmoxStatus(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	conn, err := s.getConnection()
	if errors.Is(err, sql.ErrNoRows) {
		ui.RenderPartial(w, "proxmox_form", map[string]any{"CSRF": s.csrfFor(session)})
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderConnected(w, r.Context(), conn, false)
}

func (s *Server) handleProxmoxConnect(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	r.ParseForm()
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}

	url := r.FormValue("url")
	tokenID := r.FormValue("token_id")
	secret := r.FormValue("secret")
	insecure := r.FormValue("insecure_tls") == "1"
	s.redactor.Track(secret)

	if url == "" || tokenID == "" || secret == "" {
		ui.RenderPartial(w, "proxmox_form", map[string]any{
			"Error": "URL, token ID, and secret are all required.", "CSRF": s.csrfFor(session),
			"URL": url, "TokenID": tokenID,
		})
		return
	}

	client := proxmox.New(proxmox.Config{URL: url, TokenID: tokenID, Secret: secret, InsecureSkipVerify: insecure})
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if _, err := client.Version(ctx); err != nil {
		ui.RenderPartial(w, "proxmox_form", map[string]any{
			"Error": "Could not connect: " + err.Error(), "CSRF": s.csrfFor(session),
			"URL": url, "TokenID": tokenID,
		})
		return
	}

	sealed, err := s.sealer.Seal(secret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Single-connection model for now: replace any existing profile.
	s.db.Exec(`DELETE FROM proxmox_connections`)
	res, err := s.db.Exec(`INSERT INTO proxmox_connections (name, url, token_id, secret_sealed, insecure_tls) VALUES (?, ?, ?, ?, ?)`,
		"default", url, tokenID, sealed, boolToInt(insecure))
	if err != nil {
		http.Error(w, "saving connection: "+err.Error(), http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	conn := &storedConnection{ID: id, URL: url, TokenID: tokenID, SecretSeal: sealed, InsecureTLS: insecure}
	s.renderConnected(w, r.Context(), conn, true)
}

func (s *Server) handleProxmoxRefresh(w http.ResponseWriter, r *http.Request) {
	conn, err := s.getConnection()
	if err != nil {
		http.Error(w, "no connection configured", http.StatusBadRequest)
		return
	}
	s.renderConnected(w, r.Context(), conn, true)
}

func (s *Server) handleProxmoxDisconnect(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	s.db.Exec(`DELETE FROM proxmox_connections`)
	ui.RenderPartial(w, "proxmox_form", map[string]any{"CSRF": s.csrfFor(session)})
}

func (s *Server) renderConnected(w http.ResponseWriter, ctx context.Context, conn *storedConnection, forceRefresh bool) {
	client, err := s.proxmoxClientFor(conn)
	if err != nil {
		http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var snap *proxmox.Snapshot
	if !forceRefresh {
		snap = s.loadCachedDiscovery(conn.ID)
	}
	if snap == nil {
		snap, err = client.Discover(cctx)
		if err != nil {
			ui.RenderPartial(w, "proxmox_form", map[string]any{
				"Error": "Connected before, but discovery failed now: " + err.Error(),
			})
			return
		}
		s.cacheDiscovery(conn.ID, snap)
	}

	perms := client.VerifyPermissions(cctx)

	ui.RenderPartial(w, "proxmox_connected", map[string]any{
		"Conn":     conn,
		"Snapshot": snap,
		"Perms":    perms,
	})
}

func (s *Server) loadCachedDiscovery(connID int64) *proxmox.Snapshot {
	var raw string
	err := s.db.QueryRow(`SELECT snapshot_json FROM proxmox_discovery WHERE connection_id = ?`, connID).Scan(&raw)
	if err != nil {
		return nil
	}
	var snap proxmox.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return nil
	}
	return &snap
}

func (s *Server) cacheDiscovery(connID int64, snap *proxmox.Snapshot) {
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	s.db.Exec(`INSERT INTO proxmox_discovery (connection_id, snapshot_json, refreshed_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(connection_id) DO UPDATE SET snapshot_json = excluded.snapshot_json, refreshed_at = CURRENT_TIMESTAMP`, connID, string(b))
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
