package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pvekube/internal/capi"
	"pvekube/internal/jobs"
	"pvekube/internal/ui"
)

func (s *Server) handleClusterDetailPage(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	name := r.PathValue("name")
	ui.Render(w, "cluster_detail", map[string]any{"CSRF": s.csrfFor(session), "ClusterName": name})
}

func (s *Server) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	status, err := capi.GetStatus(ctx, s.dataDir, s.binDir, name)
	if err != nil {
		ui.RenderPartial(w, "cluster_status", map[string]any{
			"ClusterName": name,
			"CSRF":        s.csrfFor(session),
			"Status":      &capi.ClusterStatus{Found: false},
		})
		return
	}

	kubeconfigReady := false
	if status.Found {
		if _, err := capi.GetWorkloadKubeconfig(ctx, s.dataDir, s.binDir, name); err == nil {
			kubeconfigReady = true
		}
	}

	// Reflect the observed phase back into our own record — the DB row is
	// created with status='provisioning' at apply time and otherwise never
	// updated, so the clusters list would show a stale status forever.
	if status.Found && status.Phase != "" {
		s.db.Exec(`UPDATE clusters SET status = ? WHERE name = ?`, status.Phase, name)
	}

	ui.RenderPartial(w, "cluster_status", map[string]any{
		"ClusterName":     name,
		"CSRF":            s.csrfFor(session),
		"Status":          status,
		"KubeconfigReady": kubeconfigReady,
		// Surfaces a known, accepted limitation directly where an operator
		// would otherwise be confused by it: certain OS images (Flatcar,
		// via Ignition — see internal/capi/ignition.go's package doc
		// comment) never get kubelet's --provider-id resolved, which pins
		// these specific CAPI conditions NotReady/the Cluster phase at
		// "Provisioned" forever even once the cluster is fully healthy.
		// Confirmed live: patching it after the fact is impossible, not
		// just unfixed — Kubernetes' API server rejects changing a
		// non-empty providerID outright.
		"ProviderIDKnownIssue": hasProviderIDCondition(status.Conditions),
	})
}

func hasProviderIDCondition(conditions []capi.ConditionView) bool {
	for _, c := range conditions {
		if strings.Contains(c.Message, "spec.providerID") {
			return true
		}
	}
	return false
}

// runLifecycleJob starts a job and reloads back into the cluster status panel
// (not a list) since that's the page the operator is already looking at.
func (s *Server) runLifecycleJob(w http.ResponseWriter, name string, spec *jobs.Spec) {
	jobID, err := s.jobs.Start(spec, `{"cluster":"`+name+`"}`)
	if err != nil {
		http.Error(w, "starting job: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ui.RenderPartial(w, "job_progress", map[string]any{
		"JobID": jobID, "Title": spec.Title,
		"WrapperID": "cluster-status", "ReloadURL": "/clusters/" + name + "/status", "ReloadTarget": "#cluster-status",
	})
}

// runDeleteJob is like runLifecycleJob but the cluster will be gone when the
// job completes, so the "OK" button navigates to /clusters rather than
// trying to reload the deleted cluster's status page.
func (s *Server) runDeleteJob(w http.ResponseWriter, name string, spec *jobs.Spec) {
	jobID, err := s.jobs.Start(spec, `{"cluster":"`+name+`"}`)
	if err != nil {
		http.Error(w, "starting job: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ui.RenderPartial(w, "job_progress", map[string]any{
		"JobID":        jobID,
		"Title":        spec.Title,
		"WrapperID":    "cluster-status",
		"ReloadURL":    "/clusters/panel",
		"ReloadTarget": "#clusters-panel",
		"IsDeleteJob":  true,
	})
}

func (s *Server) handleClusterScaleWorkers(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	name := r.PathValue("name")
	r.ParseForm()
	replicas, err := strconv.Atoi(r.FormValue("replicas"))
	if err != nil || replicas < 0 {
		http.Error(w, "invalid replica count", http.StatusBadRequest)
		return
	}
	s.runLifecycleJob(w, name, capi.ScaleWorkersSpec(s.dataDir, s.binDir, name, replicas))
}

func (s *Server) handleClusterScaleControlPlane(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	name := r.PathValue("name")
	r.ParseForm()
	replicas, err := strconv.Atoi(r.FormValue("replicas"))
	if err != nil {
		http.Error(w, "invalid replica count", http.StatusBadRequest)
		return
	}
	s.runLifecycleJob(w, name, capi.ScaleControlPlaneSpec(s.dataDir, s.binDir, name, replicas))
}

func (s *Server) handleClusterDelete(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		ui.RenderPartial(w, "cluster_delete_error", map[string]any{
			"ClusterName": r.PathValue("name"),
			"Error":       "Security token mismatch — please refresh the page and try again.",
		})
		return
	}
	name := r.PathValue("name")
	s.db.Exec(`UPDATE clusters SET status = 'deleting' WHERE name = ?`, name)

	spec := capi.DeleteClusterSpec(s.dataDir, s.binDir, name)
	spec.Step("Remove local record", func(c *jobs.Ctx) error {
		c.Logf("Removing cluster record from local database")
		_, err := s.db.Exec(`DELETE FROM clusters WHERE name = ?`, name)
		return err
	})
	s.runDeleteJob(w, name, spec)
}

func (s *Server) handleClusterKubeconfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	kc, err := capi.GetWorkloadKubeconfig(ctx, s.dataDir, s.binDir, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`-kubeconfig.yaml"`)
	w.Write(kc)
}

