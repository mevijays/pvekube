package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
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
	s.rememberConnOverride(r)
	ui.Render(w, "templates", map[string]any{"CSRF": s.csrfFor(session)})
}

// handleTemplatesPanel resolves which connection to show — an explicit
// ?conn= wins and is remembered for next time (see activeConnection), so
// switching hosts via the page's <select> is a plain navigation, not a
// dynamic swap. No connections at all is handled by activeConnection
// returning sql.ErrNoRows, same as before multi-host existed.
func (s *Server) handleTemplatesPanel(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	conn, err := s.activeConnection(r.URL.Query().Get("conn"))
	if err != nil {
		ui.RenderPartial(w, "templates_not_connected", nil)
		return
	}
	s.renderTemplatesPanel(w, r.Context(), session, conn, "")
}

func (s *Server) renderTemplatesPanel(w http.ResponseWriter, ctx context.Context, session string, conn *storedConnection, errMsg string) {
	conns, connErr := s.listConnections()
	if connErr != nil {
		conns = nil
	}

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
			ui.RenderPartial(w, "templates_panel", map[string]any{
				"Error": "Discovery failed: " + err.Error(), "CSRF": s.csrfFor(session),
				"Flavors": imagebuilder.Flavors, "LinuxOK": runtime.GOOS == "linux", "HostOS": runtime.GOOS,
				"Connections": conns, "SelectedConn": conn,
			})
			return
		}
		s.cacheDiscovery(conn.ID, snap)
	}

	rows, dbErr := s.db.Query(`SELECT id, os_flavor, k8s_version, node, vmid, created_at FROM templates WHERE connection_id = ? ORDER BY id DESC`, conn.ID)
	var built []builtTemplateView
	if dbErr == nil {
		defer rows.Close()
		var scanErr error
		for rows.Next() {
			var t builtTemplateView
			if scanErr = rows.Scan(&t.ID, &t.OSFlavor, &t.K8sVersion, &t.Node, &t.VMID, &t.CreatedAt); scanErr != nil {
				// stop collecting and surface an error to the panel render below
				break
			}
			built = append(built, t)
		}
		if scanErr == nil {
			scanErr = rows.Err()
		}
		if scanErr != nil {
			// attach DB error text to the panel's error message so the operator
			// sees what went wrong instead of silently losing rows
			if errMsg != "" {
				errMsg = errMsg + "; " + "reading templates: " + scanErr.Error()
			} else {
				errMsg = "reading templates: " + scanErr.Error()
			}
		}
	} else {
		// Query-level error
		if errMsg != "" {
			errMsg = errMsg + "; " + "reading templates: " + dbErr.Error()
		} else {
			errMsg = "reading templates: " + dbErr.Error()
		}
	}

	buildBusy, _ := s.templateBuildInProgress()

	ui.RenderPartial(w, "templates_panel", map[string]any{
		"Error":    errMsg,
		"CSRF":     s.csrfFor(session),
		"Flavors":  imagebuilder.Flavors,
		"Snapshot": snap,
		"Built":    built,
		"LinuxOK":  runtime.GOOS == "linux",
		"HostOS":   runtime.GOOS,
		// Connections + SelectedConn back the host <select> at the top of
		// the panel; every built-template row and every dropdown below it
		// (node/bridge/storage) is scoped to SelectedConn only — there is
		// deliberately no cross-host view here, see the design notes in
		// the multi-host plan.
		"Connections":  conns,
		"SelectedConn": conn,
		// BuildBusy disables the Build button (with an explanatory note)
		// while a template.build job is already running, ANY host — see
		// templateBuildInProgress's doc comment for why concurrent builds
		// aren't safe today.
		"BuildBusy": buildBusy,
	})
}

