---
layout: default
title: Features
---

# Features

PVEKube provides a complete, browser-based workflow for Kubernetes cluster management on Proxmox.

## Dashboard

**Home screen** shows:
- System status (management cluster health, available nodes)
- Quick access to all major features
- Recent cluster activity
- Prerequisite checklist status

## Prerequisites Verification

**Automated checklist** verifying:
- ✅ **kind** binary (downloads if missing)
- ✅ **clusterctl** binary (downloads if missing)
- ✅ **kubectl** binary (downloads if missing)
- ✅ Binary checksums (SHA-256 verification)
- ✅ Docker daemon connectivity
- ✅ KIND cluster existence

**Features**:
- Live download progress with ETA
- Auto-remediation (retry downloads)
- Checksum verification for security
- One-time setup — binaries cached locally

## Proxmox Connection

**Browser-based form** for connecting to Proxmox:
- **Host/IP**: Proxmox server address
- **Token ID**: API token (e.g., `capmox@pve!token-name`)
- **Secret**: Token secret (encrypted at rest)
- **Insecure TLS** (optional): For self-signed certificates
- **Test connection** before saving

**Discovery queries** (auto-run on connect):
- **Nodes**: Available Proxmox nodes for template/workload VMs
- **Network bridges**: Available bridges (vmbr0, vmbr1, etc.)
- **Storage pools**: Available storage for VMs and disks
- **Disk formats**: Auto-detects qcow2 vs. raw per pool

**Credentials management**:
- Encrypted storage in SQLite
- Automatic sync to management cluster
- Controller restart on update (for real-time credential refresh)

## VM Template Builder

**Semi-automated image building** using kubernetes-sigs/image-builder v0.1.55.

### Supported OS Flavors
- Ubuntu 22.04 LTS, 24.04 LTS
- Debian 11, 12
- Rocky Linux 8, 9
- AlmaLinux 8, 9
- Flatcar Container Linux

### Build Workflow

1. **Form input**:
   - Select OS flavor
   - Select Kubernetes version (e.g., v1.31.4)
   - Select source Proxmox node
   - Select storage pool and network bridge

2. **Packer orchestration** (inside Docker):
   - Clones a base template on Proxmox
   - Provisions via cloud-init
   - Ansible playbook installs kubeadm + kubelet
   - goss validates the image
   - Snapshot preserved as template

3. **Live build logs**:
   - Real-time streaming via SSE
   - See Packer provisioning steps
   - Ansible task output
   - Validation results

4. **Storage**:
   - Template VMID recorded in SQLite
   - Later used for cluster node cloning

### Template Management

- **List templates**: View all built templates with OS, K8s version, node
- **Reuse templates**: Use the same template for multiple clusters

## Cluster Designer

**Interactive form** for defining a Kubernetes cluster with persistent defaults and real-time hardware accounting.

### Real-Time Hardware Discovery

- **Live Memory Accounting**: Whenever the cluster creation form is loaded, PVEKube bypasses cached metrics and executes a live Proxmox API discovery to calculate exact "Free to allocate" memory across nodes.
- **Accurate Sizing Guidance**: Immediately reflects guest memory resizings or shutdowns on Proxmox without needing manual cache refreshes.

### Persistent Cluster Defaults (`cluster_defaults`)

- **Saved Form Inputs**: Remembers operator configuration defaults (e.g. SSH public keys, private registry URLs, registry CA certificates) in the `cluster_defaults` SQLite table.
- **Sealed Secrets**: Private registry passwords are encrypted at rest using AES-GCM (same mechanism as Proxmox API tokens).
- **Auto Pre-Fill**: Pre-fills saved preferences on new cluster creation, eliminating repetitive copy-pasting across clusters.

### Cluster Settings

- **Name**: DNS-safe cluster identifier (validated)
- **Template**: Select which OS+K8s template to clone
- **Control Plane Count**: 1 (single) or 3 (HA)
- **Worker Count**: 0 or more
- **CNI**: None (bring your own), Cilium, or Calico

### Machine Sizing

- **CPU Sockets**: Number of vCPU sockets (default: 2)
- **CPU Cores**: Cores per socket (default: 4)
- **Memory**: RAM in MiB (default: 8048)
- **Boot Disk Size**: Primary volume size in GB (default: 100)

