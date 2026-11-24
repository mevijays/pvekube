package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"pvekube/internal/capi"
	"pvekube/internal/ui"
)

// resizeRoleView is one role's current size, shaped for the form. Zero fields
// mean the machine template does not set them (the VM then takes the Proxmox
// template's value), which the form says instead of showing a misleading "0".
type resizeRoleView struct {
	Prefix       string // form field prefix: "cp" or "w"
	Label        string
	Replicas     int
	TemplateName string
	Sockets      int
	Cores        int
	MemoryMiB    int
	MemoryGiB    string // "25" or "12.5": the form's unit
	Summary      string
	// What the operator typed, put back after a validation error.
	InMemory, InCores, InSockets string
}

func newResizeRoleView(prefix, label string, cs capi.CurrentSize, posted url.Values) resizeRoleView {
	v := resizeRoleView{
		Prefix: prefix, Label: label, Replicas: cs.Replicas, TemplateName: cs.TemplateName,
		InMemory: posted.Get(prefix + "_memory_gib"), InCores: posted.Get(prefix + "_cores"), InSockets: posted.Get(prefix + "_sockets"),
		Sockets: cs.NumSockets, Cores: cs.NumCores, MemoryMiB: cs.MemoryMiB,
	}
	if cs.MemoryMiB > 0 {
		v.MemoryGiB = strconv.FormatFloat(float64(cs.MemoryMiB)/1024, 'f', -1, 64)
	}
	var parts []string
	if cs.NumSockets > 0 && cs.NumCores > 0 {
		parts = append(parts, fmt.Sprintf("%d socket(s) × %d core(s)", cs.NumSockets, cs.NumCores))
	}
	if cs.MemoryMiB > 0 {
		parts = append(parts, capi.FormatMiB(cs.MemoryMiB)+" RAM")
	}
	if len(parts) == 0 {
		parts = append(parts, "size comes from the Proxmox template (not set on the machine template)")
	}
	v.Summary = strings.Join(parts, " · ")
	return v
}

// parseResizeForm turns the posted form into a ResizeInput. RAM is entered in
// GiB (decimals allowed) because that is how operators think about it; a blank
// field means "leave unchanged". Every problem is reported rather than
// silently ignored, so a typo can never resize something unintended.
func parseResizeForm(name string, form url.Values) (capi.ResizeInput, error) {
	in := capi.ResizeInput{ClusterName: name}

	field := func(key string) (string, bool) {
		v := strings.TrimSpace(form.Get(key))
		return v, v != ""
	}
	intField := func(key, label string) (int, error) {
		v, ok := field(key)
		if !ok {
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%s must be a whole number greater than 0", label)
		}
		return n, nil
	}
	gibField := func(key, label string) (int, error) {
		v, ok := field(key)
		if !ok {
			return 0, nil
		}
		g, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(g) || math.IsInf(g, 0) || g <= 0 {
			return 0, fmt.Errorf("%s must be a number of GiB greater than 0", label)
		}
		return int(math.Round(g * 1024)), nil
	}

	var err error
	for _, f := range []struct {
		dst   *int
		parse func() (int, error)
	}{
		{&in.ControlPlane.MemoryMiB, func() (int, error) { return gibField("cp_memory_gib", "Control plane RAM") }},
		{&in.ControlPlane.NumCores, func() (int, error) { return intField("cp_cores", "Control plane cores") }},
		{&in.ControlPlane.NumSockets, func() (int, error) { return intField("cp_sockets", "Control plane sockets") }},
		{&in.Workers.MemoryMiB, func() (int, error) { return gibField("w_memory_gib", "Worker RAM") }},
		{&in.Workers.NumCores, func() (int, error) { return intField("w_cores", "Worker cores") }},
		{&in.Workers.NumSockets, func() (int, error) { return intField("w_sockets", "Worker sockets") }},
	} {
		if *f.dst, err = f.parse(); err != nil {
			return in, err
		}
	}

	if err := capi.ValidateResize(in); err != nil {
		return in, err
	}
	// The replacement destroys node-local disks, so it is never started
	// without an explicit acknowledgement — enforced here, not just in the UI.
	if form.Get("ack") != "on" {
		return in, fmt.Errorf("tick the box to confirm you understand that every node is replaced and anything stored on a node's own disks is lost")
	}
	return in, nil
}

// rolloutBlocker refuses an upgrade or resize while the cluster is still
// replacing machines (or recovering from a rollout). Starting a second rollout
// on top of a running one leaves machines on three different templates, so
// this is checked before every such operation, and fails closed when the
// cluster state cannot be read.
func (s *Server) rolloutBlocker(r *http.Request, name, what string) string {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	return capi.RolloutBlocker(ctx, s.dataDir, s.binDir, name, what)
}

// renderResizePanel builds the resize form with each role's live size.
func (s *Server) renderResizePanel(w http.ResponseWriter, r *http.Request, name, errMsg string, posted url.Values) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	data := map[string]any{"ClusterName": name, "CSRF": s.csrfFor(session), "Error": errMsg}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cp, workers, err := capi.CurrentSizing(ctx, s.dataDir, s.binDir, name)
	if err != nil {
		data["Error"] = err.Error()
		ui.RenderPartial(w, "cluster_resize", data)
		return
	}
	data["ControlPlane"] = newResizeRoleView("cp", "Control plane", cp, posted)
	data["Workers"] = newResizeRoleView("w", "Workers", workers, posted)
	if errMsg == "" {
		if msg := s.rolloutBlocker(r, name, "resize"); msg != "" {
			data["Error"] = msg
			delete(data, "ControlPlane") // no form to offer while the cluster is busy
		}
	}
	ui.RenderPartial(w, "cluster_resize", data)
}

func (s *Server) handleClusterResizeForm(w http.ResponseWriter, r *http.Request) {
	s.renderResizePanel(w, r, r.PathValue("name"), "", nil)
}

func (s *Server) handleClusterResize(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	name := r.PathValue("name")
	if !s.checkCSRF(r, session) {
		s.renderResizePanel(w, r, name, "Security token mismatch — please refresh the page and try again.", nil)
		return
	}
	r.ParseForm()

	var known int
	if err := s.db.QueryRow(`SELECT 1 FROM clusters WHERE name = ?`, name).Scan(&known); err != nil {
		s.renderResizePanel(w, r, name, "PVEKube has no record of this cluster.", r.Form)
		return
	}

	in, err := parseResizeForm(name, r.Form)
	if err != nil {
		s.renderResizePanel(w, r, name, err.Error(), r.Form)
		return
	}

	if msg := s.rolloutBlocker(r, name, "resize"); msg != "" {
		s.renderResizePanel(w, r, name, msg, r.Form)
		return
	}

	spec := s.attachWorkerDisks(name, capi.ResizeSpec(s.dataDir, s.binDir, in))
	jobID, err := s.jobs.StartExclusive(spec, `{"cluster":"`+name+`"}`, clusterOperationLock(name))
	if err != nil {
		s.renderResizePanel(w, r, name, clusterOperationError(err), r.Form)
		return
	}
	// Same placement as the upgrade: progress renders into the panel outside
	// the self-polling #cluster-status, and "OK" refreshes the status.
	ui.RenderPartial(w, "job_progress", map[string]any{
		"JobID": jobID, "Title": spec.Title,
		"WrapperID": "cluster-upgrade-panel", "ReloadURL": "/clusters/" + name + "/status", "ReloadTarget": "#cluster-status",
	})
}
