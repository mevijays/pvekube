---
layout: default
title: API Reference
---

# API Reference

PVEKube exposes both a **Web UI** (HTML rendered templates) and a **programmatic interface** through HTTP endpoints. The database schema is also documented below for users who need to query or backup SQLite directly.

## HTTP Endpoints

### Authentication

#### POST /login
Login with admin password.

**Request:**
```
Content-Type: application/x-www-form-urlencoded
password=<password>
```

**Response:** 302 redirect to `/dashboard` (sets session cookie)

#### GET /setup
Initial setup page (create admin password).

**Response:** HTML setup form (visible only if no password set)

#### GET /logout
Logout (clears session).

**Response:** 302 redirect to `/login`

### Dashboard

#### GET /
Dashboard homepage.

**Response:** HTML dashboard with status overview

#### GET /dashboard
Same as `/`

### Prerequisites

#### GET /prereqs
Prerequisites checklist page.

**Response:** HTML checklist of required binaries

#### POST /prereqs/check
Run prerequisites checks (returns HTML).

**Response:** HTML status table

#### GET /prereqs/list
Fetch prerequisite status as HTML (used for live updates).

**Response:** HTML checklist partial

#### GET /prereqs/download/{kind}
SSE endpoint for live download logs.

**Query params:**
- `kind` — binary to download (kind, clusterctl, kubectl)

**Response:** Server-Sent Events stream

### Proxmox Connection

#### GET /proxmox
Proxmox connection page.

**Response:** HTML form

#### POST /proxmox/test
Test Proxmox connection.

**Request:**
```json
{
  "url": "https://proxmox.example.com:8006",
  "token_id": "user@pve!token",
  "secret": "secret123",
  "insecure_tls": false
}
```

**Response:**
```json
{
  "ok": true,
  "message": "Connection successful"
}
```

#### POST /proxmox/save
Save Proxmox credentials (encrypted).

**Request:** Same as `/proxmox/test`

**Response:**
```json
{
  "ok": true,
  "message": "Credentials saved"
}
```

#### POST /proxmox/discovery
Auto-discover nodes, bridges, pools.

**Response:**
```json
{
  "nodes": ["node1", "node2"],
  "bridges": ["vmbr0", "vmbr1"],
  "pools": ["local", "local-lvm"],
  "pool_disk_formats": {
    "local": "raw",
    "local-lvm": "qcow2"
  }
}
```

### Templates

#### GET /templates
Template listing page.

**Response:** HTML template list

#### GET /templates/list
Fetch template list as HTML partial.

**Response:** HTML table of templates

#### GET /templates/form
Template creation form.

**Response:** HTML form

#### POST /templates/create
Create a new template build job.

**Request:**
```json
{
  "os_flavor": "ubuntu-2204",
  "kubernetes_version": "v1.31.4",
  "node": "node1",
  "bridge": "vmbr0",
  "storage": "local"
}
```

**Response:**
```json
{
  "job_id": 123,
  "status": "pending"
}
```

#### GET /templates/build/{job_id}
SSE endpoint for live build logs.

**Response:** Server-Sent Events stream (stdout + stderr from packer)

### Clusters

#### GET /clusters
Cluster listing page.

**Response:** HTML cluster list

#### GET /clusters/list
Fetch cluster list as HTML partial.

**Response:** HTML panel with cluster cards

#### GET /clusters/form
Cluster creation form.

**Response:** HTML form

#### POST /clusters/check-ip-plan
Validate IP plan (subnet math, ping probes).

**Request:**
```json
{
  "gateway": "10.10.10.1",
  "subnet_prefix": 24,
  "control_plane_endpoint": "10.10.10.10",
  "node_ip_range": "10.10.10.20-10.10.10.100",
  "control_plane_count": 3,
  "worker_count": 2
}
```

**Response:**
```json
{
  "valid": true,
  "warnings": [],
  "errors": []
}
```

#### POST /clusters/preview
Generate and preview CAPI manifest.

**Request:**
```json
{
  "name": "my-k8s",
  "template_id": 5,
  "control_plane_count": 3,
  "worker_count": 2,
  "cpu_sockets": 2,
  "cpu_cores": 4,
  "memory_mib": 8048,
  "boot_disk_gb": 100,
  "gateway": "10.10.10.1",
  "subnet_prefix": 24,
  "control_plane_endpoint": "10.10.10.10",
  "node_ip_range": "10.10.10.20-10.10.10.100",
  "dns_servers": "8.8.8.8, 1.1.1.1",
  "allowed_nodes": ["node1", "node2"],
  "cni_flavor": "cilium",
  "ssh_keys": ""
}
```

**Response:**
```json
{
  "yaml": "apiVersion: cluster.x-k8s.io/v1beta2\nkind: Cluster\n..."
}
```

#### POST /clusters/apply
Apply cluster manifest (launch cluster).

**Request:** Same as `/clusters/preview`

**Response:**
```json
{
  "job_id": 456,
  "status": "pending"
}
```

#### GET /clusters/{name}
Cluster detail page.

**Response:** HTML cluster detail with status

#### GET /clusters/{name}/status
Fetch cluster status as JSON.

**Response:**
```json
{
  "name": "my-k8s",
  "phase": "Provisioned",
  "ready": true,
  "conditions": [
    {
      "type": "Available",
      "status": "True",
      "reason": "AsExpected",
      "message": "This is a stable cluster"
    }
  ],
  "control_plane_ready": true,
  "infrastructure_ready": true,
  "machines": [
    {
      "name": "my-k8s-control-plane-abc12",
      "phase": "Running",
      "role": "control-plane",
      "ip": "10.10.10.20",
      "kubernetes_node_name": "my-k8s-control-plane-abc12",
      "kubernetes_version": "v1.31.4"
    }
  ]
}
```

