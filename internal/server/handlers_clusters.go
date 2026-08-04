package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"pvekube/internal/capi"
	"pvekube/internal/ipplan"
	"pvekube/internal/proxmox"
	"pvekube/internal/ui"
)

type templateOptionView struct {
	ID         int64
	OSFlavor   string
	K8sVersion string
	Node       string
	VMID       int
}

type clusterListView struct {
	Name      string
	Status    string
	CreatedAt string
}

func (s *Server) handleClustersPage(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	ui.Render(w, "clusters", map[string]any{"CSRF": s.csrfFor(session)})
}

func (s *Server) handleClustersPanel(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	conn, err := s.getConnection()
	if err != nil {
		ui.RenderPartial(w, "clusters_not_connected", nil)
		return
	}
	s.renderClustersPanel(w, r.Context(), session, conn, "")
}

func (s *Server) renderClustersPanel(w http.ResponseWriter, ctx context.Context, session string, conn *storedConnection, errMsg string) {
	client, err := s.proxmoxClientFor(conn)
	if err != nil {
		ui.RenderPartial(w, "clusters_not_connected", nil)
		return
	}
	snap := s.loadCachedDiscovery(conn.ID)
	if snap == nil {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		snap, err = client.Discover(cctx)
		if err != nil {
			ui.RenderPartial(w, "clusters_panel", map[string]any{"Error": "Discovery failed: " + err.Error(), "CSRF": s.csrfFor(session)})
			return
		}
		s.cacheDiscovery(conn.ID, snap)
	}

	templates := s.listTemplates(conn.ID)

	ui.RenderPartial(w, "clusters_panel", map[string]any{
		"Error":     errMsg,
		"CSRF":      s.csrfFor(session),
		"Snapshot":  snap,
		"Templates": templates,
	})
}

// handleClustersList renders just the cluster table, refreshed live against
// the management cluster on every call. It's polled every 5s (see
// clusters_list.html's own hx-trigger) so the Clusters list reflects real
// provisioning progress without anyone needing to open a cluster's detail
// page first — before this, the DB's status column only got updated by
// handleClusterStatus, so a cluster nobody had clicked into would show
// "provisioning" forever even after it finished.
func (s *Server) handleClustersList(w http.ResponseWriter, r *http.Request) {
	conn, err := s.getConnection()
	if err != nil {
		ui.RenderPartial(w, "clusters_list", map[string]any{"Clusters": nil})
		return
	}

	rows, err := s.db.Query(`SELECT name, status, created_at FROM clusters WHERE connection_id = ? ORDER BY id DESC`, conn.ID)
	var names []clusterListView
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var c clusterListView
			rows.Scan(&c.Name, &c.Status, &c.CreatedAt)
			names = append(names, c)
		}
	}

	// Refresh each cluster's phase concurrently — a handful of `kubectl get`
	// calls, cheap and bounded, fine at homelab scale.
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := range names {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			status, err := capi.GetStatus(cctx, s.dataDir, s.binDir, names[idx].Name)
			if err != nil || !status.Found || status.Phase == "" {
				return
			}
			mu.Lock()
			names[idx].Status = status.Phase
			mu.Unlock()
			s.db.Exec(`UPDATE clusters SET status = ? WHERE name = ?`, status.Phase, names[idx].Name)
		}(i)
	}
	wg.Wait()

	ui.RenderPartial(w, "clusters_list", map[string]any{"Clusters": names})
}