// templateBuildInProgress reports whether a template.build job is currently
// pending/running, on ANY connection. internal/imagebuilder's Docker-based
// Packer build bind-mounts one dataDir-wide directory
// (imagebuilder.RepoDir) into every build's container and writes per-build
// files (packer.json, the ISO-override JSON) into that same shared
// directory — two concurrent builds race on those files regardless of which
// Proxmox host either one targets. This is a pre-existing bug (nothing ever
// serialized template.validate/build before multi-host support), but
// letting an operator pick a different host specifically invites starting a
// second build "at the same time," so it's fixed alongside this work rather
// than left as a latent trap. DB-backed rather than an in-process mutex so
// it survives a restart mid-build correctly reflecting real job state.
// clustersUsingTemplate names the clusters whose rows still reference a
// template, so deletion can be refused with something actionable rather than
// surfacing a raw foreign-key error after the image is already destroyed.
func (s *Server) clustersUsingTemplate(templateID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM clusters WHERE template_id = ?`, templateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

func (s *Server) templateBuildInProgress() (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind = 'template.build' AND status IN ('pending', 'running')`).Scan(&n)
	return n > 0, err
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
		URL: proxmox.NormalizeURL(conn.URL), TokenID: conn.TokenID, Secret: secret, InsecureTLS: conn.InsecureTLS,
		Node: r.FormValue("node"), Bridge: r.FormValue("bridge"),
		ISOPool: r.FormValue("iso_pool"), StoragePool: r.FormValue("storage_pool"),
	}
	if env.Node == "" || env.Bridge == "" || env.ISOPool == "" || env.StoragePool == "" {
		return templateFormInput{}, errBadInput("node, bridge, ISO pool, and storage pool are all required")
	}

	// LVM-thin/ZFS storage pools reject qcow2 outright — resolve the format
	// the discovery scan already determined for this pool so it's passed
	// through to Packer instead of silently falling back to qcow2.
	if snap := s.loadCachedDiscovery(conn.ID); snap != nil {
		for _, st := range snap.Storage {
			if st.ID == env.StoragePool {
				env.DiskFormat = st.DiskFormat
				break
			}
		}
	}

	return templateFormInput{flavor: flavor, k8sVersion: r.FormValue("k8s_version"), env: env}, nil
}

type errBadInput string

func (e errBadInput) Error() string { return string(e) }

// formConnection resolves the connection a Templates/Clusters POST is
// targeting from its hidden "conn" field — set once when the panel was
// rendered — rather than re-deriving "the active connection" independently
// mid-request. Re-deriving it would be a real race: app_state's
// "remembered" host could change between page load and form submit (a
// second browser tab switching hosts, for instance), silently sending a
// build or apply to the wrong Proxmox server. An explicit field, like the
// CSRF token, means what gets submitted is exactly what the operator saw
// on screen.
func (s *Server) formConnection(r *http.Request) (*storedConnection, error) {
	id, err := strconv.ParseInt(r.FormValue("conn"), 10, 64)
	if err != nil {
		return nil, errBadInput("missing or invalid connection")
	}
	return s.getConnectionByID(id)
}

