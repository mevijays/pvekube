# PVEKube Bug Report - Comprehensive Code Review

## Summary
This review identified **7 confirmed bugs** and **3 code quality issues** across the workspace. Severity ranges from critical (data consistency) to medium (error handling and race conditions).

---

## Critical Bugs

### 1. **Missing `rows.Err()` Checks in Database Queries**
**File:** [internal/server/handlers_clusters.go](internal/server/handlers_clusters.go#L130-L145), [internal/server/handlers_clusters.go](internal/server/handlers_clusters.go#L170-L182)

**Location:** 
- Line 130-145: `handleClustersList()` 
- Line 170-182: `listTemplates()`

**Issue:** 
Both functions execute database queries and iterate through results with `rows.Next()`, but **never check `rows.Err()`** after the loop completes. If a database error occurs during row scanning (e.g., connection dropped, disk I/O error), it's silently ignored.

**Impact:** 
- Cluster list UI shows incomplete/stale data without indicating the query failed
- Template dropdown on cluster designer may be missing valid templates
- Operator has no indication something went wrong

**Example:**
```go
// WRONG - missing rows.Err()
rows, err := s.db.Query(`SELECT name, status, created_at FROM clusters WHERE connection_id = ? ORDER BY id DESC`, conn.ID)
if err == nil {
    defer rows.Close()
    for rows.Next() {
        var c clusterListView
        rows.Scan(&c.Name, &c.Status, &c.CreatedAt)  // Error here is ignored!
        names = append(names, c)
    }
    // Missing: if err := rows.Err(); err != nil { ... }
}
```

**Fix:**
```go
rows, err := s.db.Query(...)
if err == nil {
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
        slog.Error("cluster query failed", "error", err)
    }
}
```

---

### 2. **Missing Error Check on `rows.Scan()` Calls**
**File:** [internal/server/handlers_clusters.go](internal/server/handlers_clusters.go#L136-L139), [internal/server/handlers_clusters.go](internal/server/handlers_clusters.go#L175-L178)

**Issue:** 
Multiple places call `rows.Scan()` without checking the returned error. If a column cannot be scanned into the target type, the partial data continues to be used.

**Impact:**
- Uninitialized or partially-scanned struct fields used in rendering
- May cause nil pointer dereferences downstream
- Data corruption (zero-values mixed with partially-read data)

**Example:**
```go
// WRONG
for rows.Next() {
    var c clusterListView
    rows.Scan(&c.Name, &c.Status, &c.CreatedAt)  // Error ignored!
    names = append(names, c)
}
```

---

### 3. **Race Condition: Cluster Row Insertion/Deletion on Job Start Failure**
**File:** [internal/server/handlers_clusters.go](internal/server/handlers_clusters.go#L540-L580)

**Location:** `handleClustersApply()`, lines 540-580

**Issue:** 
The function inserts a cluster row into the database BEFORE starting the job. If `jobs.Start()` fails, it attempts to delete the row, but there's a narrow race window:

1. Cluster row inserted (status='provisioning')
2. User sees cluster appear in list via concurrent request
3. `jobs.Start()` fails
4. Row deletion attempted (but DELETE could also fail)
5. If DELETE fails, subsequent attempts to create the same cluster fail with UNIQUE constraint error
6. Operator is stuck with orphaned row and cannot retry

**Impact:**
- If job start fails (e.g., out of memory), operator gets misleading "UNIQUE constraint failed" on retry instead of the real error
- Orphaned cluster records if DELETE also fails
- UI shows phantom cluster that never provisions

**Code:**
```go
// Line 540: Insert happens FIRST
if _, err := s.db.Exec(`INSERT INTO clusters (...) VALUES (...)`, 
    name, conn.ID, templateID, yaml); err != nil {
    ...
    return
}
...
// Line 570: Job start can fail here
jobID, err := s.jobs.Start(spec, ...)
if err != nil {
    // LINE 573-576: Cleanup attempt
    if _, delErr := s.db.Exec(`DELETE FROM clusters WHERE name = ?`, name); delErr != nil {
        slog.Warn("could not remove the cluster row...", "err", delErr)  // Silent failure!
    }
    ...
    return
}
```

**Fix:** Use a transaction or insert AFTER successful job start:
```go
// Start job FIRST
jobID, err := s.jobs.Start(spec, ...)
if err != nil {
    s.renderClustersPanel(...)
    return  // No cleanup needed
}

// Insert row ONLY after job started successfully
if _, err := s.db.Exec(`INSERT INTO clusters (...)`, name, conn.ID, ...); err != nil {
    s.jobs.Cancel(jobID)
    s.renderClustersPanel(...)
    return
}
```

---

## High Severity Bugs

### 4. **Template Build Race Condition (Acknowledged but Unfixed)**
**File:** [internal/server/handlers_templates.go](internal/server/handlers_templates.go#L93-L105)

**Issue:**
The `templateBuildInProgress()` function checks the database to prevent concurrent builds on the same host, BUT the actual shared resource (Docker volume `imagebuilder.RepoDir`) is NOT protected. The code comment explicitly states this is a pre-existing bug:

> "two concurrent builds race on those files regardless of which Proxmox host either one targets... nothing ever serialized template.validate/build before multi-host support"

If an operator manually triggers builds on different hosts via different browser tabs, both will try to:
- Write to the same `packer.json` file
- Override each other's ISO-override JSON
- Share one Packer execution context

**Impact:**
- Template builds fail mysteriously with Packer errors
- Partial/corrupted template VMs created
- Requires manual intervention to clean up

**Code:**
```go
// templateBuildInProgress checks DB but doesn't lock the resource
func (s *Server) templateBuildInProgress() (bool, error) {
    var n int
    err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs 
        WHERE kind = 'template.build' AND status IN ('pending', 'running')`).Scan(&n)
    return n > 0, err
}
// But the actual resource: imagebuilder.RepoDir is shared and unprotected!
```

**Fix:** Need a file-based lock or per-host build mutex:
```go
type Server struct {
    buildMu sync.Mutex  // Already exists!
    // But only in server.go, not used in handlers_templates.go
}
```

---

### 5. **Silent Secret Tracking After Manifest Encoding**
**File:** [internal/server/handlers_clusters.go](internal/server/handlers_clusters.go#L544-L565)

**Issue:**
Secrets are tracked for redaction AFTER they're already included in:
1. Form value extraction (line 544): `oidcDefaults.CACertPEM`
2. Manifest YAML (already passed to rendering)
3. Saved cluster defaults (line 610+)

The `s.redactor.Track()` call comes too late to affect the manifest YAML that's been serialized and stored in the database.

```go
// Line ~560: Secret tracked too late
if addons.GitOpsToken != "" {
    s.redactor.Track(addons.GitOpsToken)  // Only affects FUTURE logs
}
// But the manifest_yaml below was already built:
// Line ~540: INSERT INTO clusters ... manifest_yaml
```

**Impact:**
- GitOps tokens, OIDC client secrets visible in `clusters.manifest_yaml` column
- Job logs correctly redacted but database has plaintext
- If database is compromised, secrets are exposed
- Backups contain plaintext credentials

**Fix:** Track secrets IMMEDIATELY when they enter the system:
```go
// Immediately when parsing
secret, err := s.sealer.Open(conn.SecretSeal)
if err != nil { ... }
s.redactor.Track(secret)  // NOW - not later

// Parse form EARLY
addons := parseAddons(r)
if addons.GitOpsToken != "" {
    s.redactor.Track(addons.GitOpsToken)  // BEFORE rendering manifest
}
```

---

## Medium Severity Bugs

### 6. **Potential Integer Overflow in IP Range Calculation**
**File:** [internal/ipplan/ipplan.go](internal/ipplan/ipplan.go#L145-L147)

**Issue:**
The `rangeSize()` function converts a uint32 to int without checking bounds. On a 32-bit system or with large ranges, this could theoretically overflow:

```go
func rangeSize(start, end net.IP) int {
    return int(ipToUint32(end)-ipToUint32(start)) + 1
    // If (end - start) > 2^31-1, int overflows to negative
}
```

**Impact:**
- Range size validation reports negative capacity
- Cluster designer allows creating cluster larger than the range permits
- Cluster provisioning fails with cryptic IP exhaustion error

**Likelihood:** Very low in practice (would need a huge IP range like 0.0.0.1-255.255.255.255), but still a bug.

**Fix:**
```go
func rangeSize(start, end net.IP) int64 {
    return int64(ipToUint32(end) - ipToUint32(start)) + 1
}
```

---

### 7. **Cluster Deletion with Orphaned Database Records**
**File:** [internal/server/handlers_cluster_detail.go](internal/server/handlers_cluster_detail.go#L105-L120)

**Issue:**
The cluster deletion flow has a subtle ordering issue:

```go
func (s *Server) handleClusterDelete(...) {
    // Line 113: Update status first
    s.db.Exec(`UPDATE clusters SET status = 'deleting' WHERE name = ?`, name)
    
    // Line 116-119: Add a cleanup step to the job
    spec.Step("Remove local record", func(c *jobs.Ctx) error {
        _, err := s.db.Exec(`DELETE FROM clusters WHERE name = ?`, name)
        return err
    })
}
```

If the DELETE step in the job fails (e.g., concurrent delete from another request), the error is returned and might cause the job to fail. But the cluster row is already marked 'deleting', so the UI won't show it even though it wasn't actually deleted.

**Impact:**
- Orphaned cluster records if the job fails
- Operator can't easily retry deletion (cluster hidden from UI)
- Need to manually edit database

---

## Code Quality Issues

### 8. **Inconsistent Error Handling in `anonCSRF()`**
**File:** [internal/server/handlers_auth.go](internal/server/handlers_auth.go#L88-L97)

**Issue:**
The function handles the "no cookie" case but the implicit nil behavior if something else fails:

```go
func (s *Server) anonCSRF(w http.ResponseWriter, r *http.Request) string {
    if c, err := r.Cookie(anonCSRFCookie); err == nil && c.Value != "" {
        return c.Value
    }
    t := auth.GenerateCSRFToken()
    http.SetCookie(w, &http.Cookie{...})
    return t
}
```

This is actually fine, but it's worth noting that if `r.Cookie()` returns a cookie with empty Value, we regenerate, which is correct defensive behavior.

---

### 9. **Missing Context Timeout Boundaries**
**File:** Multiple handlers ([handlers_clusters.go](internal/server/handlers_clusters.go#L86), [handlers_cluster_detail.go](internal/server/handlers_cluster_detail.go#L25))

**Issue:**
Some handlers create child contexts with timeouts, but don't consistently use them:

```go
func (s *Server) handleClusterStatus(...) {
    ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
    defer cancel()
    
    status, err := capi.GetStatus(ctx, s.dataDir, s.binDir, name)  // Uses child ctx ✓
    
    if status.Found {
        _, err := capi.GetWorkloadKubeconfig(ctx, ...)  // Uses child ctx ✓
    }
    
    s.db.Exec(`UPDATE clusters SET status = ? WHERE name = ?`, ...)  // Uses parent ctx ✗
}
```

Minor inconsistency, but database operations should ideally also respect the timeout.

---

### 10. **Lack of Request Validation in Form Parsing**
**File:** [internal/server/handlers_clusters.go](internal/server/handlers_clusters.go#L516-L530)

**Issue:**
The manifest YAML and cluster name are taken directly from form values without re-validation:

```go
yaml := r.FormValue("manifest_yaml")
if name == "" || yaml == "" {
    s.renderClustersPanel(...)
    return
}
// No validation that yaml is ACTUALLY valid YAML or that it was generated by clusterctl!
```

If the preview HTML form is manually modified, an invalid manifest could be stored.

**Impact:** Low (only affects authenticated operator), but could cause confusing cluster provisioning failures.

---

## Summary of Fixes

| Bug | Severity | Fix Complexity | Risk |
|-----|----------|-----------------|------|
| Missing rows.Err() | Critical | Low | High - silent data loss |
| Missing rows.Scan() error check | Critical | Low | High - partial data |
| Cluster insertion race | Critical | Medium | High - orphaned records |
| Template build race | High | Medium | Medium - build failures |
| Secret redaction timing | High | Low | High - credentials exposed |
| IP range overflow | Medium | Low | Low - rare edge case |
| Cluster deletion cleanup | Medium | Medium | Medium - orphaned records |
| Context timeout consistency | Low | Low | Low - minor |
| Form validation | Low | Low | Low - auth-gated |

---

## Recommendations

### Immediate Actions (This Sprint)
1. ✅ Add `rows.Err()` checks after all database row iterations
2. ✅ Add error handling for `rows.Scan()` calls
3. ✅ Track secrets immediately on input parsing
4. ✅ Move cluster row insert to AFTER job starts

### Short Term (Next Sprint)
1. Implement file-based lock for template builds to prevent concurrent builds on shared Docker volume
2. Add manifest YAML validation before storing
3. Improve cluster deletion error handling and visibility

### Long Term
1. Consider database transactions for cluster creation
2. Add retry logic with exponential backoff for job cleanup failures
3. Implement audit logging for cluster lifecycle operations