#### GET /clusters/{name}/kubeconfig
Download kubeconfig for cluster.

**Response:** YAML file (kubeconfig)

#### POST /clusters/{name}/scale-workers
Scale worker nodes.

**Request:**
```json
{
  "worker_count": 5
}
```

**Response:**
```json
{
  "job_id": 789,
  "status": "pending"
}
```

#### POST /clusters/{name}/scale-controlplane
Scale control plane (1, 3, or 5 only).

**Request:**
```json
{
  "control_plane_count": 3
}
```

**Response:**
```json
{
  "job_id": 790,
  "status": "pending"
}
```

#### POST /clusters/{name}/delete
Delete cluster (cascading teardown).

**Response:**
```json
{
  "job_id": 791,
  "status": "pending"
}
```

#### POST /clusters/{name}/upgrade
Upgrade Kubernetes version (rolling replacement).

**Request:**
```json
{
  "new_kubernetes_version": "v1.32.0",
  "new_template_id": 7
}
```

**Response:**
```json
{
  "job_id": 792,
  "status": "pending"
}
```

#### GET /clusters/{name}/apply
SSE endpoint for live apply/operation logs.

**Response:** Server-Sent Events stream

## Database Schema

**File location:** `~/pvekube-data/pvekube.db`

### proxmox_connections

Stores Proxmox connection details (one active connection).

```sql
CREATE TABLE proxmox_connections (
  id INTEGER PRIMARY KEY,
  url TEXT NOT NULL,
  token_id TEXT NOT NULL,
  secret_encrypted BLOB NOT NULL,  -- AES-GCM encrypted
  insecure_tls BOOLEAN DEFAULT 0,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

### templates

Stores built VM templates (snapshots with K8s pre-installed).

```sql
CREATE TABLE templates (
  id INTEGER PRIMARY KEY,
  os_flavor TEXT NOT NULL,           -- e.g., ubuntu-2204
  kubernetes_version TEXT NOT NULL,  -- e.g., v1.31.4
  node TEXT NOT NULL,                -- Proxmox node name
  vmid INTEGER NOT NULL,             -- Snapshot VMID
  bridge TEXT NOT NULL,              -- Network bridge used
  storage TEXT NOT NULL,             -- Storage pool used
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

### clusters

Stores launched Kubernetes clusters.

```sql
CREATE TABLE clusters (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,         -- Cluster name (DNS-safe)
  status TEXT DEFAULT 'Unknown',     -- Provisioning, Provisioned, Deleting, Failed
  phase TEXT DEFAULT 'Unknown',      -- From Cluster object phase
  manifest_yaml TEXT,                -- Full CAPI manifest applied
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

### jobs

Stores job history (builds, applies, scales, etc.).

```sql
CREATE TABLE jobs (
  id INTEGER PRIMARY KEY,
  kind TEXT NOT NULL,                -- e.g., cluster.apply, cluster.imagebuilder.build
  status TEXT DEFAULT 'pending',     -- pending, running, succeeded, failed, cancelled
  error TEXT,                        -- Error message if failed
  output_log_path TEXT,              -- Path to log file
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  finished_at DATETIME              -- When job completed
);
```

### jobs_steps

Stores step-level details for each job.

```sql
CREATE TABLE jobs_steps (
  id INTEGER PRIMARY KEY,
  job_id INTEGER NOT NULL,
  step_index INTEGER NOT NULL,       -- 0-based index
  name TEXT NOT NULL,                -- Step name (e.g., "Download binaries", "Apply manifest")
  status TEXT DEFAULT 'pending',     -- pending, running, succeeded, failed
  line_count INTEGER DEFAULT 0,      -- Number of log lines persisted
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(job_id) REFERENCES jobs(id) ON DELETE CASCADE
);
```

### sessions

Stores user sessions (for authentication).

```sql
CREATE TABLE sessions (
  id INTEGER PRIMARY KEY,
  token TEXT UNIQUE NOT NULL,        -- Secure random session token
  csrf_token TEXT NOT NULL,          -- CSRF protection token
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  expires_at DATETIME NOT NULL
);
```

### admin_password

Stores hashed admin password (set during setup).

```sql
CREATE TABLE admin_password (
  id INTEGER PRIMARY KEY,
  hash TEXT NOT NULL,                -- Argon2id hash
  set_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

## Jobs Output Logging

Each job step writes logs to a file:

**Location:** `~/pvekube-data/logs/job-{job_id}-step-{step_index}.log`

**Format:** Line-delimited plain text (one log line per file line)

**Example:**
```
$ cat ~/pvekube-data/logs/job-1-step-0.log
Downloading kind v0.20.0...
[████████████░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░] 15%
[███████████████████████████████████████████████░░░░░░░░░░░░░] 80%
Downloaded successfully.
Verifying checksum...
SHA256: 2c926fb3595... MATCH
```

## Error Responses

All endpoints return JSON error responses on failure:

```json
{
  "error": "validation failed",
  "message": "Subnet prefix must be between 8 and 30"
}
```

HTTP status codes:
- **200** — Success
- **400** — Bad request (validation error)
- **401** — Unauthorized (login required)
- **403** — Forbidden (CSRF token mismatch)
- **404** — Not found
- **500** — Server error

## Webhook Integrations (Future)

PVEKube currently does not support outbound webhooks. Future versions may support:
- Cluster state change notifications
- Job completion callbacks
- Health check endpoints

---

**[← Back to Docs](/)** | **[Next: Troubleshooting](troubleshooting/)**
