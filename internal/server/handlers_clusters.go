package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
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
	clusters := s.listClusters(conn.ID)

	ui.RenderPartial(w, "clusters_panel", map[string]any{
		"Error":     errMsg,
		"CSRF":      s.csrfFor(session),
		"Snapshot":  snap,
		"Templates": templates,
		"Clusters":  clusters,
	})
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

func (s *Server) listClusters(connID int64) []clusterListView {
	rows, err := s.db.Query(`SELECT name, status, created_at FROM clusters WHERE connection_id = ? ORDER BY id DESC`, connID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []clusterListView
	for rows.Next() {
		var c clusterListView
		rows.Scan(&c.Name, &c.Status, &c.CreatedAt)
		out = append(out, c)
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
}

func (s *Server) parseClusterForm(r *http.Request, connID int64) (clusterForm, error) {
	r.ParseForm()
	f := clusterForm{
		name:                 strings.TrimSpace(r.FormValue("name")),
		controlPlaneCount:    atoiDefault(r.FormValue("control_plane_count"), 1),
		workerCount:          atoiDefault(r.FormValue("worker_count"), 0),
		cni:                  capi.CNIFlavor(r.FormValue("cni")),
		bridge:               r.FormValue("bridge"),
		numSockets:           atoiDefault(r.FormValue("num_sockets"), 2),
		numCores:             atoiDefault(r.FormValue("num_cores"), 4),
		memoryMiB:            atoiDefault(r.FormValue("memory_mib"), 8048),
		bootVolumeSize:       atoiDefault(r.FormValue("boot_volume_size"), 100),
		gateway:              r.FormValue("gateway"),
		ipPrefix:             atoiDefault(r.FormValue("ip_prefix"), 24),
		controlPlaneEndpoint: r.FormValue("control_plane_endpoint_ip"),
		nodeIPRange:          r.FormValue("node_ip_range"),
		allowedNodes:         r.Form["allowed_nodes"],
	}
	if f.name == "" {
		return f, errBadInput("cluster name is required")
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
		"ClusterName": f.name, "TemplateID": f.templateID, "YAML": yaml, "CSRF": s.csrfFor(session),
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

	spec := capi.ApplySpec(name, s.dataDir, s.binDir, proxmox.NormalizeURL(conn.URL), conn.TokenID, secret, yaml)
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
