---
layout: default
title: Troubleshooting
---

# Troubleshooting

## Connection Issues

### Proxmox Connection Failed

**Symptom**: "Failed to connect to Proxmox" error when testing connection.

**Check**:
```bash
# Verify network connectivity
ping <proxmox-ip>
curl -k https://<proxmox-ip>:8006/api2/json/version
```

**Solutions**:
- ✅ Verify IP/hostname is correct
- ✅ Check token ID format: should be `user@realm!token-name`
- ✅ Verify token has sufficient permissions (Administrateur or VM.Allocate + VM.Console)
- ✅ If self-signed HTTPS, check "Insecure TLS" box
- ✅ Ensure PVEKube host can reach Proxmox server on port 8006
- ✅ Check firewall rules (if applicable)

### Invalid Token or Permission Denied

**Symptom**: "Permission denied" or "401 Unauthorized" in PVEKube logs.

**Check**:
1. Go to Proxmox Web UI → **Datacenter → Permissions → API Tokens**
2. Find your token; verify it exists and hasn't expired
3. Verify token privilege level (should include VM operations)

**Solutions**:
- ✅ Regenerate token (if expired or uncertain of secret)
- ✅ Increase token privileges (set to Administrator if uncertain)
- ✅ Ensure token secret is copied correctly (long alphanumeric string)

## Prerequisites Issues

### Binary Download Fails

**Symptom**: Download stuck or "Checksum mismatch" error.

**Check**:
```bash
# Verify network connectivity to GitHub
curl -I https://github.com/kubernetes-sigs/kind/releases/download/v0.20.0/kind-linux-amd64
```

**Solutions**:
- ✅ Ensure Ubuntu VM has internet access
- ✅ Try retry button in PVEKube UI
- ✅ Check disk space: `df -h`
- ✅ If checksum fails, delete the file and retry
- ✅ Check firewall/proxy (if applicable)

### Docker Daemon Not Running

**Symptom**: "Cannot connect to Docker daemon" error.

**Solutions**:
```bash
# Start Docker
sudo systemctl start docker

# Add user to docker group (requires logout/login)
sudo usermod -aG docker $USER
newgrp docker

# Test docker
docker ps
```

### KIND Cluster Creation Fails

**Symptom**: "kind: command not found" or cluster creation timeout.

**Check**:
```bash
which kind
kind version
```

**Solutions**:
- ✅ Ensure kind binary downloaded successfully
- ✅ Verify kind is in PATH: `export PATH=/usr/local/bin:$PATH`
- ✅ Check Docker is running: `docker ps`
- ✅ Check disk space for KIND cluster image
- ✅ Try manual creation: `kind create cluster --name pvekube`

## Template Building Issues

### Build Job Never Starts

**Symptom**: Job shows "pending" indefinitely.

**Check**:
```bash
# Verify Docker is running
docker ps

# Check for stuck containers
docker ps -a

# Check disk space
df -h
```

**Solutions**:
- ✅ Restart Docker: `sudo systemctl restart docker`
- ✅ Ensure sufficient disk space (5+ GB)
- ✅ Clean up old containers: `docker system prune -a`
- ✅ Check PVEKube logs: `journalctl -u pvekube -f`

### Packer Validation Fails

**Symptom**: "packer validate" fails in build logs.

**Check** the live logs in PVEKube for:
- `invalid option` → Docker ENTRYPOINT issue (already fixed in this version)
- `PROXMOX_URL missing scheme` → Ensure URL is normalized
- `403 Forbidden` → Proxmox permissions issue

**Solutions**:
- ✅ Verify Proxmox connection details in form
- ✅ Ensure template VMID is correct
- ✅ Check Proxmox token permissions
- ✅ Verify storage pool exists and has space

### Template Build Timeout

**Symptom**: Build runs for 20+ minutes without completing.

**Solutions**:
- ✅ Cancel job; try with simpler OS (e.g., Ubuntu 22.04 instead of Flatcar)
- ✅ Check Proxmox host CPU/RAM (provisioning can be CPU-bound)
- ✅ Verify network speed (image-builder downloads packages)
- ✅ Check Proxmox logs for VM errors: `/var/log/messages`

