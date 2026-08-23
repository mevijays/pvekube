# Quick Fix Guide - Critical Bugs

This document provides code patches for the 3 most critical bugs that should be fixed immediately.

## Bug #1: Missing rows.Err() in handleClustersList() and listTemplates()

### Location
- [internal/server/handlers_clusters.go](internal/server/handlers_clusters.go#L130-L145) - `handleClustersList()`
- [internal/server/handlers_clusters.go](internal/server/handlers_clusters.go#L170-L182) - `listTemplates()`

### Current Code (BROKEN)
```go
// handleClustersList - line 133-143
rows, err := s.db.Query(`SELECT name, status, created_at FROM clusters WHERE connection_id = ? ORDER BY id DESC`, conn.ID)
var names []clusterListView
if err == nil {
    defer rows.Close()
    for rows.Next() {
        var c clusterListView
        rows.Scan(&c.Name, &c.Status, &c.CreatedAt)
        names = append(names, c)
    }
    // BUG: Missing rows.Err() check!
}
```

### Fixed Code
```go
rows, err := s.db.Query(`SELECT name, status, created_at FROM clusters WHERE connection_id = ? ORDER BY id DESC`, conn.ID)
var names []clusterListView
if err != nil {
    slog.Error("query clusters failed", "error", err)
    // Return empty list with error indication
    ui.RenderPartial(w, "clusters_list", map[string]any{"Clusters": nil, "Error": "Failed to load clusters"})
    return
}
defer rows.Close()
for rows.Next() {
    var c clusterListView
    if err := rows.Scan(&c.Name, &c.Status, &c.CreatedAt); err != nil {
        slog.Warn("scanning cluster row", "error", err)
        continue
    }
    names = append(names, c)
}
if err := rows.Err(); err != nil {
    slog.Error("cluster query rows error", "error", err)
}
```

### For listTemplates() - line 170-182
```go
// BEFORE
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

// AFTER
rows, err := s.db.Query(`SELECT id, os_flavor, k8s_version, node, vmid FROM templates WHERE connection_id = ? ORDER BY id DESC`, connID)
if err != nil {
    slog.Error("query templates failed", "error", err)
    return nil
}
defer rows.Close()
var out []templateOptionView
for rows.Next() {
    var t templateOptionView
    if err := rows.Scan(&t.ID, &t.OSFlavor, &t.K8sVersion, &t.Node, &t.VMID); err != nil {
        slog.Warn("scanning template row", "error", err)
        continue
    }
    out = append(out, t)
}
if err := rows.Err(); err != nil {
    slog.Error("template query rows error", "error", err)
}
return out
```

---

## Bug #3: Cluster Row Insertion/Deletion Race

### Location
[internal/server/handlers_clusters.go](internal/server/handlers_clusters.go#L516-L600) - `handleClustersApply()`

### Current Code (BROKEN)
```go
// Line 540-545: INSERT before job
if _, err := s.db.Exec(`INSERT INTO clusters (name, connection_id, template_id, manifest_yaml, status) VALUES (?, ?, ?, ?, 'provisioning')`,
    name, conn.ID, templateID, yaml); err != nil {
    s.renderClustersPanel(w, r.Context(), session, conn, "recording cluster: "+err.Error())
    return
}

// ... lots of code ...

// Line 570-580: Job start can fail
jobID, err := s.jobs.Start(spec, `{"cluster":"`+name+`"}`)
if err != nil {
    // Attempt cleanup but it can fail silently
    if _, delErr := s.db.Exec(`DELETE FROM clusters WHERE name = ?`, name); delErr != nil {
        slog.Warn("could not remove the cluster row after the job failed to start", "cluster", name, "err", delErr)
    }
    s.renderClustersPanel(w, r.Context(), session, conn, "starting job: "+err.Error())
    return
}
```

### Fixed Code

**Option A: Start job first (RECOMMENDED)**

```go
func (s *Server) handleClustersApply(w http.ResponseWriter, r *http.Request) {
    // ... validation ...
    
    // Parse form early
    cni := capi.CNIFlavor(r.FormValue("cni"))
    addons := capi.AddonSelection{ /* ... */ }
    registry := capi.RegistryConfig{ /* ... */ }
    oidcDefaults := capi.OIDCConfig{ /* ... */ }
    
    // Create job spec WITHOUT starting yet
    spec := capi.ApplySpec(name, s.dataDir, s.binDir, capi.ClusterConnection{
        ID: conn.ID, URL: proxmox.NormalizeURL(conn.URL), TokenID: conn.TokenID,
        Secret: secret, InsecureTLS: conn.InsecureTLS, IsPrimary: conn.IsPrimary,
    }, yaml, cni, addons, registry, oidcDefaults)
    
    // START JOB FIRST
    jobID, err := s.jobs.Start(spec, `{"cluster":"`+name+`"}`)
    if err != nil {
        // No cleanup needed - nothing was inserted yet
        s.renderClustersPanel(w, r.Context(), session, conn, "starting job: "+err.Error())
        return
    }
    
    // NOW insert the cluster row (after job successfully started)
    if _, err := s.db.Exec(`INSERT INTO clusters (name, connection_id, template_id, manifest_yaml, status, job_id) VALUES (?, ?, ?, ?, 'provisioning', ?)`,
        name, conn.ID, templateID, yaml, jobID); err != nil {
        // Cancel the already-running job
        s.jobs.Cancel(jobID)
        s.renderClustersPanel(w, r.Context(), session, conn, "recording cluster: "+err.Error())
        return
    }
    
    // Save defaults after successful DB insert
    s.saveClusterDefaults(clusterDefaults{ /* ... */ })
    
    ui.RenderPartial(w, "job_progress", map[string]any{
        "JobID": jobID, "Title": spec.Title,
        "WrapperID": "clusters-panel", "ReloadURL": "/clusters/panel", "ReloadTarget": "#clusters-panel",
    })
}
```

**Option B: Use transaction (if you prefer the old order)**

```go
// BEGIN transaction
tx, err := s.db.Begin()
if err != nil {
    s.renderClustersPanel(w, r.Context(), session, conn, "database error: "+err.Error())
    return
}
defer tx.Rollback()  // Rollback if we don't explicitly Commit

// Insert cluster
if _, err := tx.Exec(`INSERT INTO clusters (name, connection_id, template_id, manifest_yaml, status) VALUES (?, ?, ?, ?, 'provisioning')`,
    name, conn.ID, templateID, yaml); err != nil {
    s.renderClustersPanel(w, r.Context(), session, conn, "recording cluster: "+err.Error())
    return
}

// Start job
jobID, err := s.jobs.Start(spec, `{"cluster":"`+name+`"}`)
if err != nil {
    // Transaction automatically rolled back (Rollback at defer above)
    s.renderClustersPanel(w, r.Context(), session, conn, "starting job: "+err.Error())
    return
}

// Commit transaction
if err := tx.Commit(); err != nil {
    s.jobs.Cancel(jobID)
    s.renderClustersPanel(w, r.Context(), session, conn, "database error: "+err.Error())
    return
}

s.saveClusterDefaults(clusterDefaults{ /* ... */ })
// Render success
```

---

## Bug #5: Secret Tracking Timing

### Location
[internal/server/handlers_clusters.go](internal/server/handlers_clusters.go#L516-L580) - `handleClustersApply()`

### Current Code (BROKEN)
```go
// Line ~544: Parse form WITHOUT tracking secrets
secret, err := s.sealer.Open(conn.SecretSeal)
if err != nil {
    s.renderClustersPanel(w, r.Context(), session, conn, err.Error())
    return
}
s.redactor.Track(secret)  // Track proxmox secret

// ... build cluster form ...

// Line ~560-567: These secrets are already in the manifest by now!
if addons.GitOpsToken != "" {
    s.redactor.Track(addons.GitOpsToken)  // TOO LATE - manifest already built!
}
if registry.Password != "" {
    s.redactor.Track(registry.Password)  // TOO LATE
}
```

### Fixed Code

```go
func (s *Server) handleClustersApply(w http.ResponseWriter, r *http.Request) {
    session := r.Context().Value(ctxSessionKey{}).(string)
    if !s.checkCSRF(r, session) {
        http.Error(w, "bad csrf", http.StatusForbidden)
        return
    }
    r.ParseForm()
    
    // Get connection
    conn, err := s.formConnection(r)
    if err != nil {
        ui.RenderPartial(w, "clusters_not_connected", nil)
        return
    }
    
    // IMMEDIATELY unlock and track the proxmox secret
    secret, err := s.sealer.Open(conn.SecretSeal)
    if err != nil {
        s.renderClustersPanel(w, r.Context(), session, conn, err.Error())
        return
    }
    s.redactor.Track(secret)  // Track NOW, before using it
    
    // Parse form inputs and track secrets BEFORE using them
    name := r.FormValue("name")
    yaml := r.FormValue("manifest_yaml")
    templateID, _ := strconv.ParseInt(r.FormValue("template_id"), 10, 64)
    
    // --- Parse and track GitOps token EARLY ---
    gitopsToken := strings.TrimSpace(r.FormValue("gitops_token"))
    if gitopsToken != "" {
        s.redactor.Track(gitopsToken)  // Track BEFORE including in manifest
    }
    
    // --- Parse and track registry password EARLY ---
    registryPassword := r.FormValue("registry_password")
    if registryPassword != "" {
        s.redactor.Track(registryPassword)  // Track BEFORE including in manifest
    }
    
    // --- Parse and track OIDC secrets EARLY ---
    oidcCACert := strings.TrimSpace(r.FormValue("oidc_ca_cert"))
    if oidcCACert != "" {
        s.redactor.Track(oidcCACert)
    }
    
    // NOW build the cluster form and manifest with tracked secrets
    cni := capi.CNIFlavor(r.FormValue("cni"))
    addons := capi.AddonSelection{
        MetricsServer: r.FormValue("install_metrics_server") == "1",
        Istio:         r.FormValue("install_istio") == "1",
        MetalLB:       r.FormValue("install_metallb") == "1",
        MetalLBIPPool: strings.TrimSpace(r.FormValue("metallb_ip_pool")),
        GitOps:         r.FormValue("install_gitops") == "1",
        GitOpsRepoURL:  strings.TrimSpace(r.FormValue("gitops_repo_url")),
        GitOpsBranch:   strings.TrimSpace(r.FormValue("gitops_branch")),
        GitOpsPath:     strings.TrimSpace(r.FormValue("gitops_path")),
        GitOpsUsername: strings.TrimSpace(r.FormValue("gitops_username")),
        GitOpsToken:    gitopsToken,  // Already tracked
        GitOpsCACert:   strings.TrimSpace(r.FormValue("gitops_ca_cert")),
    }
    
    registry := capi.RegistryConfig{
        Host:      capi.NormalizeRegistryHost(r.FormValue("registry_host")),
        CACertPEM: strings.TrimSpace(r.FormValue("registry_ca_cert")),
        Username:  strings.TrimSpace(r.FormValue("registry_username")),
        Password:  registryPassword,  // Already tracked
    }
    
    oidcDefaults := capi.OIDCConfig{
        Provider:          r.FormValue("oidc_provider"),
        IssuerURL:         strings.TrimSpace(r.FormValue("oidc_issuer_url")),
        ClientID:          strings.TrimSpace(r.FormValue("oidc_client_id")),
        UsernameClaim:     strings.TrimSpace(r.FormValue("oidc_username_claim")),
        GroupsClaim:       strings.TrimSpace(r.FormValue("oidc_groups_claim")),
        CACertPEM:         oidcCACert,  // Already tracked
        DefaultUsersGroup: strings.TrimSpace(r.FormValue("oidc_default_users_group")),
    }
    
    // ... rest of function ...
}
```

---

## Testing Recommendations

### For Bug #1 & #2 (Database Errors)
```go
// Test that database errors don't crash the handler
func TestHandleClustersList_DBError(t *testing.T) {
    // Mock database to return error after first row
    // Verify handler returns gracefully and logs error
}
```

### For Bug #3 (Race Condition)
```go
// Test that cluster row cleanup works
func TestHandleClustersApply_JobStartFailure(t *testing.T) {
    // Mock jobs.Start() to fail
    // Verify cluster row is NOT in database after failure
    // Verify retry with same name works (no UNIQUE constraint error)
}
```

### For Bug #5 (Secret Redaction)
```go
// Test that secrets are redacted in job logs
func TestSecretRedactionTiming(t *testing.T) {
    // Parse form with secrets
    // Verify secrets tracked before manifest generation
    // Verify no secrets appear in database manifest column
}
```
