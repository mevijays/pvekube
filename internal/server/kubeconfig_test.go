package server

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const kcTestSession = "sess"
const kcTestCSRF = "csrf-ok"

func kcServer() *Server {
	return &Server{csrfBySess: map[string]string{kcTestSession: kcTestCSRF}}
}

// tokenKubeconfig is a kubeadm-style file using a bearer token, so the tests
// need no certificate material.
func tokenKubeconfig(server, token string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: kubernetes
  cluster: {server: %s}
users:
- name: kubernetes-admin
  user: {token: %s}
contexts:
- name: kubernetes-admin@kubernetes
  context: {cluster: kubernetes, user: kubernetes-admin}
current-context: kubernetes-admin@kubernetes
`, server, token)
}

// kcRequest builds a multipart POST like the page's form does.
func kcRequest(t *testing.T, path, csrf string, fields map[string]string, files map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("csrf", csrf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for name, content := range files {
		fw, _ := mw.CreateFormFile("files", name)
		fw.Write([]byte(content))
	}
	mw.Close()
	r := httptest.NewRequest("POST", path, &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r.WithContext(context.WithValue(r.Context(), ctxSessionKey{}, kcTestSession))
}

func twoClashingFiles() map[string]string {
	return map[string]string{
		"prod.yaml": tokenKubeconfig("https://10.0.0.1:6443", "tok-prod"),
		"dev.yaml":  tokenKubeconfig("https://10.0.0.2:6443", "tok-dev"),
	}
}

func TestKubeconfigValidateReportsClashAndWithholdsDownload(t *testing.T) {
	rec := httptest.NewRecorder()
	kcServer().handleKubeconfigValidate(rec, kcRequest(t, "/kubeconfig/validate", kcTestCSRF, nil, twoClashingFiles()))

	body := rec.Body.String()
	if !strings.Contains(body, "different settings") || !strings.Contains(body, "must be fixed") {
		t.Errorf("the clash should be reported as blocking errors:\n%s", body)
	}
	if strings.Contains(body, "data-kc-download") {
		t.Error("no download button may be offered while there are errors")
	}
	// The report names things; it must never echo a credential back.
	if strings.Contains(body, "tok-prod") || strings.Contains(body, "tok-dev") {
		t.Error("the report leaked a token")
	}
}

func TestKubeconfigValidateOffersDownloadAfterRename(t *testing.T) {
	rec := httptest.NewRecorder()
	kcServer().handleKubeconfigValidate(rec, kcRequest(t, "/kubeconfig/validate", kcTestCSRF,
		map[string]string{"on_conflict": "rename"}, twoClashingFiles()))

	body := rec.Body.String()
	if !strings.Contains(body, "Validation passed") || !strings.Contains(body, "data-kc-download") {
		t.Errorf("rename should produce a passing report with a download button:\n%s", body)
	}
	if !strings.Contains(body, "kubernetes-admin@kubernetes-dev") {
		t.Errorf("the renamed context should be listed:\n%s", body)
	}
}

func TestKubeconfigDownloadIsGatedOnValidation(t *testing.T) {
	s := kcServer()

	// Errors: the server refuses even though the client asked for a download.
	rec := httptest.NewRecorder()
	s.handleKubeconfigDownload(rec, kcRequest(t, "/kubeconfig/download", kcTestCSRF, nil, twoClashingFiles()))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 for files that fail validation", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "apiVersion") {
		t.Error("a file must not be produced for files that fail validation")
	}

	// Clean: an attachment that must not be cached, since it holds credentials.
	rec = httptest.NewRecorder()
	s.handleKubeconfigDownload(rec, kcRequest(t, "/kubeconfig/download", kcTestCSRF,
		map[string]string{"on_conflict": "rename"}, twoClashingFiles()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "kubeconfig-merged.yaml") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "tok-prod") || !strings.Contains(out, "tok-dev") {
		t.Error("the merged file must keep BOTH clusters' credentials")
	}
}

func TestKubeconfigRejectsBadCSRF(t *testing.T) {
	s := kcServer()
	for name, h := range map[string]http.HandlerFunc{"validate": s.handleKubeconfigValidate, "download": s.handleKubeconfigDownload} {
		rec := httptest.NewRecorder()
		h(rec, kcRequest(t, "/kubeconfig/"+name, "wrong", nil, twoClashingFiles()))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with a bad CSRF token: status %d, want 403", name, rec.Code)
		}
	}
}

func TestKubeconfigPastedTextAndLimits(t *testing.T) {
	s := kcServer()

	rec := httptest.NewRecorder()
	s.handleKubeconfigValidate(rec, kcRequest(t, "/kubeconfig/validate", kcTestCSRF,
		map[string]string{"pasted": tokenKubeconfig("https://10.0.0.1:6443", "x")}, nil))
	if !strings.Contains(rec.Body.String(), "pasted text") || !strings.Contains(rec.Body.String(), "Validation passed") {
		t.Errorf("pasted text should be validated as its own source:\n%s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.handleKubeconfigValidate(rec, kcRequest(t, "/kubeconfig/validate", kcTestCSRF, nil, nil))
	if !strings.Contains(rec.Body.String(), "no kubeconfig files were provided") {
		t.Errorf("an empty submission should say so:\n%s", rec.Body.String())
	}

	many := map[string]string{}
	for i := 0; i <= kubeconfigMaxFiles; i++ {
		many[fmt.Sprintf("f%d.yaml", i)] = tokenKubeconfig("https://h:6443", "x")
	}
	rec = httptest.NewRecorder()
	s.handleKubeconfigValidate(rec, kcRequest(t, "/kubeconfig/validate", kcTestCSRF, nil, many))
	if !strings.Contains(rec.Body.String(), "at most") {
		t.Errorf("too many files should be rejected with a message:\n%s", rec.Body.String())
	}
}
