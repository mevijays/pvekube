// Package secrets handles at-rest encryption of sensitive values (Proxmox API
// tokens, etc.) and redaction of those values from anything that might be
// logged or streamed to the browser.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Sealer encrypts/decrypts secret values with a key generated on first run and
// persisted to disk (0600). This is "at rest on this host" protection, not a
// defense against someone who already has root on the box — that's an
// inherent limit of a self-hosted single-binary tool holding infra credentials.
type Sealer struct {
	key [32]byte
}

func LoadOrCreateSealer(dataDir string) (*Sealer, error) {
	keyPath := filepath.Join(dataDir, "seal.key")
	b, err := os.ReadFile(keyPath)
	if err == nil {
		if len(b) != 32 {
			return nil, fmt.Errorf("seal key at %s is corrupt (want 32 bytes, got %d)", keyPath, len(b))
		}
		s := &Sealer{}
		copy(s.key[:], b)
		return s, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	s := &Sealer{}
	if _, err := rand.Read(s.key[:]); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, s.key[:], 0o600); err != nil {
		return nil, fmt.Errorf("writing seal key: %w", err)
	}
	return s, nil
}

func (s *Sealer) Seal(plaintext string) ([]byte, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func (s *Sealer) Open(sealed []byte) (string, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("sealed value too short")
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("decrypting: %w", err)
	}
	return string(pt), nil
}

// Redactor scrubs known-secret substrings out of text before it is written to
// job logs or streamed over SSE to the browser. Every value handed to Track
// is masked in all future Redact calls for the process lifetime.
type Redactor struct {
	mu     sync.RWMutex
	values []string
}

func NewRedactor() *Redactor { return &Redactor{} }

// Track registers a secret value to be redacted. Short values (<4 chars) are
// ignored to avoid mass-redacting common substrings.
func (r *Redactor) Track(secret string) {
	if len(strings.TrimSpace(secret)) < 4 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = append(r.values, secret)
}

func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, "[REDACTED]")
	}
	return s
}