## Cluster Launch Issues

### Network Validation Fails

**Symptom**: "Check IP Plan" shows errors in red.

**Common Issues**:
- **VIP already in use**: Another device responds to ping on VIP address
  - **Fix**: Choose a different unused IP
- **Node range too small**: Not enough IPs for all nodes
  - **Fix**: Increase the range (e.g., 10.10.10.50-10.10.10.100 instead of .50-.60)
- **VIP in node range**: Virtual IP overlaps with node pool
  - **Fix**: Adjust ranges to not overlap

**Check**:
```bash
# From PVEKube host, test gateway and VIP
ping <gateway>
ping <vip>

# Check if IPs already in use
arp-scan --localnet | grep <gateway-or-vip>
```

### Cluster Stuck in "Provisioning"

**Symptom**: Cluster doesn't transition to "Provisioned" after 15+ minutes.

**Check** the **Cluster detail page → Conditions**:
- **InfrastructureReady = False**: CAPMOX can't reach Proxmox
- **ControlPlaneInitialized = False**: kubeadm bootstrap still running
- **RemoteConnectionProbe = ProbeFailed**: Cluster API server unreachable

**Specific Fixes**:

#### InfrastructureReady = False
```bash
# Credentials issue; check management cluster Secret
kubectl --kubeconfig ~/pvekube-data/kubeconfigs/management.yaml \
  get secret capmox-manager-credentials -n capmox-system -o yaml

# Verify values are base64-encoded correctly
kubectl ... get secret ... -o jsonpath='{.data.url}' | base64 -d
```

**Solutions**:
- ✅ Verify Proxmox credentials are correct
- ✅ Check template VMID exists and is accessible
- ✅ Verify storage pool has space
- ✅ Check Proxmox VM creation logs (Proxmox Web UI → Logs)

#### ControlPlaneInitialized = False
- Kubeadm is running; this is normal for the first 5-10 minutes
- Wait longer before troubleshooting

#### RemoteConnectionProbe = ProbeFailed
- Control plane API server not yet reachable
- Can be due to slow network provisioning or incorrect VIP

**Check**:
```bash
# Try reaching cluster API from PVEKube host
curl -k https://<vip>:6443/healthz

# Check if nodes are running
# Go to Proxmox Web UI → Cluster → Nodes
```

**Solutions**:
- ✅ Wait for nodes to boot (watch Proxmox console)
- ✅ Verify VIP is on correct network/reachable
- ✅ Check network routes and gateway accessibility
- ✅ Increase cluster sizes to ensure quorum

### "No such file or directory" Errors in Logs

**Symptom**: Jobs fail with "terraform not found" or similar.

**Check**:
```bash
# Verify binaries are downloaded
ls ~/pvekube-data/bin/
```

**Solution**:
- ✅ Run Prerequisites verification again to download missing binaries
- ✅ Manually download: `GOOS=linux GOARCH=amd64 go build -o pvekube ./cmd/pvekube`

## Cluster Access Issues

### Kubeconfig Download Not Available

**Symptom**: "Download kubeconfig" button is greyed out.

**Check**:
- Cluster status should show "Provisioned" (not "Provisioning")
- Conditions should include "Available = True"

**Solutions**:
- ✅ Wait for cluster to reach "Provisioned" status
- ✅ If waiting > 30 min, check Conditions for errors (see "Cluster Stuck" above)

### kubectl Fails: "Unable to Connect to Server"

**Symptom**: `kubectl get nodes` fails with "no route to host" or "connection refused".

**Check**:
```bash
# Verify kubeconfig file
cat ~/Downloads/my-k8s-kubeconfig.yaml | head -5

# Test connectivity to VIP
ping <vip>
curl -k https://<vip>:6443/healthz
```

**Solutions**:
- ✅ Verify VIP is on same network as your machine
- ✅ Check firewall rules (6443/tcp must be accessible)
- ✅ Ensure cluster is still in "Provisioned" state (may have been deleted)
- ✅ Re-download kubeconfig (might be stale)

### Authentication Error: "invalid credentials"

**Symptom**: `kubectl get nodes` fails with "authentication failure".

