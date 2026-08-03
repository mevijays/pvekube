// Package proxmox is a thin REST client for the Proxmox VE API, scoped to
// what PVEKube needs: read-only discovery (nodes, storage, network, next
// VMID) plus permission self-verification. It deliberately does NOT
// reimplement VM lifecycle management — that's Packer's and CAPMOX's job;
// this client only ever needs to look, not touch.
package proxmox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL    string // e.g. https://172.16.1.101:8006/api2/json
	tokenID    string // capmox@pve!capi
	secret     string
	httpClient *http.Client
}

type Config struct {
	// URL is the Proxmox web UI/API endpoint, e.g. "https://172.16.1.101:8006"
	// or bare "172.16.1.101" (port 8006 and scheme are filled in).
	URL string
	// TokenID is the full token id: "<user>@<realm>!<tokenname>", e.g. "capmox@pve!capi".
	TokenID string
	// Secret is the token's UUID secret.
	Secret string
	// InsecureSkipVerify accepts Proxmox's self-signed certificate — the
	// default and expected case for a home-lab / on-prem Proxmox install.
	InsecureSkipVerify bool
}

func New(cfg Config) *Client {
	base := normalizeURL(cfg.URL)
	return &Client{
		baseURL: base + "/api2/json",
		tokenID: cfg.TokenID,
		secret:  cfg.Secret,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify},
			},
		},
	}
}

// NormalizeURL fills in a default https:// scheme and :8006 port when
// missing, e.g. "172.16.1.101" -> "https://172.16.1.101:8006". Exported so
// other packages that need the same Proxmox endpoint (image-builder's
// PROXMOX_URL env var, which requires a full "https://host:8006/api2/json"
// per its own docs) don't have to duplicate or guess at this logic.
func NormalizeURL(u string) string {
	return normalizeURL(u)
}

func normalizeURL(u string) string {
	u = strings.TrimSuffix(strings.TrimSpace(u), "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	if !strings.Contains(u[strings.Index(u, "://")+3:], ":") {
		u += ":8006"
	}
	return u
}

// apiError carries the HTTP status and Proxmox's own error payload (which
// names exactly which privilege was missing) so callers can show the user
// something actionable instead of "401 Unauthorized".
type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("proxmox API returned %d: %s", e.Status, e.Body)
}

func IsAuthError(err error) bool {
	ae, ok := err.(*apiError)
	return ok && (ae.Status == 401 || ae.Status == 403)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s=%s", c.tokenID, c.secret))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to proxmox at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	if out == nil {
		return nil
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decoding proxmox response from %s: %w", path, err)
	}
	return json.Unmarshal(envelope.Data, out)
}

// Version confirms the connection works at all — no privileges beyond a
// valid token are required, so this is the very first check to run.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := c.get(ctx, "/version", &v); err != nil {
		return "", err
	}
	return v.Version, nil
}
