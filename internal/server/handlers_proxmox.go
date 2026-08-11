package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pvekube/internal/proxmox"
	"pvekube/internal/ui"
)

// storedConnection mirrors a proxmox_connections row, with the secret
// still sealed (decrypted only right before use, never held longer than needed).
type storedConnection struct {
	ID          int64
	Name        string
	URL         string
	TokenID     string
	SecretSeal  []byte
	InsecureTLS bool
	// IsPrimary is purely informational — it drives the "(primary)" badge in
	// the UI and nothing else. It used to also select whether ApplySpec
	// synced credentials into CAPMOX's shared global-fallback Secret; that
	// was removed after discovering (against a real cluster) that a
	// populated global Secret makes CAPMOX ignore spec.credentialsRef for
	// EVERY cluster, not just ones without their own — see the long comment
	// on ClusterConnection in internal/capi/generate.go for the full story.
	// Every cluster now always resolves credentials from its own connection-
	// scoped Secret regardless of which connection is primary.
	IsPrimary bool
}

// TokenUser returns the "user@realm" portion of TokenID (stripping
// "!tokenname"), for rendering copy-paste-ready pveum commands in the UI.
func (c *storedConnection) TokenUser() string {
	if idx := strings.Index(c.TokenID, "!"); idx >= 0 {
		return c.TokenID[:idx]
	}
	return c.TokenID
}

const connColumns = `id, name, url, token_id, secret_sealed, insecure_tls, is_primary`

func scanConnection(row interface{ Scan(...any) error }) (*storedConnection, error) {
	var c storedConnection
	var insecure, primary int
	if err := row.Scan(&c.ID, &c.Name, &c.URL, &c.TokenID, &c.SecretSeal, &insecure, &primary); err != nil {
		return nil, err
	}
	c.InsecureTLS = insecure == 1
	c.IsPrimary = primary == 1
	return &c, nil
}

