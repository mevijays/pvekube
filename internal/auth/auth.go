// Package auth implements single-operator admin auth: argon2id password hash,
// server-side sessions in SQLite, and CSRF tokens for state-changing requests.
//
// This app holds Proxmox root-equivalent API credentials, so even a
// single-user "appliance" product needs real auth, not an open door.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/crypto/argon2"
)

const sessionCookieName = "pvekube_session"
const sessionTTL = 30 * 24 * time.Hour

var ErrNoAdmin = errors.New("no admin account configured")
var ErrBadCredentials = errors.New("incorrect password")

type Manager struct {
	db *sql.DB
}

func New(db *sql.DB) *Manager { return &Manager{db: db} }

// HasAdmin reports whether first-run setup has already happened.
func (m *Manager) HasAdmin() (bool, error) {
	var n int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM admin_user`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (m *Manager) CreateAdmin(password string) error {
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	_, err = m.db.Exec(`INSERT INTO admin_user (id, password_hash) VALUES (1, ?)`, hash)
	return err
}

func (m *Manager) VerifyPassword(password string) error {
	var hash string
	err := m.db.QueryRow(`SELECT password_hash FROM admin_user WHERE id = 1`).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoAdmin
	}
	if err != nil {
		return err
	}
	ok, err := verifyPassword(password, hash)
	if err != nil {
		return err
	}
	if !ok {
		return ErrBadCredentials
	}
	return nil
}

func (m *Manager) CreateSession() (token string, expires time.Time, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	expires = time.Now().Add(sessionTTL)
	_, err = m.db.Exec(`INSERT INTO sessions (token, expires_at) VALUES (?, ?)`, token, expires)
	return token, expires, err
}

func (m *Manager) ValidSession(token string) bool {
	if token == "" {
		return false
	}
	var expiresAt time.Time
	err := m.db.QueryRow(`SELECT expires_at FROM sessions WHERE token = ?`, token).Scan(&expiresAt)
	if err != nil {
		return false
	}
	return time.Now().Before(expiresAt)
}

func (m *Manager) DeleteSession(token string) {
	m.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
}

func (m *Manager) SetSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (m *Manager) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (m *Manager) SessionTokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// --- CSRF: double-submit token tied to the session, stored in app_state per-session ---

func GenerateCSRFToken() string {
	raw := make([]byte, 24)
	rand.Read(raw)
	return hex.EncodeToString(raw)
}

func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// --- password hashing: argon2id ---

const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16
)

func hashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$%d$%d$%d$%s$%s",
		argonTime, argonMemory, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(password, encoded string) (bool, error) {
	parts := splitN(encoded, '$', 6)
	if len(parts) != 6 || parts[0] != "argon2id" {
		return false, errors.New("unrecognized password hash format")
	}
	var timeC, memory uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[1], "%d", &timeC); err != nil {
		return false, err
	}
	if _, err := fmt.Sscanf(parts[2], "%d", &memory); err != nil {
		return false, err
	}
	if _, err := fmt.Sscanf(parts[3], "%d", &threads); err != nil {
		return false, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, timeC, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func splitN(s string, sep byte, n int) []string {
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(s) && len(out) < n-1; i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
