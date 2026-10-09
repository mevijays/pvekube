package server

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"pvekube/internal/kubeconfig"
	"pvekube/internal/ui"
)

// Limits are generous for what a kubeconfig is (a few KB) while keeping an
// accidental upload of something huge from being read into memory.
const (
	kubeconfigMaxFiles     = 20
	kubeconfigMaxFileBytes = 2 << 20
	kubeconfigMaxRequest   = 12 << 20
)

// The Kubeconfig utility is stateless on purpose. Kubeconfigs hold admin
// credentials, so nothing uploaded is written to disk or the database, kept in
// memory beyond the request, or logged (withLogging records only the path).
// Validate and Download both take the full upload and redo the work, which
// also means a download can never be produced from files that did not pass
// validation: the server re-checks rather than trusting the page.

func (s *Server) handleKubeconfigPage(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	ui.Render(w, "kubeconfig", map[string]any{"CSRF": s.csrfFor(session)})
}

// kubeconfigRequest is a parsed upload.
type kubeconfigRequest struct {
	sources []kubeconfig.Source
	opts    kubeconfig.Options
}

// readKubeconfigRequest checks CSRF, then reads the uploaded files and pasted
// text. A non-nil problem is a message safe to show the operator.
func (s *Server) readKubeconfigRequest(w http.ResponseWriter, r *http.Request) (*kubeconfigRequest, string, int) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	r.Body = http.MaxBytesReader(w, r.Body, kubeconfigMaxRequest)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		return nil, "the upload could not be read — it may be larger than " + fmt.Sprint(kubeconfigMaxRequest>>20) + " MB", http.StatusRequestEntityTooLarge
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if !s.checkCSRF(r, session) {
		return nil, "bad csrf", http.StatusForbidden
	}

	req := &kubeconfigRequest{opts: kubeconfig.Options{
		OnConflict:     kubeconfig.OnConflictError,
		CurrentContext: strings.TrimSpace(r.FormValue("current_context")),
	}}
	if r.FormValue("on_conflict") == string(kubeconfig.OnConflictRename) {
		req.opts.OnConflict = kubeconfig.OnConflictRename
	}

	seen := map[string]int{}
	add := func(name, content string) {
		name = filepath.Base(name)
		if name == "" || name == "." || name == "/" {
			name = "kubeconfig"
		}
		// Two files with the same name (from different folders) must stay
		// distinguishable in the report and in rename suffixes.
		if seen[name]++; seen[name] > 1 {
			name = fmt.Sprintf("%s (%d)", name, seen[name])
		}
		req.sources = append(req.sources, kubeconfig.Source{Name: name, Content: content})
	}

	if r.MultipartForm != nil {
		headers := r.MultipartForm.File["files"]
		if len(headers) > kubeconfigMaxFiles {
			return nil, fmt.Sprintf("at most %d files can be merged at once", kubeconfigMaxFiles), http.StatusBadRequest
		}
		for _, fh := range headers {
			if fh.Filename == "" {
				continue
			}
			if fh.Size > kubeconfigMaxFileBytes {
				return nil, fmt.Sprintf("%s is larger than %d MB — it does not look like a kubeconfig", filepath.Base(fh.Filename), kubeconfigMaxFileBytes>>20), http.StatusBadRequest
			}
			f, err := fh.Open()
			if err != nil {
				return nil, "could not read " + filepath.Base(fh.Filename), http.StatusBadRequest
			}
			b, err := io.ReadAll(io.LimitReader(f, kubeconfigMaxFileBytes+1))
			f.Close()
			if err != nil {
				return nil, "could not read " + filepath.Base(fh.Filename), http.StatusBadRequest
			}
			add(fh.Filename, string(b))
		}
	}
	if pasted := strings.TrimSpace(r.FormValue("pasted")); pasted != "" {
		if len(pasted) > kubeconfigMaxFileBytes {
			return nil, "the pasted text is too large to be a kubeconfig", http.StatusBadRequest
		}
		add("pasted text", pasted)
	}
	return req, "", 0
}

func (s *Server) handleKubeconfigValidate(w http.ResponseWriter, r *http.Request) {
	req, problem, status := s.readKubeconfigRequest(w, r)
	if req == nil {
		if status == http.StatusForbidden {
			http.Error(w, problem, status)
			return
		}
		// Shown in the report area like any other validation error.
		rep := kubeconfig.Report{Global: []kubeconfig.Finding{{Level: kubeconfig.Error, Msg: problem}}, Errors: 1}
		ui.RenderPartial(w, "kubeconfig_report", map[string]any{"Report": rep})
		return
	}
	rep, _ := kubeconfig.Merge(req.sources, req.opts)
	ui.RenderPartial(w, "kubeconfig_report", map[string]any{"Report": rep, "OnConflict": string(req.opts.OnConflict)})
}

func (s *Server) handleKubeconfigDownload(w http.ResponseWriter, r *http.Request) {
	req, problem, status := s.readKubeconfigRequest(w, r)
	if req == nil {
		http.Error(w, problem, status)
		return
	}
	rep, out := kubeconfig.Merge(req.sources, req.opts)
	if !rep.CanDownload || out == nil {
		http.Error(w, "the files have validation errors — run Validate again to see them", http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="kubeconfig-merged.yaml"`)
	// It holds credentials: keep browsers and proxies from caching it.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(out)
}