func (s *Server) listTemplates(connID int64) []templateOptionView {
	rows, err := s.db.Query(`SELECT id, os_flavor, k8s_version, node, vmid FROM templates WHERE connection_id = ? ORDER BY id DESC`, connID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []templateOptionView
	for rows.Next() {
		var t templateOptionView
		rows.Scan(&t.ID, &t.OSFlavor, &t.K8sVersion, &t.Node, &t.VMID)
		out = append(out, t)
	}
	return out
}

// clusterForm is the parsed, typed form payload shared by check-ip, preview,
// and apply — one parser, three consumers, so the fields can't drift.
type clusterForm struct {
	name                 string
	templateID           int64
	template             templateOptionView
	controlPlaneCount    int
	workerCount          int
	cni                  capi.CNIFlavor
	bridge               string
	numSockets           int
	numCores             int
	memoryMiB            int
	bootVolumeSize       int
	gateway              string
	ipPrefix             int
	controlPlaneEndpoint string
	nodeIPRange          string
	dnsServers           []string
	vmSSHKeys            []string
	allowedNodes         []string
	addons               capi.AddonSelection
}

func (s *Server) parseClusterForm(r *http.Request, connID int64) (clusterForm, error) {
	r.ParseForm()
	f := clusterForm{
		name:                 strings.TrimSpace(r.FormValue("name")),
		controlPlaneCount:    atoiDefault(r.FormValue("control_plane_count"), 1),
		workerCount:          atoiDefault(r.FormValue("worker_count"), 0),
		cni:                  capi.CNIFlavor(r.FormValue("cni")),
		bridge:               r.FormValue("bridge"),
		numSockets:           atoiDefault(r.FormValue("num_sockets"), 1),
		numCores:             atoiDefault(r.FormValue("num_cores"), 2),
		memoryMiB:            atoiDefault(r.FormValue("memory_mib"), 4096),
		bootVolumeSize:       atoiDefault(r.FormValue("boot_volume_size"), 100),
		gateway:              r.FormValue("gateway"),
		ipPrefix:             atoiDefault(r.FormValue("ip_prefix"), 24),
		controlPlaneEndpoint: r.FormValue("control_plane_endpoint_ip"),
		nodeIPRange:          r.FormValue("node_ip_range"),
		allowedNodes:         r.Form["allowed_nodes"],
		addons: capi.AddonSelection{
			MetricsServer: r.FormValue("install_metrics_server") == "1",
			Istio:         r.FormValue("install_istio") == "1",
			MetalLB:       r.FormValue("install_metallb") == "1",
			MetalLBIPPool: strings.TrimSpace(r.FormValue("metallb_ip_pool")),
		},
	}
	if f.name == "" {
		return f, errBadInput("cluster name is required")
	}
	if f.addons.MetalLB && f.addons.MetalLBIPPool == "" {
		return f, errBadInput("MetalLB is checked but no IP pool was given")
	}
	for _, s := range strings.Split(r.FormValue("dns_servers"), ",") {
		if t := strings.TrimSpace(s); t != "" {
			f.dnsServers = append(f.dnsServers, t)
		}
	}
	for _, s := range strings.Split(r.FormValue("vm_ssh_keys"), ",") {
		if t := strings.TrimSpace(s); t != "" {
			f.vmSSHKeys = append(f.vmSSHKeys, t)
		}
	}

	tid, err := strconv.ParseInt(r.FormValue("template_id"), 10, 64)
	if err != nil {
		return f, errBadInput("a template must be selected")
	}
	f.templateID = tid
	row := s.db.QueryRow(`SELECT id, os_flavor, k8s_version, node, vmid FROM templates WHERE id = ? AND connection_id = ?`, tid, connID)
	if err := row.Scan(&f.template.ID, &f.template.OSFlavor, &f.template.K8sVersion, &f.template.Node, &f.template.VMID); err != nil {
		return f, errBadInput("selected template no longer exists")
	}
	return f, nil
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func (f clusterForm) ipPlan() ipplan.Plan {
	return ipplan.Plan{
		Gateway: f.gateway, PrefixLen: f.ipPrefix, DNSServers: f.dnsServers,
		NodeIPRange: f.nodeIPRange, ControlPlaneEndpoint: f.controlPlaneEndpoint,
		MachineCount: f.controlPlaneCount + f.workerCount,
	}
}

func (s *Server) handleClustersCheckIP(w http.ResponseWriter, r *http.Request) {
	conn, err := s.getConnection()
	if err != nil {
		http.Error(w, "not connected", http.StatusBadRequest)
		return
	}
	f, err := s.parseClusterForm(r, conn.ID)
	if err != nil {
		ui.RenderPartial(w, "ip_plan_issues", map[string]any{"Issues": []ipplan.Issue{{Field: "form", Severity: ipplan.SeverityError, Message: err.Error()}}})
		return
	}
	issues := ipplan.Validate(f.ipPlan())
	ui.RenderPartial(w, "ip_plan_issues", map[string]any{"Issues": issues})
}

func (s *Server) handleClustersPreview(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	conn, err := s.getConnection()
	if err != nil {
		http.Error(w, "not connected", http.StatusBadRequest)
		return
	}
	f, err := s.parseClusterForm(r, conn.ID)
	if err != nil {
		ui.RenderPartial(w, "cluster_preview", map[string]any{"Error": err.Error()})
		return
	}

	secret, err := s.sealer.Open(conn.SecretSeal)
	if err != nil {
		ui.RenderPartial(w, "cluster_preview", map[string]any{"Error": err.Error()})
		return
	}
	s.redactor.Track(secret)

	in := capi.GenerateInput{
		ClusterName: f.name, KubernetesVersion: f.template.K8sVersion,
		ControlPlaneCount: f.controlPlaneCount, WorkerCount: f.workerCount, CNI: f.cni,
		ProxmoxURL: proxmox.NormalizeURL(conn.URL), ProxmoxTokenID: conn.TokenID, ProxmoxSecret: secret,
		SourceNode: f.template.Node, TemplateVMID: f.template.VMID,
		AllowedNodes: f.allowedNodes, VMSSHKeys: f.vmSSHKeys,
		ControlPlaneEndpointIP: f.controlPlaneEndpoint, NodeIPRange: f.nodeIPRange,
		Gateway: f.gateway, IPPrefix: f.ipPrefix, DNSServers: f.dnsServers, Bridge: f.bridge,
		BootVolumeSizeGB: f.bootVolumeSize, NumSockets: f.numSockets, NumCores: f.numCores, MemoryMiB: f.memoryMiB,
	}

	cctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	yaml, err := capi.Generate(cctx, s.dataDir, s.binDir, in)
	if err != nil {
		ui.RenderPartial(w, "cluster_preview", map[string]any{"Error": err.Error()})
		return
	}

	ui.RenderPartial(w, "cluster_preview", map[string]any{
		"ClusterName": f.name, "TemplateID": f.templateID, "YAML": yaml, "CSRF": s.csrfFor(session), "CNI": string(f.cni),
		"InstallMetricsServer": f.addons.MetricsServer, "InstallIstio": f.addons.Istio,
		"InstallMetalLB": f.addons.MetalLB, "MetalLBIPPool": f.addons.MetalLBIPPool,
	})
}

func (s *Server) handleClustersApply(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	conn, err := s.getConnection()
	if err != nil {
		ui.RenderPartial(w, "clusters_not_connected", nil)
		return
	}
	r.ParseForm()
	name := r.FormValue("name")
	yaml := r.FormValue("manifest_yaml")
	templateID, _ := strconv.ParseInt(r.FormValue("template_id"), 10, 64)
	if name == "" || yaml == "" {
		s.renderClustersPanel(w, r.Context(), session, conn, "missing cluster name or manifest — preview again")
		return
	}

	secret, err := s.sealer.Open(conn.SecretSeal)
	if err != nil {
		s.renderClustersPanel(w, r.Context(), session, conn, err.Error())
		return
	}
	s.redactor.Track(secret)

	if _, err := s.db.Exec(`INSERT INTO clusters (name, connection_id, template_id, manifest_yaml, status) VALUES (?, ?, ?, ?, 'provisioning')`,
		name, conn.ID, templateID, yaml); err != nil {
		s.renderClustersPanel(w, r.Context(), session, conn, "recording cluster: "+err.Error())
		return
	}

	cni := capi.CNIFlavor(r.FormValue("cni"))
	addons := capi.AddonSelection{
		MetricsServer: r.FormValue("install_metrics_server") == "1",
		Istio:         r.FormValue("install_istio") == "1",
		MetalLB:       r.FormValue("install_metallb") == "1",
		MetalLBIPPool: strings.TrimSpace(r.FormValue("metallb_ip_pool")),
	}
	spec := capi.ApplySpec(name, s.dataDir, s.binDir, proxmox.NormalizeURL(conn.URL), conn.TokenID, secret, yaml, cni, addons)
	jobID, err := s.jobs.Start(spec, `{"cluster":"`+name+`"}`)
	if err != nil {
		s.renderClustersPanel(w, r.Context(), session, conn, "starting job: "+err.Error())
		return
	}
	ui.RenderPartial(w, "job_progress", map[string]any{
		"JobID": jobID, "Title": spec.Title,
		"WrapperID": "clusters-panel", "ReloadURL": "/clusters/panel", "ReloadTarget": "#clusters-panel",
	})
}
