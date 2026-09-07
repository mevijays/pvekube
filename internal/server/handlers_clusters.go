package server

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
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
	s.rememberConnOverride(r)
	ui.Render(w, "clusters", map[string]any{"CSRF": s.csrfFor(session)})
}

// handleClustersDefaultsClear forgets the remembered creation inputs and
// re-renders the panel, so the form comes back blank.
func (s *Server) handleClustersDefaultsClear(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	r.ParseForm()
	conn, err := s.formConnection(r)
	if err != nil {
		ui.RenderPartial(w, "clusters_not_connected", nil)
		return
	}
	s.clearClusterDefaults()
	s.renderClustersPanel(w, r.Context(), session, conn, "")
}

// handleClustersPanel resolves which connection's clusters/capacity/templates
// to show exactly the way Templates does — an explicit ?conn= wins and is
// remembered for next time (activeConnection), so the host <select> at the
// top of the panel is a plain navigation, not a dynamic swap.
func (s *Server) handleClustersPanel(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	conn, err := s.activeConnection(r.URL.Query().Get("conn"))
	if err != nil {
		ui.RenderPartial(w, "clusters_not_connected", nil)
		return
	}
	s.renderClustersPanel(w, r.Context(), session, conn, "")
}

func (s *Server) renderClustersPanel(w http.ResponseWriter, ctx context.Context, session string, conn *storedConnection, errMsg string) {
	conns, connErr := s.listConnections()
	if connErr != nil {
		conns = nil
	}

	client, err := s.proxmoxClientFor(conn)
	if err != nil {
		ui.RenderPartial(w, "clusters_not_connected", nil)
		return
	}
	// Force a fresh discovery when rendering the cluster creation panel
	// so the user always sees accurate memory availability for placement.
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	snap, err := client.Discover(cctx)
	if err != nil {
		ui.RenderPartial(w, "clusters_panel", map[string]any{
			"Error": "Discovery failed: " + err.Error(), "CSRF": s.csrfFor(session), "Defaults": s.loadClusterDefaults(),
			"Connections": conns, "SelectedConn": conn,
		})
		return
	}
	s.cacheDiscovery(conn.ID, snap)

	templates := s.listTemplates(conn.ID)

	ui.RenderPartial(w, "clusters_panel", map[string]any{
		"Error":     errMsg,
		"CSRF":      s.csrfFor(session),
		"Snapshot":  snap,
		"Templates": templates,
		"Defaults":  s.loadClusterDefaults(),
		// Connections + SelectedConn back the host <select>; capacity table,
		// AllowedNodes checkboxes, and the template dropdown are all scoped
		// to SelectedConn only — same "no cross-host view" design as
		// Templates, for the same consistency reasons.
		"Connections":  conns,
		"SelectedConn": conn,
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
	// No ?conn= forwarding needed here: this loads via hx-get="load" from
	// inside clusters_panel.html, which has already resolved+remembered the
	// active connection (via handleClustersPanel/rememberConnOverride) by
	// the time this fires — same pattern as Templates.
	conn, err := s.activeConnection("")
	if err != nil {
		ui.RenderPartial(w, "clusters_list", map[string]any{"Clusters": nil})
		return
	}

	rows, dbErr := s.db.Query(`SELECT name, status, created_at FROM clusters WHERE connection_id = ? ORDER BY id DESC`, conn.ID)
	var names []clusterListView
	if dbErr == nil {
		defer rows.Close()
		for rows.Next() {
			var c clusterListView
			if err := rows.Scan(&c.Name, &c.Status, &c.CreatedAt); err != nil {
				rows.Close()
				ui.RenderPartial(w, "clusters_list", map[string]any{"Clusters": nil})
				return
			}
			names = append(names, c)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			ui.RenderPartial(w, "clusters_list", map[string]any{"Clusters": nil})
			return
		}
	} else {
		// Query-level failure: show empty list
		ui.RenderPartial(w, "clusters_list", map[string]any{"Clusters": nil})
		return
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
	rows, dbErr := s.db.Query(`SELECT id, os_flavor, k8s_version, node, vmid FROM templates WHERE connection_id = ? ORDER BY id DESC`, connID)
	if dbErr != nil {
		return nil
	}
	defer rows.Close()
	var out []templateOptionView
	for rows.Next() {
		var t templateOptionView
		if err := rows.Scan(&t.ID, &t.OSFlavor, &t.K8sVersion, &t.Node, &t.VMID); err != nil {
			// Stop on scan error and return what we have so caller doesn't get
			// silently corrupted rows.
			return nil
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil
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
	registry             capi.RegistryConfig
	oidc                 capi.OIDCConfig
	privateDNS           capi.PrivateDNSConfig
	// internalCA is the organisation's private CA in PEM. Separate from
	// registry.CACertPEM because it is useful without a registry at all —
	// any internal HTTPS endpoint the nodes or pods reach needs it.
	internalCA string
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

			GitOps:         r.FormValue("install_gitops") == "1",
			GitOpsRepoURL:  strings.TrimSpace(r.FormValue("gitops_repo_url")),
			GitOpsBranch:   strings.TrimSpace(r.FormValue("gitops_branch")),
			GitOpsPath:     strings.TrimSpace(r.FormValue("gitops_path")),
			GitOpsUsername: strings.TrimSpace(r.FormValue("gitops_username")),
			// Trimmed for the same reason the registry password is: a pasted
			// trailing space is invisible in the form and silently breaks the
			// basic-auth header Flux builds from it.
			GitOpsToken:  strings.TrimSpace(r.FormValue("gitops_token")),
			GitOpsCACert: strings.TrimSpace(r.FormValue("gitops_ca_cert")),
		},
		registry: capi.RegistryConfig{
			Host:      capi.NormalizeRegistryHost(r.FormValue("registry_host")),
			CACertPEM: strings.TrimSpace(r.FormValue("registry_ca_cert")),
			Username:  strings.TrimSpace(r.FormValue("registry_username")),
			// Trimmed for the same reason the Proxmox connection secret is
			// (handlers_proxmox.go handleProxmoxConnect) — a pasted trailing
			// space is invisible in the form but breaks the registry
			// authentication header built from this value later.
			Password: strings.TrimSpace(r.FormValue("registry_password")),
		},
		oidc: capi.OIDCConfig{
			Provider:          r.FormValue("oidc_provider"),
			IssuerURL:         strings.TrimSpace(r.FormValue("oidc_issuer_url")),
			ClientID:          strings.TrimSpace(r.FormValue("oidc_client_id")),
			UsernameClaim:     strings.TrimSpace(r.FormValue("oidc_username_claim")),
			GroupsClaim:       strings.TrimSpace(r.FormValue("oidc_groups_claim")),
			CACertPEM:         strings.TrimSpace(r.FormValue("oidc_ca_cert")),
			DefaultUsersGroup: strings.TrimSpace(r.FormValue("oidc_default_users_group")),
		},
		privateDNS: capi.PrivateDNSConfig{
			Domains: capi.ParseDNSList(r.FormValue("private_dns_domains")),
			Servers: capi.ParseDNSList(r.FormValue("private_dns_servers")),
		},
		internalCA: strings.TrimSpace(r.FormValue("internal_ca_cert")),
	}
	if f.name == "" {
		return f, errBadInput("cluster name is required")
	}
	if f.addons.MetalLB && f.addons.MetalLBIPPool == "" {
		return f, errBadInput("MetalLB is checked but no IP pool was given")
	}
	if err := validateRegistryForm(f.registry); err != nil {
		return f, err
	}
	if err := validateOIDCForm(f.oidc); err != nil {
		return f, err
	}
	if err := validateGitOpsForm(f.addons); err != nil {
		return f, err
	}
	// Checked here rather than in the apply step because a Corefile CoreDNS
	// refuses to load does not fail loudly — CoreDNS crash-loops afterwards
	// and every name in the cluster stops resolving.
	if err := f.privateDNS.Validate(); err != nil {
		return f, errBadInput(err.Error())
	}
	if err := validateInternalCAForm(f.internalCA); err != nil {
		return f, err
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

// validateRegistryForm catches the mistakes that would otherwise only show up
// as a node that boots fine but can't pull — by which point the operator is
// reading kubelet logs on a VM instead of a form error. The CA is actually
// parsed rather than just checked for non-emptiness, since a truncated or
// wrong-format paste is the most likely failure and is invisible by eye.
func validateRegistryForm(reg capi.RegistryConfig) error {
	if !reg.Enabled() {
		if reg.CACertPEM != "" || reg.Username != "" || reg.Password != "" {
			return errBadInput("registry CA/credentials were given but the registry host is empty")
		}
		return nil
	}
	if strings.ContainsAny(reg.Host, " \t") {
		return errBadInput("registry host must be a bare host[:port], with no spaces")
	}
	if ca := reg.CACertPEM; ca != "" {
		block, _ := pem.Decode([]byte(ca))
		if block == nil {
			return errBadInput("registry CA certificate is not valid PEM — it should start with -----BEGIN CERTIFICATE-----")
		}
		if block.Type != "CERTIFICATE" {
			return errBadInput("registry CA must be a CERTIFICATE PEM block, got " + block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return errBadInput("registry CA certificate could not be parsed: " + err.Error())
		}
	}
	if (reg.Username == "") != (reg.Password == "") {
		return errBadInput("registry username and password must be given together")
	}
	return nil
}

// validateOIDCForm mirrors validateRegistryForm's shape: same "either fully
// configured or fully empty" contract, same CA-is-actually-PEM check.
func validateOIDCForm(oidc capi.OIDCConfig) error {
	if !oidc.Enabled() {
		if oidc.IssuerURL != "" || oidc.ClientID != "" || oidc.CACertPEM != "" {
			return errBadInput("OIDC issuer URL and client ID must be given together")
		}
		if oidc.DefaultUsersGroup != "" {
			return errBadInput("a default users group was given but OIDC issuer URL/client ID are empty — it would grant access to a group that can never authenticate")
		}
		return nil
	}
	if !strings.HasPrefix(oidc.IssuerURL, "https://") {
		return errBadInput("OIDC issuer URL must start with https:// — kube-apiserver's OIDC plugin requires it")
	}
	if ca := oidc.CACertPEM; ca != "" {
		block, _ := pem.Decode([]byte(ca))
		if block == nil {
			return errBadInput("OIDC CA certificate is not valid PEM — it should start with -----BEGIN CERTIFICATE-----")
		}
		if block.Type != "CERTIFICATE" {
			return errBadInput("OIDC CA must be a CERTIFICATE PEM block, got " + block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return errBadInput("OIDC CA certificate could not be parsed: " + err.Error())
		}
	}
	return nil
}

// validateGitOpsForm mirrors validateRegistryForm/validateOIDCForm: same
// "either configured or empty" contract, same real-PEM check on the CA.
func validateGitOpsForm(a capi.AddonSelection) error {
	if !a.GitOps {
		if a.GitOpsRepoURL != "" || a.GitOpsToken != "" || a.GitOpsCACert != "" {
			return errBadInput("GitOps settings were given but the GitOps checkbox is not ticked")
		}
		return nil
	}
	if a.GitOpsRepoURL == "" {
		return errBadInput("GitOps is checked but no repository URL was given")
	}
	// Flux requires a full scheme and rejects the scp-style shorthand
	// outright (per its GitRepository API docs), so catch it here rather
	// than letting source-controller fail after the cluster is already up.
	switch {
	case strings.HasPrefix(a.GitOpsRepoURL, "https://"), strings.HasPrefix(a.GitOpsRepoURL, "http://"):
	case strings.HasPrefix(a.GitOpsRepoURL, "ssh://"):
	default:
		if strings.Contains(a.GitOpsRepoURL, "@") && strings.Contains(a.GitOpsRepoURL, ":") {
			return errBadInput("GitOps repository URL must be a full URL — Flux does not accept the scp-style form; use ssh://git@host/path/repo.git instead")
		}
		return errBadInput("GitOps repository URL must start with https://, http:// or ssh://")
	}
	// SSH auth needs an identity + known_hosts pair, which this form doesn't
	// collect — refusing is honest, whereas accepting would produce a
	// cluster whose Flux can never authenticate.
	if strings.HasPrefix(a.GitOpsRepoURL, "ssh://") && a.GitOpsToken != "" {
		return errBadInput("a token cannot authenticate an ssh:// repository — use an https:// URL for token auth, or make the repository readable without credentials")
	}
	if ca := a.GitOpsCACert; ca != "" {
		block, _ := pem.Decode([]byte(ca))
		if block == nil {
			return errBadInput("GitOps CA certificate is not valid PEM — it should start with -----BEGIN CERTIFICATE-----")
		}
		if block.Type != "CERTIFICATE" {
			return errBadInput("GitOps CA must be a CERTIFICATE PEM block, got " + block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return errBadInput("GitOps CA certificate could not be parsed: " + err.Error())
		}
	}
	return nil
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

func blockingIPPlanError(issues []ipplan.Issue) error {
	var messages []string
	for _, issue := range issues {
		if issue.Severity == ipplan.SeverityError {
			messages = append(messages, issue.Field+": "+issue.Message)
		}
	}
	if len(messages) == 0 {
		return nil
	}
	return errBadInput("IP plan is invalid: " + strings.Join(messages, "; "))
}

func (s *Server) handleClustersCheckIP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	conn, err := s.formConnection(r)
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
	r.ParseForm()
	conn, err := s.formConnection(r)
	if err != nil {
		http.Error(w, "not connected", http.StatusBadRequest)
		return
	}
	f, err := s.parseClusterForm(r, conn.ID)
	if err != nil {
		ui.RenderPartial(w, "cluster_preview", map[string]any{"Error": err.Error()})
		return
	}
	if err := blockingIPPlanError(ipplan.Validate(f.ipPlan())); err != nil {
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
		SourceNode: f.template.Node, TemplateVMID: f.template.VMID, OSFlavor: f.template.OSFlavor,
		AllowedNodes: f.allowedNodes, VMSSHKeys: f.vmSSHKeys,
		ControlPlaneEndpointIP: f.controlPlaneEndpoint, NodeIPRange: f.nodeIPRange,
		Gateway: f.gateway, IPPrefix: f.ipPrefix, DNSServers: f.dnsServers, Bridge: f.bridge,
		BootVolumeSizeGB: f.bootVolumeSize, NumSockets: f.numSockets, NumCores: f.numCores, MemoryMiB: f.memoryMiB,
		Registry: f.registry, OIDC: f.oidc, ConnectionID: conn.ID,
		InternalCA: f.internalCA, PrivateDNS: f.privateDNS,
	}
	// Keep the registry password out of the rendered manifest, job logs, and
	// anything else the redactor covers. The CA and host are fine to show.
	if f.registry.Password != "" {
		s.redactor.Track(f.registry.Password)
	}
	if f.addons.GitOpsToken != "" {
		s.redactor.Track(f.addons.GitOpsToken)
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
		"ControlPlaneCount": f.controlPlaneCount, "WorkerCount": f.workerCount,
		"Bridge": f.bridge, "NumSockets": f.numSockets, "NumCores": f.numCores,
		"MemoryMiB": f.memoryMiB, "BootVolumeSize": f.bootVolumeSize,
		"Gateway": f.gateway, "IPPrefix": f.ipPrefix, "ControlPlaneEndpoint": f.controlPlaneEndpoint,
		"NodeIPRange": f.nodeIPRange, "DNSServers": strings.Join(f.dnsServers, ", "), "AllowedNodes": f.allowedNodes,
		"InstallMetricsServer": f.addons.MetricsServer, "InstallIstio": f.addons.Istio,
		"InstallMetalLB": f.addons.MetalLB, "MetalLBIPPool": f.addons.MetalLBIPPool,
		"InstallGitOps": f.addons.GitOps,
		// Carried through the preview into the apply POST as hidden fields —
		// parseClusterForm runs again on apply, so anything missing from
		// cluster_preview.html is silently lost between the two.
		"PrivateDNSDomains": strings.Join(f.privateDNS.Domains, ", "),
		"PrivateDNSServers": strings.Join(f.privateDNS.Servers, ", "),
		"InternalCACert":    f.internalCA,
		"GitOpsRepoURL":     f.addons.GitOpsRepoURL, "GitOpsBranch": f.addons.GitOpsBranch,
		"GitOpsPath": f.addons.GitOpsPath, "GitOpsUsername": f.addons.GitOpsUsername,
		"GitOpsToken": f.addons.GitOpsToken, "GitOpsCACert": f.addons.GitOpsCACert,
		"RegistryHost": f.registry.Host, "RegistryCACert": f.registry.CACertPEM,
		"RegistryUsername": f.registry.Username, "RegistryPassword": f.registry.Password,
		"RegistryInsecure": f.registry.Enabled() && f.registry.CACertPEM == "",
		"VMSSHKeys":        strings.Join(f.vmSSHKeys, ", "),
		"OIDCEnabled":      f.oidc.Enabled(),
		"OIDCProvider":     f.oidc.Provider, "OIDCIssuerURL": f.oidc.IssuerURL, "OIDCClientID": f.oidc.ClientID,
		"OIDCUsernameClaim": f.oidc.UsernameClaim, "OIDCGroupsClaim": f.oidc.GroupsClaim, "OIDCCACert": f.oidc.CACertPEM,
		"OIDCDefaultUsersGroup": f.oidc.DefaultUsersGroup,
		// Threaded through as a hidden field so handleClustersApply resolves
		// the SAME connection the manifest was actually generated against —
		// re-deriving "the active connection" independently at apply time
		// would be the exact class of race formConnection's doc comment
		// warns about (a second tab switching hosts between preview and
		// apply must not silently redirect this cluster to a different one).
		"ConnID": conn.ID,
	})
}

func (s *Server) handleClustersApply(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	if !s.checkCSRF(r, session) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return
	}
	r.ParseForm()
	// Resolve from the "ConnID" hidden field the preview screen threaded
	// through (see handleClustersPreview) — NOT the currently-active
	// connection, which could have changed in another tab since the
	// manifest was generated. formConnection reads "conn", so the preview
	// template names its hidden field "conn" too; see cluster_preview.html.
	conn, err := s.formConnection(r)
	if err != nil {
		ui.RenderPartial(w, "clusters_not_connected", nil)
		return
	}
	f, err := s.parseClusterForm(r, conn.ID)
	if err != nil {
		s.renderClustersPanel(w, r.Context(), session, conn, err.Error())
		return
	}
	if err := blockingIPPlanError(ipplan.Validate(f.ipPlan())); err != nil {
		s.renderClustersPanel(w, r.Context(), session, conn, err.Error())
		return
	}

	name := f.name
	yaml := r.FormValue("manifest_yaml")
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
		name, conn.ID, f.templateID, yaml); err != nil {
		s.renderClustersPanel(w, r.Context(), session, conn, "recording cluster: "+err.Error())
		return
	}

	addons := f.addons
	// Same treatment as the registry password: keep the Git token out of the
	// job log, the rendered manifest, and anything else the redactor covers.
	if addons.GitOpsToken != "" {
		s.redactor.Track(addons.GitOpsToken)
	}
	// The manifest already carries the CA/containerd config from preview
	// time; only the credentials are still needed here, to build the
	// in-cluster pull Secret.
	registry := f.registry
	if registry.Password != "" {
		s.redactor.Track(registry.Password)
	}

	// The manifest already carries the OIDC auth flags baked in from preview
	// time (see InjectOIDCAuth) — only DefaultUsersGroup still needs to be
	// acted on here, since RBAC isn't part of the manifest at all (it's a
	// post-provision step, needs a reachable API server — see
	// InstallOIDCDefaultGroupRBACStep).
	oidcDefaults := f.oidc

	// Remember the environment-wide inputs so the next cluster's form comes
	// up pre-filled. Done here rather than at preview so that abandoning a
	// half-filled form never changes the seed for the next one.
	s.saveClusterDefaults(clusterDefaults{
		VMSSHKeys:             strings.Join(f.vmSSHKeys, ", "),
		RegistryHost:          registry.Host,
		RegistryCACert:        registry.CACertPEM,
		RegistryUsername:      registry.Username,
		RegistryPassword:      registry.Password,
		OIDCProvider:          oidcDefaults.Provider,
		OIDCIssuerURL:         oidcDefaults.IssuerURL,
		OIDCClientID:          oidcDefaults.ClientID,
		OIDCUsernameClaim:     oidcDefaults.UsernameClaim,
		OIDCGroupsClaim:       oidcDefaults.GroupsClaim,
		OIDCCACert:            oidcDefaults.CACertPEM,
		OIDCDefaultUsersGroup: oidcDefaults.DefaultUsersGroup,

		GitOpsRepoURL:  addons.GitOpsRepoURL,
		GitOpsBranch:   addons.GitOpsBranch,
		GitOpsPath:     addons.GitOpsPath,
		GitOpsUsername: addons.GitOpsUsername,
		GitOpsToken:    addons.GitOpsToken,
		GitOpsCACert:   addons.GitOpsCACert,

		InternalCACert:    f.internalCA,
		PrivateDNSDomains: strings.Join(f.privateDNS.Domains, ", "),
		PrivateDNSServers: strings.Join(f.privateDNS.Servers, ", "),
	})
	spec := capi.ApplySpec(name, s.dataDir, s.binDir, capi.ClusterConnection{
		ID: conn.ID, URL: proxmox.NormalizeURL(conn.URL), TokenID: conn.TokenID,
		Secret: secret, InsecureTLS: conn.InsecureTLS, IsPrimary: conn.IsPrimary,
	}, yaml, f.cni, addons, registry, oidcDefaults, f.privateDNS, f.internalCA)
	jobID, err := s.jobs.StartExclusive(spec, `{"cluster":"`+name+`"}`, clusterOperationLock(name))
	if err != nil {
		// The row above is inserted first on purpose — clusters.name is
		// UNIQUE, so it is what rejects a duplicate name before any real work
		// happens. But leaving it behind when the job never starts strands it
		// at status 'provisioning' forever, and because of that same UNIQUE
		// constraint the operator can then never retry under the same name:
		// the next attempt fails with a bare "UNIQUE constraint failed".
		// Attempt a robust cleanup with a few retries to reduce the chance of
		// the row being left behind due to transient DB contention.
		const maxDelAttempts = 3
		for i := 0; i < maxDelAttempts; i++ {
			if _, delErr := s.db.Exec(`DELETE FROM clusters WHERE name = ?`, name); delErr == nil {
				break
			} else if i == maxDelAttempts-1 {
				slog.Warn("could not remove the cluster row after the job failed to start", "cluster", name, "err", delErr)
			} else {
				// brief backoff
				time.Sleep(100 * time.Millisecond)
			}
		}
		s.renderClustersPanel(w, r.Context(), session, conn, clusterOperationError(err))
		return
	}
	ui.RenderPartial(w, "job_progress", map[string]any{
		"JobID": jobID, "Title": spec.Title,
		"WrapperID": "clusters-panel", "ReloadURL": "/clusters/panel", "ReloadTarget": "#clusters-panel",
	})
}

// validateInternalCAForm applies the same "is this actually a certificate"
// check the registry and OIDC CA fields get. Worth repeating here because
// this CA reaches further than either of those: a malformed value would be
// written into every node's OS trust store and published to every namespace
// as a ConfigMap, so it fails at form time rather than halfway through an
// apply.
func validateInternalCAForm(ca string) error {
	if ca == "" {
		return nil
	}
	block, _ := pem.Decode([]byte(ca))
	if block == nil {
		return errBadInput("internal CA certificate is not valid PEM — it should start with -----BEGIN CERTIFICATE-----")
	}
	if block.Type != "CERTIFICATE" {
		return errBadInput("internal CA must be a CERTIFICATE PEM block, got " + block.Type)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return errBadInput("internal CA certificate could not be parsed: " + err.Error())
	}
	return nil
}