// upgradeTemplateView is one candidate template on the upgrade screen.
// Ineligible entries are still shown (greyed out, with Reason) rather than
// hidden: an operator whose only other template is a downgrade or a
// two-minor jump needs to see WHY it isn't offered, otherwise the list just
// looks mysteriously empty.
type upgradeTemplateView struct {
	ID         int64
	OSFlavor   string
	K8sVersion string
	Node       string
	VMID       int
	Eligible   bool
	Reason     string
}

// renderUpgradePanel builds the upgrade form: the cluster's live Kubernetes
// version plus every template on the SAME Proxmox connection, each already
// checked against kubeadm's skew rules so the UI can disable the ones that
// would fail.
//
// Templates are scoped to the cluster's own connection because a
// ProxmoxMachineTemplate addresses its image by (sourceNode, templateID) —
// both only meaningful on the Proxmox host the cluster actually runs on.
func (s *Server) renderUpgradePanel(w http.ResponseWriter, r *http.Request, name, errMsg string) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	data := map[string]any{"ClusterName": name, "CSRF": s.csrfFor(session), "Error": errMsg}

	var connID int64
	if err := s.db.QueryRow(`SELECT connection_id FROM clusters WHERE name = ?`, name).Scan(&connID); err != nil {
		data["Error"] = "PVEKube has no record of this cluster, so it can't tell which Proxmox host its templates live on."
		ui.RenderPartial(w, "cluster_upgrade", data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	current, err := capi.CurrentVersion(ctx, s.dataDir, s.binDir, name)
	if err != nil {
		data["Error"] = err.Error()
		ui.RenderPartial(w, "cluster_upgrade", data)
		return
	}
	data["CurrentVersion"] = current

	var views []upgradeTemplateView
	anyEligible := false
	for _, t := range s.listTemplates(connID) {
		v := upgradeTemplateView{ID: t.ID, OSFlavor: t.OSFlavor, K8sVersion: t.K8sVersion, Node: t.Node, VMID: t.VMID}
		if err := capi.ValidateUpgrade(current, t.K8sVersion); err != nil {
			v.Reason = err.Error()
		} else {
			v.Eligible = true
			anyEligible = true
		}
		views = append(views, v)
	}
	data["Templates"] = views
	data["AnyEligible"] = anyEligible
	ui.RenderPartial(w, "cluster_upgrade", data)
}

func (s *Server) handleClusterUpgradeForm(w http.ResponseWriter, r *http.Request) {
	s.renderUpgradePanel(w, r, r.PathValue("name"), "")
}

func (s *Server) handleClusterUpgrade(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	name := r.PathValue("name")
	if !s.checkCSRF(r, session) {
		s.renderUpgradePanel(w, r, name, "Security token mismatch — please refresh the page and try again.")
		return
	}
	r.ParseForm()

	templateID, err := strconv.ParseInt(r.FormValue("template_id"), 10, 64)
	if err != nil {
		s.renderUpgradePanel(w, r, name, "Pick a template to upgrade to.")
		return
	}

	var connID int64
	if err := s.db.QueryRow(`SELECT connection_id FROM clusters WHERE name = ?`, name).Scan(&connID); err != nil {
		s.renderUpgradePanel(w, r, name, "PVEKube has no record of this cluster.")
		return
	}

	// Re-read the template from the DB scoped to the cluster's own
	// connection rather than trusting the posted id: a template on another
	// Proxmox host would resolve to a (sourceNode, templateID) pair that
	// doesn't exist where this cluster's VMs actually run.
	var tmpl templateOptionView
	if err := s.db.QueryRow(`SELECT id, os_flavor, k8s_version, node, vmid FROM templates WHERE id = ? AND connection_id = ?`,
		templateID, connID).Scan(&tmpl.ID, &tmpl.OSFlavor, &tmpl.K8sVersion, &tmpl.Node, &tmpl.VMID); err != nil {
		s.renderUpgradePanel(w, r, name, "That template no longer exists on this cluster's Proxmox host.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	current, err := capi.CurrentVersion(ctx, s.dataDir, s.binDir, name)
	if err != nil {
		s.renderUpgradePanel(w, r, name, err.Error())
		return
	}

	// Validated again here, not just when the form was rendered: the
	// version check that greyed out an option happened against whatever the
	// cluster's version was at page load, and an upgrade may have completed
	// in another tab since.
	if err := capi.ValidateUpgrade(current, tmpl.K8sVersion); err != nil {
		s.renderUpgradePanel(w, r, name, err.Error())
		return
	}

	spec := capi.UpgradeSpec(s.dataDir, s.binDir, capi.UpgradeInput{
		ClusterName:     name,
		CurrentVersion:  current,
		NewVersion:      tmpl.K8sVersion,
		NewSourceNode:   tmpl.Node,
		NewTemplateVMID: tmpl.VMID,
	})
	jobID, err := s.jobs.Start(spec, `{"cluster":"`+name+`"}`)
	if err != nil {
		s.renderUpgradePanel(w, r, name, "starting job: "+err.Error())
		return
	}
	// Progress renders into the upgrade panel (outside #cluster-status, which
	// self-polls every 5s and would otherwise wipe it); "OK" clears it and
	// refreshes the status panel to show the new version.
	ui.RenderPartial(w, "job_progress", map[string]any{
		"JobID": jobID, "Title": spec.Title,
		"WrapperID": "cluster-upgrade-panel", "ReloadURL": "/clusters/" + name + "/status", "ReloadTarget": "#cluster-status",
	})
}
