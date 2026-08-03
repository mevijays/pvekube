# PVEKube: Getting Started

PVEKube is a single-binary web application for managing Kubernetes clusters on Proxmox via Cluster API. This guide walks you through building and running it.

## Prerequisites

### Build Machine (macOS/Linux)
- **Go 1.23+** (download from https://golang.org/dl/)
- **Git**
- ~500 MB disk space

### Ubuntu VM (Target Deployment)
- **Ubuntu 22.04 LTS or later**
- **2+ vCPU, 2+ GB RAM** (minimum; 4+ recommended)
- **Internet access** to pull container images and download binaries
- **sudo** privileges
- **Docker** (for image-builder integration)
- **5 GB+ free disk space** (for binaries, logs, templates)

### Proxmox Infrastructure
- **Proxmox VE 7.x or 8.x or 9.x** (tested on 9.1.4)
- **API token** with VM creation + management permissions (recommend a dedicated user like `capmox@pve`)
- **Network bridge** for cluster node VMs (typically `vmbr0`)
- **VM storage pool** with sufficient capacity (LVM, ZFS, or local)
- **Kubernetes 1.31.x support** in Proxmox (for CAPMOX v0.9.0)

## Step 1: Build the Binary

On your build machine (macOS or Linux):

```bash
git clone https://github.com/ionos-cloud/cluster-api-provider-proxmox
cd cluster-api-provider-proxmox
go build -o pvekube ./cmd/pvekube
```

The binary `pvekube` will be created in the current directory (~20 MB, statically linked).

Alternatively, if you prefer to clone this repository:

```bash
git clone <this-repo>
cd golang-proxmox-clusterctl-k8s
go build -o pvekube ./cmd/pvekube
```

## Step 2: Prepare Ubuntu VM

### 2a. Launch Ubuntu VM
1. In Proxmox, create a new VM with Ubuntu 22.04 LTS (or newer)
2. Allocate at least 2 vCPU, 2 GB RAM, 20 GB disk
3. Enable DHCP or assign a static IP
4. Boot and note the IP address (e.g., `192.168.1.50`)

### 2b. Install System Dependencies
SSH into the VM and run:

```bash
sudo apt-get update
sudo apt-get install -y \
  docker.io \
  curl \
  wget \
  git \
  openssl \
  ca-certificates
```

Add your user to the docker group (so you don't need `sudo` for docker):

```bash
sudo usermod -aG docker $USER
newgrp docker
```

### 2c. Install Kind (Kubernetes in Docker)
PVEKube auto-downloads kind, clusterctl, and kubectl, but pre-installing saves time on first boot:

```bash
# Kind
curl -L https://github.com/kubernetes-sigs/kind/releases/download/v0.20.0/kind-linux-amd64 \
  -o kind && chmod +x kind && sudo mv kind /usr/local/bin/

# Verify
kind version
```

## Step 3: Copy Binary to Ubuntu VM

From your build machine:

```bash
scp pvekube user@<vm-ip>:/home/user/
ssh user@<vm-ip>
chmod +x pvekube
```

Or, if you built directly on the VM:

```bash
cd /home/user
./pvekube --help
```

## Step 4: Create Data Directory and Run PVEKube

On the Ubuntu VM:

```bash
# Create data directory (stores SQLite database, secrets, binaries, logs)
mkdir -p ~/pvekube-data

# Run PVEKube
./pvekube --data-dir ~/pvekube-data --listen 0.0.0.0:8080
```

Output should show:
```
PVEKube server listening on 0.0.0.0:8080
Navigate to http://<vm-ip>:8080/setup to get started
```

## Step 5: Initial Setup

1. **Open browser** to `http://<vm-ip>:8080/setup`
2. **Set admin password** (used for all login attempts)
3. **Click "Sign in"** — you're in!

## Step 6: Connect Proxmox

1. Navigate to **Proxmox** tab
2. Enter your Proxmox details:
   - **Host/IP**: Your Proxmox hostname or IP (e.g., `proxmox.example.com` or `192.168.1.100`)
   - **Token ID**: Your API token (e.g., `capmox@pve!api-token-name`)
   - **Secret**: The token secret (long alphanumeric string)
   - **Insecure TLS** (optional): Check only if Proxmox uses self-signed HTTPS cert
3. **Click "Connect"** — PVEKube queries Proxmox for nodes, bridges, and storage pools

### Getting a Proxmox API Token

In Proxmox Web UI:
1. Go to **Datacenter → Permissions → API Tokens**
2. **Create Token** — user: `capmox@pve`, expires: never, privilege: Administrateur (or grant minimal: PVE VM/LXC operations)
3. Copy the **Token ID** and **Secret** (displayed once; save it)

## Step 7: Set Up Prerequisites

On the **Prerequisites** tab, PVEKube will auto-download and verify:
- **kind** (Kubernetes in Docker)
- **clusterctl** (Cluster API bootstrap)
- **kubectl** (Kubernetes CLI)

Each one shows as it completes. If any fail, PVEKube surfaces the error and offers to retry.

**This creates a permanent KIND management cluster** on the Ubuntu VM — all Kubernetes workload clusters are reconciled from this single management cluster.

## Step 8: Build a VM Template

Templates are pre-built VM snapshots (Proxmox clones of a template VM) that include:
- Debian/Ubuntu with kubeadm pre-installed
- Kubernetes `v1.31.4` or another pinned version
- Image-builder tools (Packer/Ansible)

### Option A: Use Pre-Built Template (Fastest)
If you have an existing Proxmox template VM:
1. Note its **VMID** (e.g., `100`)
2. Go to **Templates** tab
3. **Create Template**:
   - **OS Flavor**: Select the OS (e.g., Ubuntu 22.04)
   - **Kubernetes Version**: `v1.31.4`
   - **Node**: Select the Proxmox node hosting the template
   - **Bridge**: Select the network bridge (e.g., `vmbr0`)
   - **Storage Pool**: Select the pool (auto-detects disk format)
4. **Click "Preview"** to see the packer build command
5. **Click "Build"** — PVEKube launches `docker run` with image-builder

The build takes **10–15 minutes** and streams live logs. On completion, it stores the template VMID for later cluster launches.

### Option B: Build from Scratch
If you don't have a pre-built template, the **Templates** tab will guide you. This requires Packer + Ansible inside the container — PVEKube automates all of it.

## Step 9: Launch a Kubernetes Cluster

1. Go to **Clusters** tab
2. **Fill the form**:
   - **Cluster name**: e.g., `my-k8s`
   - **Template**: Pick the template you just built
   - **Control Plane Count**: `1` (standalone) or `3` (HA)
   - **Worker Count**: `2` or more
   - **Machine sizing**: CPU sockets, cores, RAM (defaults: 2 sockets, 4 cores, 8 GB)
   - **Network settings**:
     - **Gateway**: e.g., `10.10.10.1` (your subnet gateway)
     - **Subnet Prefix**: e.g., `24` (for `/24` CIDR)
     - **Control Plane Endpoint (VIP)**: e.g., `10.10.10.10` (virtual IP for cluster API)
     - **Node IP Range**: e.g., `10.10.10.20-10.10.10.100` (for cluster nodes)
     - **DNS Servers**: e.g., `8.8.8.8, 1.1.1.1`
3. **Click "Check IP Plan"** — PVEKube validates subnet math and pings the gateway/VIP
4. **Click "Preview Manifest"** — PVEKube generates the CAPI manifest and shows it
5. **Click "Apply — launch this cluster"** — Manifest is applied to the management cluster

Cluster creation takes **5–10 minutes**:
1. Proxmox clones the template to create control plane VM
2. If HA, clones 2 more control plane VMs
3. Clones worker VMs
4. kubeadm on each node brings up the cluster
5. PVEKube watches the Cluster/Machine objects and updates the status page in real-time

## Step 10: Access Your Cluster

Once the cluster reaches **Provisioned** phase:
1. Go to the **Cluster detail page** (click the cluster name)
2. **Click "Download kubeconfig"** — saves the workload cluster's kubeconfig YAML
3. **On your local machine**:
   ```bash
   export KUBECONFIG=my-k8s-kubeconfig.yaml
   kubectl get nodes
   kubectl get pods -A
   ```

You now have a fully functional Kubernetes cluster running on Proxmox!

## Cluster Management

### Scale Workers
On the cluster detail page, change the worker count and click "Scale workers" — the MachineDeployment replica count updates, CAPI handles the rest.

### Scale Control Plane
Change to 1, 3, or 5 control plane nodes — quorum is enforced. Scaling from 1→3 adds HA.

### Download Kubeconfig
Available once the cluster reaches `Provisioned` phase.

### Delete Cluster
Click "Delete Cluster" on the detail page — cascades through CAPI to delete all Machines and underlying Proxmox VMs.

## Troubleshooting

### "Proxmox connection failed"
- Check IP/token/secret in the Proxmox connection form
- Verify the API token has sufficient permissions (Administrateur or equivalent)
- If using self-signed HTTPS, check "Insecure TLS"

### "Template build failed"
- Check the live build logs in PVEKube
- Ensure Proxmox has internet access (for image-builder to pull packages)
- Verify the template VM VMID and node are correct

### "Cluster stuck in Provisioning"
- Go to the cluster detail page and check **Conditions**
- Common issues:
  - `InfrastructureReady = False`: CAPMOX controller can't reach Proxmox (credentials, VMID issues)
  - `ControlPlaneInitialized = False`: kubeadm bootstrap is running (normal for a few minutes)
  - `RemoteConnectionProbe = ProbeFailed`: cluster API server not yet reachable
- Solutions:
  - Verify Proxmox credentials are correct (PVEKube syncs them automatically)
  - Check Proxmox VM console to see if kubeadm is running
  - Give it more time — full cluster bring-up can take 5–10 minutes

### "Network connectivity issues"
- Ensure the control plane endpoint VIP (`10.10.10.10` in the example) is reachable from your machine
- Verify the subnet gateway is correct and accessible
- Check that the network bridge on Proxmox has internet access (if pulling images)

## Running PVEKube Persistently

For production, run PVEKube as a systemd service:

```bash
sudo tee /etc/systemd/system/pvekube.service > /dev/null <<EOF
[Unit]
Description=PVEKube Kubernetes Cluster Manager
After=network.target docker.service
Requires=docker.service

[Service]
Type=simple
User=$USER
WorkingDirectory=/home/$USER
ExecStart=/home/$USER/pvekube --data-dir /home/$USER/pvekube-data --listen 0.0.0.0:8080
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable pvekube
sudo systemctl start pvekube
sudo systemctl status pvekube
```

Check logs:
```bash
journalctl -u pvekube -f
```

## Next Steps

- **Documentation**: See [PLAN.md](PLAN.md) for architecture, design decisions, and Phase 10 enhancements
- **Feedback**: Issues and discussions welcome
- **Token rotation**: Periodically refresh your Proxmox API token for security

---

**Happy clustering!**
