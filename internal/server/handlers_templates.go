package server

import (
	"context"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"pvekube/internal/imagebuilder"
	"pvekube/internal/jobs"
	"pvekube/internal/proxmox"
	"pvekube/internal/ui"
)

type builtTemplateView struct {
	ID         int64
	OSFlavor   string
	K8sVersion string
	Node       string
	VMID       int
	CreatedAt  string
}

func (s *Server) handleTemplatesPage(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	ui.Render(w, "templates", map[string]any{"CSRF": s.csrfFor(session)})
}

func (s *Server) handleTemplatesPanel(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	conn, err := s.getConnection()
	if err != nil {
		ui.RenderPartial(w, "templates_not_connected", nil)
		return
	}
	s.renderTemplatesPanel(w, r.Context(), session, conn, "")
}

func (s *Server) renderTemplatesPanel(w http.ResponseWriter, ctx context.Context, session string, conn *storedConnection, errMsg string) {
	client, err := s.proxmoxClientFor(conn)
	if err != nil {
		ui.RenderPartial(w, "templates_not_connected", nil)
		return
	}
	snap := s.loadCachedDiscovery(conn.ID)
	if snap == nil {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		snap, err = client.Discover(cctx)
		if err != nil {
			ui.RenderPartial(w, "templates_panel", map[string]any{"Error": "Discovery failed: " + err.Error(), "CSRF": s.csrfFor(session), "Flavors": imagebuilder.Flavors, "LinuxOK": runtime.GOOS == "linux", "HostOS": runtime.GOOS})
			return
		}
		s.cacheDiscovery(conn.ID, snap)
	}

	rows, err := s.db.Query(`SELECT id, os_flavor, k8s_version, node, vmid, created_at FROM templates WHERE connection_id = ? ORDER BY id DESC`, conn.ID)
	var built []builtTemplateView
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var t builtTemplateView
			rows.Scan(&t.ID, &t.OSFlavor, &t.K8sVersion, &t.Node, &t.VMID, &t.CreatedAt)
			built = append(built, t)
		}
	}

	ui.RenderPartial(w, "templates_panel", map[string]any{
		"Error":    errMsg,
		"CSRF":     s.csrfFor(session),
		"Flavors":  imagebuilder.Flavors,
		"Snapshot": snap,
		"Built":    built,
		"LinuxOK":  runtime.GOOS == "linux",
		"HostOS":   runtime.GOOS,
	})
}

// templateFormInput is the shared, validated form payload for both the
// validate-only and build actions — same fields, different job.
type templateFormInput struct {
	flavor     imagebuilder.OSFlavor
	k8sVersion string
	env        imagebuilder.ConnEnv
}

func (s *Server) parseTemplateForm(r *http.Request, conn *storedConnection) (templateFormInput, error) {
	r.ParseForm()
	flavorID := r.FormValue("os_flavor")
	var flavor imagebuilder.OSFlavor
	found := false
	for _, f := range imagebuilder.Flavors {
		if f.ID == flavorID {
			flavor, found = f, true
			break
		}
	}
	if !found {
		return templateFormInput{}, errBadInput("unknown OS flavor")
	}

	secret, err := s.sealer.Open(conn.SecretSeal)
	if err != nil {
		return templateFormInput{}, err
	}
	s.redactor.Track(secret)

	env := imagebuilder.ConnEnv{
		URL: proxmox.NormalizeURL(conn.URL), TokenID: conn.TokenID, Secret: secret,
		Node: r.FormValue("node"), Bridge: r.FormValue("bridge"),
		ISOPool: r.FormValue("iso_pool"), StoragePool: r.FormValue("storage_pool"),
	}
	if env.Node == "" || env.Bridge == "" || env.ISOPool == "" || env.StoragePool == "" {
		return templateFormInput{}, errBadInput("node, bridge, ISO pool, and storage pool are all required")
	}

	return templateFormInput{flavor: flavor, k8sVersion: r.FormValue("k8s_version"), env: env}, nil
}

type errBadInput string

func (e errBadInput) Error() string { return string(e) }

func (s *Server) handleTemplatesValidate(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	conn, err := s.getConnection()
	if err != nil {
		ui.RenderPartial(w, "templates_not_connected", nil)
		return
	}
	input, err := s.parseTemplateForm(r, conn)
	if err != nil {
		s.renderTemplatesPanel(w, r.Context(), session, conn, err.Error())
		return
	}

	spec := imagebuilder.ValidateSpec(s.dataDir, input.flavor, input.env)
	jobID, err := s.jobs.Start(spec, `{"kind":"validate"}`)
	if err != nil {
		s.renderTemplatesPanel(w, r.Context(), session, conn, "starting job: "+err.Error())
		return
	}
	ui.RenderPartial(w, "job_progress", map[string]any{
		"JobID": jobID, "Title": spec.Title,
		"WrapperID": "templates-panel", "ReloadURL": "/templates/panel", "ReloadTarget": "#templates-panel",
	})
}

func (s *Server) handleTemplatesBuild(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	conn, err := s.getConnection()
	if err != nil {
		ui.RenderPartial(w, "templates_not_connected", nil)
		return
	}
	input, err := s.parseTemplateForm(r, conn)
	if err != nil {
		s.renderTemplatesPanel(w, r.Context(), session, conn, err.Error())
		return
	}

	client, err := s.proxmoxClientFor(conn)
	if err != nil {
		s.renderTemplatesPanel(w, r.Context(), session, conn, err.Error())
		return
	}
	cctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	// Pre-allocate the VMID ourselves rather than letting Packer pick one
	// implicitly — this is what lets us record a deterministic templates
	// row on success instead of scraping the VMID back out of build logs.
	vmid, err := client.NextVMID(cctx)
	if err != nil {
		s.renderTemplatesPanel(w, r.Context(), session, conn, "allocating VMID: "+err.Error())
		return
	}

	k8sVersion := input.k8sVersion
	if k8sVersion == "" {
		k8sVersion = "image-builder default"
	}

	spec := imagebuilder.BuildSpec(s.dataDir, input.flavor, input.k8sVersion, vmid, input.env)
	connID, node, flavorID := conn.ID, input.env.Node, input.flavor.ID
	spec.Step("Record template", func(c *jobs.Ctx) error {
		_, err := s.db.Exec(`INSERT INTO templates (connection_id, os_flavor, k8s_version, node, vmid, build_job_id) VALUES (?, ?, ?, ?, ?, ?)`,
			connID, flavorID, k8sVersion, node, vmid, c.JobID)
		if err != nil {
			return err
		}
		c.Logf("Recorded template: %s / %s on node %s, VMID %d", flavorID, k8sVersion, node, vmid)
		return nil
	})

	jobID, err := s.jobs.Start(spec, `{"kind":"build","vmid":`+strconv.Itoa(vmid)+`}`)
	if err != nil {
		s.renderTemplatesPanel(w, r.Context(), session, conn, "starting job: "+err.Error())
		return
	}
	ui.RenderPartial(w, "job_progress", map[string]any{
		"JobID": jobID, "Title": spec.Title,
		"WrapperID": "templates-panel", "ReloadURL": "/templates/panel", "ReloadTarget": "#templates-panel",
	})
}