### Networking Configuration

- **Gateway**: Subnet gateway (validated via ICMP probe)
- **Subnet Prefix**: CIDR prefix length (e.g., 24 for /24)
- **Control Plane Endpoint (VIP)**: Virtual IP for cluster API server
  - Must be on the same subnet
  - Checked for occupancy via ICMP probe
- **Node IP Range**: Range for cluster node IPs (e.g., 10.10.10.20-10.10.10.80)
  - Parsed and validated for sufficient pool size
- **DNS Servers**: Comma-separated list for all nodes
- **SSH Public Keys**: Optional; injected via cloud-init

### Allowed Nodes

- **Proxmox node selection**: Check nodes where VMs can be placed
- **CAPMOX constraint**: Only selected nodes can run cluster VMs

## IP Plan Validator

**Network validation** before manifest generation.

### Checks Performed

- ✅ **Subnet math**: VIP, node range, gateway all within configured subnet
- ✅ **Range size**: Sufficient IP addresses for all control plane + worker nodes
- ✅ **Overlap detection**: VIP not in node IP range; node range doesn't overlap gateway
- ✅ **Address occupancy**:
  - Ping gateway (warns if responds — indicates live network)
  - Ping VIP (errors if responds — would conflict)

### User Feedback

- **Green checkmark**: All checks pass
- **Yellow warning**: Potential issue (e.g., gateway responds unexpectedly)
- **Red error**: Blocker (e.g., VIP already in use; range too small)

## Manifest Generation & Preview

**clusterctl** wrapper for rendering optimized cluster manifests.

### Process & Transformations

1. Calls `clusterctl generate cluster` with Proxmox connection details
2. Substitutes all variables (VIP, node range, template VMID, etc.)
3. **Linked Clones Transformation**: Patches `full: true` to `full: false` in `ProxmoxMachineTemplate` resources. This forces copy-on-write linked cloning, enabling sub-second VM instantiation instead of slow 20GB+ disk copies.
4. **Validation Webhook Alignment**: Automatically strips `format: qcow2` declarations from `ProxmoxMachineTemplate` specs, satisfying CAPMOX's strict webhook requirement that format must be omitted when `full: false` is used.

### Preview Screen

- **View YAML**: Full manifest displayed in code editor
- **Verify before apply**: User can inspect every resource
- **Syntax check**: YAML is valid before showing

## Cluster Launch & Status

**One-click apply** of Cluster API manifests to the management cluster with guaranteed provisioning sequence.

### Launch Workflow & Multi-Stage Readiness Gates

1. Click **"Apply — launch this cluster"**
2. **Sync Credentials**: Proxmox API credentials synced to management cluster secret
3. **Controller Restart**: CAPMOX controller restarted if credentials changed
4. **Apply Manifest**: `kubectl apply` pushes Cluster API objects to KIND management cluster
5. **Kubeconfig Secret Wait**: Waits for management cluster to publish workload `<cluster>-kubeconfig` secret
6. **Control Plane API Stability Check**: Performs a 5-consecutive-success healthcheck loop (5s apart) against the workload API server to ensure stability before scheduling downstream workloads
7. **Node & CNI Readiness Gate (`WaitForNodesReadyStep`)**: Polls `kubectl get nodes` until all expected control plane and worker nodes reach `Ready` state (verifying cloud-init, CNI pod startup, and node registration)
8. **Post-Provisioning Addon Installation**: Safely applies requested manifests (`metrics-server`, `Istio`, `MetalLB`) without encountering `dial tcp: no route to host` errors

### Live Status Monitoring

**Automatic polling** (every 5 seconds) of cluster state:

- **Cluster Phase**: Provisioning → Provisioned → Deleting
- **CAPI v1beta2 Condition Evaluation**: Parses `status.conditions` (`ControlPlaneAvailable`, `InfrastructureReady`, `WorkersAvailable`) to accurately represent readiness across Cluster API API contract versions
- **Readiness flags**:
  - Control Plane Ready
  - Infrastructure Ready
  - Overall Available

- **Conditions** (shown as expandable cards):
  - Available
  - RemoteConnectionProbe
  - InfrastructureReady
  - ControlPlaneInitialized
  - ControlPlaneAvailable
  - WorkersAvailable
  - (and more)
  - Each shows Type, Status, Reason, Message

