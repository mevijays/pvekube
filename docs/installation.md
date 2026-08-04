---
layout: default
title: Installation & Setup
---

# Installation & Setup

## Prerequisites

### Build Machine (macOS or Linux)
- **Go 1.23+** — [Download](https://golang.org/dl/)
- **Git**
- ~500 MB free disk space

### Ubuntu VM (Deployment Target)
- **Ubuntu 22.04 LTS or newer**
- **2+ vCPU, 2+ GB RAM** minimum (4+ GB recommended)
- **Internet access** for container images and binary downloads
- **sudo** privileges
- **Docker** installed
- **5 GB+ free disk space** for binaries, SQLite database, and logs

### Proxmox Infrastructure
- **Proxmox VE 7.x, 8.x, or 9.x** (tested on 9.1.4)
- **API token** with VM management permissions
  - Create a dedicated user like `capmox@pve`
  - Or use existing `root@pam` with restricted token permissions
- **Network bridge** for cluster VMs (default: `vmbr0`)
- **VM storage pool** with adequate capacity (LVM, ZFS, or local storage)

## Step 1: Build the Binary

### Option A: Build for Linux x86_64 (Recommended)

If you're building on macOS or a different architecture, cross-compile for Linux x86_64:

```bash
git clone https://github.com/mevijays/pvekube
cd golang-proxmox-clusterctl-k8s

# Cross-compile for Linux x86_64
GOOS=linux GOARCH=amd64 go build -o pvekube ./cmd/pvekube

# Verify (should show ELF binary)
file pvekube
# Output: ELF 64-bit LSB executable, x86-64, version 1 (SYSV), statically linked...
```

### Option B: Build on Linux Target

If building directly on the Ubuntu VM:

```bash
git clone https://github.com/mevijays/pvekube
cd golang-proxmox-clusterctl-k8s
go build -o pvekube ./cmd/pvekube
```

## Step 2: Set Up Ubuntu VM

### 2a. Launch Ubuntu VM in Proxmox
1. Create a new VM with **Ubuntu 22.04 LTS** (or newer)
2. Allocate: **2+ vCPU**, **2+ GB RAM**, **20+ GB disk**
3. Enable DHCP or configure static IP
4. Boot and note the IP address (e.g., `192.168.1.50`)

### 2b. SSH and Install Dependencies

```bash
ssh user@<vm-ip>
sudo apt-get update
sudo apt-get install -y \
  docker.io \
  curl \
  wget \
  git \
  openssl \
  ca-certificates

# Add your user to docker group (avoids sudo for docker commands)
sudo usermod -aG docker $USER
newgrp docker

# Verify Docker
docker --version
```

### 2c. Pre-Download Binaries (Optional, Saves Time)

PVEKube auto-downloads kind, clusterctl, and kubectl on first run. To pre-install:

```bash
# kind (Kubernetes in Docker)
curl -L https://github.com/kubernetes-sigs/kind/releases/download/v0.20.0/kind-linux-amd64 \
  -o kind && chmod +x kind && sudo mv kind /usr/local/bin/

# clusterctl
curl -L https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.12.10/clusterctl-linux-amd64 \
  -o clusterctl && chmod +x clusterctl && sudo mv clusterctl /usr/local/bin/

# kubectl
curl -L https://dl.k8s.io/release/v1.31.4/bin/linux/amd64/kubectl \
  -o kubectl && chmod +x kubectl && sudo mv kubectl /usr/local/bin/

# Verify
kind version
clusterctl version
kubectl version --client
```

## Step 3: Transfer Binary to Ubuntu VM

### From Build Machine to VM

```bash
scp pvekube user@<vm-ip>:/home/user/
ssh user@<vm-ip>
chmod +x pvekube
```

### Verify Architecture

```bash
file pvekube
# Should show: ELF 64-bit LSB executable

./pvekube --help
# Should print usage information
```

## Step 4: Create Data Directory and Start PVEKube

```bash
# Create data directory (stores SQLite, secrets, binaries, logs)
mkdir -p ~/pvekube-data

# Run PVEKube
./pvekube --data-dir ~/pvekube-data --listen 0.0.0.0:8080
```

**Expected output:**
```
[INFO] PVEKube server listening on 0.0.0.0:8080
[INFO] Navigate to http://<vm-ip>:8080/setup to get started
```

## Step 5: Initial Setup (Browser)

1. **Open browser** to `http://<vm-ip>:8080/setup`
2. **Set admin password** (required for login)
3. Click **"Sign in"**
4. You're in! 🎉

## Step 6: Connect Proxmox

1. Navigate to **Proxmox** tab
2. Enter connection details:
   - **Host/IP**: Proxmox hostname or IP (e.g., `proxmox.example.com` or `192.168.1.100`)
   - **Token ID**: API token (e.g., `capmox@pve!api-token-name`)
   - **Secret**: Token secret (long alphanumeric string)
   - **Insecure TLS** (optional): Check only for self-signed HTTPS certs
3. Click **"Connect"**

### Getting a Proxmox API Token

In **Proxmox Web UI**:
1. Go to **Datacenter → Permissions → API Tokens**
2. Click **"Add"**
3. Fill in:
   - **User**: `capmox@pve` (create if needed)
   - **Token ID**: `api-token-name` (choose a name)
   - **Expire**: Never (or set an expiration)
   - **Privilege**: Administrator (or grant minimal VM/LXC permissions)
4. Click **"Add"**
5. **Copy the Token ID and Secret** (secret displayed once; save it securely)

## Step 7: Verify Prerequisites

1. Navigate to **Prerequisites** tab
2. PVEKube auto-downloads:
   - **kind** (Kubernetes in Docker)
   - **clusterctl** (Cluster API bootstrap)
   - **kubectl** (Kubernetes CLI)
3. Each shows download progress and checksum verification
4. If any fail, PVEKube surfaces the error with a retry button

**This creates a permanent KIND management cluster** on the Ubuntu VM. All Kubernetes workload clusters are reconciled from this single cluster.

## Step 8: Build a VM Template

VM templates are pre-built snapshots with kubeadm and Kubernetes installed.

1. Navigate to **Templates** tab
2. Click **"Create Template"**
3. Fill in:
   - **OS Flavor**: Ubuntu 22.04, Ubuntu 24.04, Debian, Flatcar, etc.
   - **Kubernetes Version**: `v1.31.4` (or pinned version)
   - **Node**: Proxmox node hosting the template
   - **Bridge**: Network bridge (e.g., `vmbr0`)
   - **Storage Pool**: Storage pool for VM
4. Click **"Preview"** to see the build command
5. Click **"Build"** — launches Packer in a container

**Build time**: 10–15 minutes with live streaming logs.

Once complete, the template VMID is stored for cluster launches.

## Step 9: Launch a Kubernetes Cluster

1. Navigate to **Clusters** tab
2. Fill in the form:
   - **Cluster name**: `my-k8s` (DNS-safe, lowercase with hyphens)
   - **Template**: Select the template from step 8
   - **Control Plane Count**: `1` (standalone) or `3` (HA)
   - **Worker Count**: `2` or more
   - **Machine sizing**: CPU sockets, cores, RAM (defaults: 2 sockets, 4 cores, 8 GB)
   - **Network settings**:
     - **Gateway**: Subnet gateway (e.g., `10.10.10.1`)
     - **Subnet Prefix**: CIDR prefix (e.g., `24` for `/24`)
     - **Control Plane Endpoint (VIP)**: Virtual IP for the cluster API server (e.g., `10.10.10.10`)
     - **Node IP Range**: IP range for cluster nodes (e.g., `10.10.10.20-10.10.10.100`)
     - **DNS Servers**: Comma-separated DNS servers (e.g., `8.8.8.8, 1.1.1.1`)
3. Click **"Check IP Plan"** — validates subnet math and probes network
4. Click **"Preview Manifest"** — generates CAPI manifest
5. Click **"Apply — launch this cluster"** — applies manifest to management cluster

**Cluster creation time**: 5–10 minutes
- Template cloned to create VMs
- kubeadm runs on each node
- Cluster API controllers reconcile the Cluster object
- Status updates in real-time on the cluster detail page

## Step 10: Access Your Cluster

1. Navigate to the **Cluster detail page** (click cluster name on Clusters tab)
2. Once status reaches **"Provisioned"**, click **"Download kubeconfig"**
3. On your local machine:
   ```bash
   export KUBECONFIG=my-k8s-kubeconfig.yaml
   kubectl get nodes
   kubectl get pods -A
   ```

🎉 **Your Kubernetes cluster on Proxmox is now running!**

## Production Setup

To run PVEKube as a systemd service:

```bash
sudo tee /etc/systemd/system/pvekube.service > /dev/null <<'EOF'
[Unit]
Description=PVEKube Kubernetes Cluster Manager
After=network.target docker.service
Requires=docker.service

[Service]
Type=simple
User=$USER
WorkingDirectory=$HOME
ExecStart=$HOME/pvekube --data-dir $HOME/pvekube-data --listen 0.0.0.0:8080
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable pvekube
sudo systemctl start pvekube
sudo systemctl status pvekube

# View logs
journalctl -u pvekube -f
```

## Next Steps

- **[Features Overview](features/)** — What you can do with PVEKube
- **[Architecture](architecture/)** — How it all fits together
- **[Troubleshooting](troubleshooting/)** — Common issues and solutions
- **[API Reference](api-reference/)** — HTTP endpoints and database schema

---

**Questions?** [Open an issue](https://github.com/mevijays/pvekube/issues) or start a [discussion](https://github.com/mevijays/pvekube/discussions).