func (s *Server) handleTemplatesValidate(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	r.ParseForm()
	conn, err := s.formConnection(r)
	if err != nil {
		ui.RenderPartial(w, "templates_not_connected", nil)
		return
	}
	input, err := s.parseTemplateForm(r, conn)
	if err != nil {
		s.renderTemplatesPanel(w, r.Context(), session, conn, err.Error())
		return
	}

	kv, err := s.resolveK8sVersion(r, input.k8sVersion)
	if err != nil {
		s.renderTemplatesPanel(w, r.Context(), session, conn, err.Error())
		return
	}

	spec := imagebuilder.ValidateSpec(s.dataDir, input.flavor, input.env, kv)
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

// resolveK8sVersion turns the optional "Kubernetes version" form field into
// the full set of image-builder variables, checking against upstream that
// the version is actually published as a package.
//
// Blank stays blank — that path keeps using image-builder's own pinned
// default, which is the behaviour every template built so far relied on.
func (s *Server) resolveK8sVersion(r *http.Request, requested string) (imagebuilder.KubernetesVersion, error) {
	if strings.TrimSpace(requested) == "" {
		return imagebuilder.KubernetesVersion{}, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	return imagebuilder.ResolveKubernetesVersion(ctx, requested)
}

func (s *Server) handleTemplatesBuild(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	r.ParseForm()
	conn, err := s.formConnection(r)
	if err != nil {
		ui.RenderPartial(w, "templates_not_connected", nil)
		return
	}

	// Reject a second concurrent build outright rather than let it race with
	// the first — see templateBuildInProgress's doc comment. Checked before
	// parsing/validating the rest of the form so the operator finds out
	// immediately, not after a VMID has already been allocated.
	// Held until the job row exists, so the busy check below cannot be
	// overtaken by a second submission that read the same "not busy".
	s.buildMu.Lock()
	defer s.buildMu.Unlock()

	if busy, _ := s.templateBuildInProgress(); busy {
		s.renderTemplatesPanel(w, r.Context(), session, conn, "a template build is already in progress — wait for it to finish before starting another")
		return
	}

	input, err := s.parseTemplateForm(r, conn)
	if err != nil {
		s.renderTemplatesPanel(w, r.Context(), session, conn, err.Error())
		return
	}

	// Resolved before a VMID is allocated: an unavailable version should
	// cost the operator a few seconds and nothing else, rather than burning
	// a VMID and failing ~25 minutes later at package install.
	kv, err := s.resolveK8sVersion(r, input.k8sVersion)
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

	spec := imagebuilder.BuildSpec(s.dataDir, input.flavor, kv, vmid, input.env)
	connID, node, flavorID, dataDir := conn.ID, input.env.Node, input.flavor.ID, s.dataDir
	// The RESOLVED semver, not the raw form input: what gets recorded has to
	// be what the image will actually contain, because cluster creation and
	// the upgrade flow both trust this column to name the version baked into
	// the template.
	requestedK8sVersion := kv.Semver
	spec.Step("Record template", func(c *jobs.Ctx) error {
		// Resolve the actual semver here, not eagerly before the build starts:
		// the image-builder repo (which config/kubernetes.json lives in) is
		// only guaranteed cloned by this point in the pipeline. Recording a
		// placeholder like "image-builder default" instead of a real semver
		// breaks cluster creation later — clusterctl rejects non-semver
		// --kubernetes-version values outright.
		k8sVersion := requestedK8sVersion
		if k8sVersion == "" {
			resolved, err := imagebuilder.DefaultKubernetesSemver(dataDir)
			if err != nil {
				return fmt.Errorf("resolving image-builder's default Kubernetes version: %w", err)
			}
			k8sVersion = resolved
		}
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

// handleTemplatesDelete removes a built template — both the Proxmox VM and
// PVEKube's own record of it. Synchronous rather than job-based: deleting a
// template VM is quick (Proxmox has no OS to shut down cleanly, just
// storage to reclaim), unlike a multi-minute template build.
func (s *Server) handleTemplatesDelete(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad template id", http.StatusBadRequest)
		return
	}

	var connID, vmid int64
	var node string
	if err := s.db.QueryRow(`SELECT connection_id, node, vmid FROM templates WHERE id = ?`, id).Scan(&connID, &node, &vmid); err != nil {
		http.Error(w, "template not found", http.StatusNotFound)
		return
	}

	// Look up the CONNECTION THIS TEMPLATE ACTUALLY BELONGS TO, not "whatever
	// host happens to be active right now" — those are no longer the same
	// thing once more than one connection exists. The old single-connection
	// version of this check (comparing against s.getConnection()) would
	// incorrectly reject deleting a template that belongs to any host other
	// than the currently-selected one.
	conn, err := s.getConnectionByID(connID)
	if err != nil {
		http.Error(w, "this template's Proxmox connection has been disconnected — reconnect it to delete this template's VM", http.StatusBadRequest)
		return
	}
	client, err := s.proxmoxClientFor(conn)
	if err != nil {
		s.renderTemplatesPanel(w, r.Context(), session, conn, err.Error())
		return
	}

	// Refused BEFORE anything is destroyed, because deleting the Proxmox VM
	// is irreversible and the database check that would have stopped it comes
	// after. clusters.template_id references templates(id) with no ON DELETE
	// clause, so removing a template a cluster still uses fails on the
	// foreign key — but only after the image is already gone. The old
	// ordering therefore produced the worst of both: the template image
	// destroyed on Proxmox (so the cluster can no longer scale or upgrade)
	// AND the row still present, reported as "VM deleted on Proxmox, but
	// removing the local record failed".
	if users, err := s.clustersUsingTemplate(id); err != nil {
		s.renderTemplatesPanel(w, r.Context(), session, conn, "checking whether any cluster uses this template: "+err.Error())
		return
	} else if len(users) > 0 {
		s.renderTemplatesPanel(w, r.Context(), session, conn, fmt.Sprintf(
			"this template is still used by %s — deleting it would leave that cluster unable to scale or upgrade. Delete the cluster first.",
			strings.Join(users, ", ")))
		return
	}

	cctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := client.DeleteVM(cctx, node, int(vmid)); err != nil {
		// A template removed directly in Proxmox leaves PVEKube's row behind,
		// and the delete then fails with Proxmox's "Configuration file
		// 'nodes/<node>/qemu-server/<vmid>.conf' does not exist" (an HTTP 500,
		// not a 404 — see VMExists). Before this check the handler returned
		// here, so the local record could never be removed through the UI at
		// all: the row was permanently stuck with no way to clear it short of
		// editing the database by hand.
		//
		// Re-checking rather than pattern-matching the error text keeps a real
		// failure (permissions, host down, VM locked by a running task) from
		// being silently swallowed as "already gone" — only a VM Proxmox
		// positively reports as absent falls through to removing the record.
		exists, existsErr := client.VMExists(cctx, node, int(vmid))
		if existsErr != nil || exists {
			s.renderTemplatesPanel(w, r.Context(), session, conn, fmt.Sprintf("deleting VM %d on Proxmox: %s", vmid, err.Error()))
			return
		}
		slog.Info("template VM was already gone from Proxmox; removing the stale local record",
			"vmid", vmid, "node", node, "template_id", id)
	}

	if _, err := s.db.Exec(`DELETE FROM templates WHERE id = ?`, id); err != nil {
		s.renderTemplatesPanel(w, r.Context(), session, conn, "VM deleted on Proxmox, but removing the local record failed: "+err.Error())
		return
	}

	s.renderTemplatesPanel(w, r.Context(), session, conn, "")
}