- **Machines Table**:
  - Machine name
  - Role (control-plane or worker)
  - Phase (Pending, Running, Failed, etc.)
  - IP address (when assigned)
  - Kubernetes node name (when bootstrapped)
  - Kubernetes version

## Kubeconfig Download

**Secure kubeconfig delivery** for cluster access.

- Available once cluster reaches "Provisioned" phase
- Fetches from Kubernetes Secret (capmox-manager-credentials, auto-created by CAPI)
- Base64-decoded before download
- Browser saves as `{cluster-name}-kubeconfig.yaml`
- User can immediately use with kubectl

## Cluster Scaling

### Scale Workers

- **MachineDeployment replica adjustment**
- Click "Scale workers", enter count, submit
- CAPI scales MachineDeployment
- VMs cloned/destroyed automatically
- Workload nodes join/leave cluster

### Scale Control Plane

- **KubeadmControlPlane replica adjustment**
- Selection: 1, 3, or 5 (quorum enforcement)
- CAPI scales control plane
- Etcd membership updated automatically
- High availability transitions handled

## Cluster Deletion

**Cascading teardown** of all workload infrastructure.

- Click **"Delete cluster"** on cluster detail page
- Confirmation required (prevents accidents)
- Deletes Cluster object
- CAPI cascades to delete all Machines
- CAPMOX deletes underlying Proxmox VMs
- Network resources cleaned up
- Status updates in real-time

## Cluster Upgrade

**Rolling replacement** of Kubernetes versions.

### Workflow

1. **Select new K8s version**: Must have a pre-built template
2. **Create new ProxmoxMachineTemplate** in management cluster
3. **Update KubeadmControlPlane** to reference new template + version
4. **Update MachineDeployment** to reference new template + version
5. **CAPI orchestrates rolling replacement**:
   - Control plane nodes replaced one at a time
   - Etcd replication maintained
   - Workers replaced per MachineDeployment rollout strategy
   - No downtime (with 3+ control planes)

### Verification

- Live status shows nodes in "Pending" → "Running" during replacement
- Conditions track ControlPlaneAvailable state through upgrade
- User can watch nodes rejoin cluster in real-time

## Job History & Logging

**Persistent job tracking** for all operations.

### Job Types

- `prereq.download_*`: Binary downloads
- `cluster.imagebuilder.build`: Template builds
- `cluster.apply`: Manifest application
- `cluster.scale_workers`: Worker scaling
- `cluster.scale_controlplane`: Control plane scaling
- `cluster.delete`: Cluster deletion
- `cluster.upgrade`: Version upgrade

### For Each Job

- **Status**: pending → running → succeeded / failed / cancelled
- **Steps**: Each job has one or more named steps
- **Logs**: Persisted line-by-line to disk
- **Duration**: Start/end timestamps
- **Error messages**: Surfaced if failed

### Log Access

- **Live**: SSE stream while job runs
- **Historical**: Jobs dashboard shows recent jobs
- **Debugging**: Server-side logs at `~/pvekube-data/logs/`

## Security Features

### Authentication

- **Admin password**: Set during setup
- **Session management**: Secure cookies, CSRF tokens per session
- **No external auth** (LDAP/OAuth) in core — can be added as extension

### Credential Encryption

- **Proxmox tokens**: AES-GCM encrypted in SQLite
- **Master key**: Generated at first startup, stored securely (must be backed up)
- **Redaction layer**: Secrets redacted from all logs before display

### Network Security

- **HTTPS**: Recommended for production (can use reverse proxy)
- **API token auth**: All Proxmox API calls use token-based auth
- **No credential exposure**: Tokens never logged or displayed

## Advanced Features

### Systemd Integration

- Run as systemd service for automatic startup/restart
- Journalctl integration for logs
- Health monitoring built in

### Data Persistence

- All state in SQLite (portable, no external DB)
- Backupable: single `pvekube.db` file
- WAL mode for reliability

### Concurrent Operations

- Multiple jobs can run in parallel
- Cluster operations don't block each other
- SSE streams for real-time updates

---

**[← Back to Docs](/)** | **[Next: API Reference](api-reference/)**