// listConnections returns every stored Proxmox connection, most recently
// added first — backs both the multi-host list screen and the host
// selector on Templates/Clusters.
func (s *Server) listConnections() ([]*storedConnection, error) {
	rows, err := s.db.Query(`SELECT ` + connColumns + ` FROM proxmox_connections ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*storedConnection
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Server) getConnectionByID(id int64) (*storedConnection, error) {
	row := s.db.QueryRow(`SELECT `+connColumns+` FROM proxmox_connections WHERE id = ?`, id)
	return scanConnection(row)
}

// getPrimaryConnection fetches whichever connection is_primary=1, used only
// by the legacy default-credentials sync path (EnsureCredentialsStep) for
// clusters that predate per-connection credentialsRef. Returns
// sql.ErrNoRows if the primary connection was disconnected — callers must
// treat that as "nothing to sync," not a fatal error.
func (s *Server) getPrimaryConnection() (*storedConnection, error) {
	row := s.db.QueryRow(`SELECT ` + connColumns + ` FROM proxmox_connections WHERE is_primary = 1 LIMIT 1`)
	return scanConnection(row)
}

// appStateGet/appStateSet read and write the app_state KV table — unused
// anywhere else in the codebase until now, which makes it a safe, natural
// place for "currently selected Proxmox host," a single value that only
// matters to this one single-operator instance.
func (s *Server) appStateGet(key string) (string, bool) {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM app_state WHERE key = ?`, key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

func (s *Server) appStateSet(key, value string) {
	s.db.Exec(`INSERT INTO app_state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
}

const activeConnectionKey = "active_connection_id"

// activeConnection resolves "the currently selected host" for the
// Templates/Clusters screens: an explicit override (from a page's ?conn=
// query param) wins and is remembered for next time; otherwise whatever was
// last remembered; otherwise the most-recently-added connection — which
// preserves today's exact single-connection behavior for anyone who never
// adds a second host.
func (s *Server) activeConnection(override string) (*storedConnection, error) {
	if override != "" {
		id, err := strconv.ParseInt(override, 10, 64)
		if err == nil {
			if conn, err := s.getConnectionByID(id); err == nil {
				s.appStateSet(activeConnectionKey, override)
				return conn, nil
			}
		}
	}
	if v, ok := s.appStateGet(activeConnectionKey); ok {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			if conn, err := s.getConnectionByID(id); err == nil {
				return conn, nil
			}
			// Remembered connection no longer exists (disconnected) — fall
			// through to "most recent" rather than erroring the whole page.
		}
	}
	row := s.db.QueryRow(`SELECT ` + connColumns + ` FROM proxmox_connections ORDER BY id DESC LIMIT 1`)
	return scanConnection(row)
}

// rememberConnOverride persists a page-level ?conn= to app_state without
// resolving a client or rendering anything. Templates/Clusters pages are two
// separate requests — the page shell (which reads ?conn=) and an htmx
// hx-get="load" for the panel inside it (which doesn't forward query
// params) — so the override has to reach app_state here, in the page
// handler, for the panel's own activeConnection("") call to pick it up via
// the "remembered" fallback a moment later.
func (s *Server) rememberConnOverride(r *http.Request) {
	if override := r.URL.Query().Get("conn"); override != "" {
		if id, err := strconv.ParseInt(override, 10, 64); err == nil {
			if _, err := s.getConnectionByID(id); err == nil {
				s.appStateSet(activeConnectionKey, override)
			}
		}
	}
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

// handleProxmoxList is the hx-get="/proxmox/list" target on the Proxmox
// page: every stored connection as a card, plus the "Add host" form. Empty
// list and "no connections yet" both fall out of the same partial via
// {{if}} — no separate empty-state handler needed.
func (s *Server) handleProxmoxList(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	conns, err := s.listConnections()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ui.RenderPartial(w, "proxmox_list", map[string]any{"Connections": conns, "CSRF": s.csrfFor(session)})
}

func (s *Server) handleProxmoxConnect(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	r.ParseForm()
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}

	url := proxmox.NormalizeURL(r.FormValue("url"))
	// A trailing space/newline pasted into either field is invisible in the
	// browser but not in the HTTP Authorization header built from it later.
	// Confirmed the hard way: some Proxmox API clients (Packer's, evidently)
	// tolerate the stray whitespace, but CAPMOX's own client does not — a
	// connection saved with an untrimmed secret authenticates fine for
	// template builds yet fails EVERY cluster-creation API call with a 401,
	// and because that 401 comes back with an empty body, CAPMOX's JSON
	// decoder then fails with the opaque "unexpected end of JSON input"
	// rather than anything mentioning auth. Trimming here is the one place
	// that fixes it for every consumer of this connection at once.
	tokenID := strings.TrimSpace(r.FormValue("token_id"))
	secret := strings.TrimSpace(r.FormValue("secret"))
	insecure := r.FormValue("insecure_tls") == "1"
	s.redactor.Track(secret)

	if url == "" || tokenID == "" || secret == "" {
		ui.RenderPartial(w, "proxmox_form", map[string]any{
			"Error": "URL, token ID, and secret are all required.", "CSRF": s.csrfFor(session),
			"URL": url, "TokenID": tokenID,
		})
		return
	}

	// A connection is identified by (url, token_id), not url alone —
	// Proxmox legitimately supports multiple tokens per realm, so two
	// distinct connections can share a host. This also doubles as the only
	// practical way to add a second, fully independent connection against a
	// single physical Proxmox server for testing.
	var existing int
	s.db.QueryRow(`SELECT COUNT(*) FROM proxmox_connections WHERE url = ? AND token_id = ?`, url, tokenID).Scan(&existing)
	if existing > 0 {
		ui.RenderPartial(w, "proxmox_form", map[string]any{
			"Error": "Already connected with this URL and token — use a different token, or manage the existing connection below.",
			"CSRF":  s.csrfFor(session), "URL": url, "TokenID": tokenID,
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
	// First connection ever becomes primary automatically (matches the
	// pre-multi-host behavior exactly, since there was only ever one
	// connection); every connection added after that is explicitly not
	// primary — see storedConnection.IsPrimary's doc comment for why that
	// matters.
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM proxmox_connections`).Scan(&count)
	isPrimary := count == 0

	if _, err := s.db.Exec(`INSERT INTO proxmox_connections (name, url, token_id, secret_sealed, insecure_tls, is_primary) VALUES (?, ?, ?, ?, ?, ?)`,
		"default", url, tokenID, sealed, boolToInt(insecure), boolToInt(isPrimary)); err != nil {
		http.Error(w, "saving connection: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.handleProxmoxList(w, r)
}

// handleProxmoxDetail renders the full readiness/permissions view (nodes,
// bridges, storage, permission checklist) for exactly one connection — the
// "Manage" action on a card in the list.
func (s *Server) handleProxmoxDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad connection id", http.StatusBadRequest)
		return
	}
	conn, err := s.getConnectionByID(id)
	if err != nil {
		http.Error(w, "connection not found — it may have just been disconnected", http.StatusNotFound)
		return
	}
	s.renderConnected(w, r.Context(), conn, false)
}

func (s *Server) handleProxmoxRefresh(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad connection id", http.StatusBadRequest)
		return
	}
	conn, err := s.getConnectionByID(id)
	if err != nil {
		http.Error(w, "connection not found", http.StatusBadRequest)
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
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad connection id", http.StatusBadRequest)
		return
	}
	s.db.Exec(`DELETE FROM proxmox_connections WHERE id = ?`, id)
	s.handleProxmoxList(w, r)
}

func (s *Server) renderConnected(w http.ResponseWriter, ctx context.Context, conn *storedConnection, forceRefresh bool) {
	// The session was stashed in ctx by requireAuth and travels untouched
	// through handleProxmoxDetail/handleProxmoxRefresh into here — pulling
	// it back out is simpler than threading a session string through both
	// callers just for this one CSRF token the Disconnect button needs.
	var csrf string
	if session, ok := ctx.Value(ctxSessionKey{}).(string); ok {
		csrf = s.csrfFor(session)
	}

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
			ui.RenderPartial(w, "proxmox_detail_error", map[string]any{
				"Conn": conn, "CSRF": csrf, "Error": "Connected before, but discovery failed now: " + err.Error(),
			})
			return
		}
		s.cacheDiscovery(conn.ID, snap)
	}

	perms := client.VerifyPermissions(cctx)

	ui.RenderPartial(w, "proxmox_connected", map[string]any{
		"Conn":     conn,
		"CSRF":     csrf,
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