**Solutions**:
- ✅ Re-download kubeconfig
- ✅ Verify cluster hasn't been deleted/recreated
- ✅ Check kubeconfig file isn't corrupted: `cat kubeconfig.yaml | wc -l` (should be ~100+ lines)

## Cluster Operations Issues

### Scale Operation Hangs

**Symptom**: Scale workers/control-plane job never completes.

**Check**:
```bash
# Verify job is running
sqlite3 ~/pvekube-data/pvekube.db "SELECT status FROM jobs WHERE kind='cluster.scale_workers' ORDER BY id DESC LIMIT 1;"

# Check management cluster
kubectl get machinedeployment
```

**Solutions**:
- ✅ Wait longer (scaling can take 5-10 minutes)
- ✅ Check Proxmox for VM provisioning errors
- ✅ Verify network connectivity (nodes might be slow to boot)
- ✅ Cancel job; check Proxmox for stuck VMs; retry

### Delete Fails

**Symptom**: Cluster deletion job fails or VMs not removed.

**Check**:
```bash
# Verify cluster object was deleted
kubectl get cluster -A
```

**Solutions**:
- ✅ Manually clean up VMs in Proxmox if deletion stuck
- ✅ Check CAPMOX controller logs for errors:
  ```bash
  kubectl logs -n capmox-system -l control-plane=controller-manager --tail=50
  ```
- ✅ Manually delete finalizers if stuck:
  ```bash
  kubectl patch cluster <name> -p '{"metadata":{"finalizers":[]}}' --type=merge
  ```

## Upgrade Issues

### Upgrade Job Fails: "Template not found"

**Symptom**: Upgrade fails because new K8s version template doesn't exist.

**Solutions**:
- ✅ Build the template with the new K8s version first
- ✅ Run template builder with desired K8s version
- ✅ Retry upgrade after template is ready

### Upgrade Hangs During Node Replacement

**Symptom**: Nodes stuck in "Pending" or "NotReady" during upgrade.

**Check**:
```bash
# SSH to control plane node
ssh core@<node-ip>  # or ubuntu@<node-ip>

# Check kubelet status
journalctl -u kubelet | tail -50
```

**Solutions**:
- ✅ Wait longer (rolling replacement takes 10-30 minutes)
- ✅ If node stuck: manually delete the old VM and let CAPI recreate it
- ✅ Check Proxmox logs for VM creation errors

## General Debugging

### Check PVEKube Logs

```bash
# If running via systemd
journalctl -u pvekube -f

# If running in foreground
# (output goes to terminal)
```

### Check SQLite Database

```bash
# View recent clusters
sqlite3 ~/pvekube-data/pvekube.db "SELECT name, status, created_at FROM clusters ORDER BY id DESC LIMIT 10;"

# View recent jobs
sqlite3 ~/pvekube-data/pvekube.db "SELECT kind, status, error FROM jobs ORDER BY id DESC LIMIT 10;"
```

### Check Management Cluster State

```bash
export KUBECONFIG=~/pvekube-data/kubeconfigs/management.yaml

# View Cluster API objects
kubectl get cluster,machine,machinedeployment,proxmoxcluster -A

# Check controller status
kubectl get deployment -n capmox-system
kubectl get pods -n capmox-system

# View detailed conditions
kubectl describe cluster <cluster-name>
```

### Check Proxmox Server

1. **Proxmox Web UI** → **Cluster**:
   - **Nodes**: Check node health
   - **VMs**: Check running VMs
   - **Logs**: Check Proxmox system logs
   - **Storage**: Verify pools have space

2. **SSH to Proxmox host** (if needed):
   ```bash
   tail -f /var/log/messages  # system log
   pvesh get /clusters        # cluster API status
   ```

## Getting Help

If you're stuck:
1. **Check logs** (PVEKube + management cluster + Proxmox)
2. **Check network** (ping, curl, route)
3. **Check state** (cluster status, node status, VM status)
4. **Open an issue**: [GitHub Issues](https://github.com/ionos-cloud/golang-proxmox-clusterctl-k8s/issues)
   - Include: PVEKube version, cluster design (control plane count, worker count), error logs
   - Redact: Proxmox IPs, token secrets

---

**[← Back to Docs](/)** | **[Contributing](contributing/)**
