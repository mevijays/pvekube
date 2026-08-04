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
- `permission denied` writing into `/home/imagebuilder/...` → mounted directory ownership issue (already fixed — PVEKube chmods the bind-mounted repo/ISO-cache directories world-writable before every `docker run`, since the container's `imagebuilder` user doesn't match the host UID)

**Solutions**:
- ✅ Verify Proxmox connection details in form
- ✅ Ensure template VMID is correct
- ✅ Check Proxmox token permissions
- ✅ Verify storage pool exists and has space

### Build Fails: "write: broken pipe" Uploading the ISO

**Symptom**: Packer downloads the installer ISO successfully, then fails uploading it to Proxmox:
```
Post "https://<proxmox>:8006/api2/json/nodes/<node>/storage/<pool>/upload": write tcp ...: write: broken pipe
```

**Cause**: Packer's default flow downloads the ISO into the container, then re-uploads it to Proxmox over the REST API — multi-GB uploads over that path are prone to timing out or dropping, especially on slower or less stable links.

**Fix (built in)**: PVEKube pre-stages Ubuntu ISOs directly on Proxmox before every validate/build, sidestepping the client-side upload entirely:
- If the ISO is already present in the selected ISO storage pool, PVEKube skips the download/upload completely and points Packer at it via image-builder's `ISO_FILE` mechanism.
- If it isn't present, PVEKube asks **Proxmox itself** to fetch it (via Proxmox's `download-url` API) — the transfer happens server-side on Proxmox, never crossing the PVEKube-host↔Proxmox link that was dropping the connection.

This only applies to Ubuntu flavors (22.04/24.04/26.04, ± EFI) — Flatcar and Rocky Linux's upstream image-builder configs don't support `ISO_FILE` pre-staging, so those still go through the normal download+upload path.

If you still see this error on an Ubuntu flavor, check the job log for a step titled **"Stage installer ISO on Proxmox"** — it logs exactly which path was taken (already present / downloading server-side / unsupported flavor).

### Build Fails: "one of iso_file, iso_url... must be specified"

**Symptom**: Validate or build fails immediately with:
```
Error: Failed to prepare build
one of iso_file, iso_url, or a combination of cd_files and cd_content must be specified for boot_iso
```

**Cause**: image-builder's Proxmox Packer builder rejects the config when **both** `iso_file` and `iso_url` are set (it doesn't silently prefer one) — and the per-flavor config always sets a real `iso_url` by default. PVEKube handles this automatically by writing a small override var-file (`.pvekube-iso-override.json`, containing `{"iso_url": ""}`) into the image-builder checkout and passing it via the Makefile's `PACKER_VAR_FILES` hook — the only override point applied *after* the flavor's own config, so it actually takes effect. You shouldn't see this error in a current build; if you do, check that `.pvekube-iso-override.json` exists under `~/pvekube-data/image-builder/images/capi/`.

### Build Fails: "unsupported format 'qcow2'"

**Symptom**: VM creation fails with:
```
unable to create VM <vmid> - unsupported format 'qcow2' at .../LvmThinPlugin.pm line ...
```

**Cause**: LVM-thin and ZFS storage pools don't support `qcow2` disk images — only `raw`. PVEKube's discovery scan already detects this per pool (shown in the storage pools table on the Proxmox tab), and passes the correct format through to Packer automatically as of this version. If you still see this, the storage pool you picked for the build may not match what discovery reported — refresh the Proxmox tab and re-check the "Disk format" column for your pool.

### Build Hangs Forever at the Installer's Language-Selection Screen

**Symptom**: The Proxmox console for the build VM shows Ubuntu's interactive installer sitting on its "select your language" screen and never progresses, while PVEKube's job log sits at `Waiting for SSH to become available...` for a very long time (Packer's own timeout for this is 2 hours — it will not fail on its own quickly).

**Cause**: This means the VM booted, but the unattended `autoinstall` never actually kicked in — it fell back to the normal interactive installer. There are three distinct root causes that all produce this exact symptom, so check each in order:

1. **DHCP isn't available on the network the build VM boots into.** The VM needs an IPv4 address to call back to PVEKube's host for the autoinstall config. Confirm from Proxmox:
   ```bash
   qm config <vmid> | grep net0        # note the MAC
   ip neigh | grep <mac>               # look for an IPv4 (not just IPv6 link-local) entry
   ```
   If there's no IPv4 entry, the bridge/VLAN the build uses needs a working DHCP server.

2. **Proxmox's datacenter-wide firewall is silently dropping the callback.** PVEKube now checks this automatically — go to the **Proxmox tab → Permissions** section and look for "Datacenter firewall not silently blocking builds". If it reports the firewall is enabled, either fix the rule set to cover your actual build subnet, or disable it:
   ```bash
   pvesh set /cluster/firewall/options --enable 0
   ```
   This one is nasty because it produces *zero* error anywhere — no 403, no denied-connection message — the packet is just gone.

3. **A GRUB boot-timing race** — Packer's typed boot command (which injects the `autoinstall` kernel parameter) can land after GRUB has already auto-booted into its default entry, especially on Proxmox's noVNC console versus more direct hypervisor keyboard injection. PVEKube already sets a generous `boot_wait` for this, but it's a timing heuristic, not a guarantee — if you rule out #1 and #2 and it's still landing on this screen, retry once; if it recurs consistently, it points at something console-injection-specific to your host rather than a one-off race.

**If you need to stop a hung build**: use the **Cancel** button on the build's progress panel (added in this version) rather than waiting out the 2-hour timeout — it sends a graceful stop to the underlying Docker container so the orphaned build VM gets cleaned up too, instead of leaving both hanging.

### Build Fails: "Permission check failed" (403) on VM Creation

**Symptom**: VM creation fails partway through with a 403, e.g.:
```
Permission check failed (/storage/<pool>, Datastore.AllocateSpace)
Permission check failed (/sdn/zones/<zone>/<bridge>, SDN.Use)
```

**Cause**: The Proxmox API token is missing a specific privilege that a broad-sounding role doesn't actually cover:
- **`PVEVMAdmin` does NOT include `Datastore.AllocateSpace`** — it's VM-lifecycle-only (create/clone/config/power). Disk allocation needs `PVEDatastoreAdmin` too.
- **`SDN.Use`** is required to attach a VM to a network bridge on Proxmox 8.x/9.x installs that use SDN zones (the default on newer installs) — a separate grant on the `/sdn` path.

**Fix**: On the Proxmox tab's **Permissions** section, expand "Template builds or cluster launches failing with a 403? Run the full readiness script" — it prints the exact `pveum acl modify` commands for your configured token. Or run directly on the Proxmox host (as root), substituting your token's user:
```bash
pveum acl modify / -user capmox@pve -role PVEVMAdmin
pveum acl modify / -user capmox@pve -role PVEDatastoreAdmin
pveum acl modify / -user capmox@pve -role PVEAuditor
pveum acl modify /sdn -user capmox@pve -role PVEAdmin
```

**If that doesn't fix it**: check whether the token has privilege separation (privsep) enabled — grants on the *user* don't reach the token when privsep is on:
```bash
pveum user token list capmox@pve   # look at the "privsep" column
```
If `privsep` shows `1`, repeat the four commands above with `-token 'capmox@pve!capi'` instead of `-user capmox@pve`.

See **[Installation → Proxmox API Token Setup](installation/#proxmox-api-token-setup)** for the complete, correct set of grants to use from the start.

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

## Performance & Provisioning Troubleshooting

### Slow VM Power-On After Clone

**Symptom**: VMs appear in Proxmox UI quickly but remain in "Stopped" state for several minutes before powering on.

**Cause**: By default, CAPMOX specifies Full Clones (`full: true`), which requires Proxmox to copy 20GB+ disk images across storage. CAPMOX waits for the Proxmox UPID task to finish before proceeding to `qm set` and `qm start`.

**Solution**:
- ✅ Ensure PVEKube is updated to the version supporting **Linked Clones** (`full: false`).
- ✅ Linked Clones create tiny copy-on-write delta disks referencing the base template in sub-second time.
- ✅ PVEKube automatically transforms `full: true` to `full: false` during manifest generation.

### Webhook Error: `Must set full=true when specifying format`

**Symptom**: Manifest apply fails with `ProxmoxMachineTemplate "my-cluster" is invalid: spec.template.spec: Invalid value: Must set full=true when specifying format`.

**Cause**: CAPMOX validating webhook strictly forbids `format: qcow2` or `format: raw` when `full: false` (Linked Clones) is set, because linked clones inherit disk format from the base template.

**Solution**:
- ✅ PVEKube automatically strips `format: qcow2` from manifests when generating templates with `full: false`.
- ✅ If applying custom manifests manually via `kubectl`, remove `format: "qcow2"` from `ProxmoxMachineTemplate.spec.template.spec`.

### Addon Installation Fails: `dial tcp <ip>:6443: connect: no route to host`

**Symptom**: Cluster creation fails at Step 4/5 while installing `metrics-server`, `Istio`, or `MetalLB`.

**Cause**: Manifest addons were previously applied immediately when the API server responded to a single health check ping, but before worker VMs booted or CNI pods (Calico/Cilium) were ready.

**Solution**:
- ✅ PVEKube includes **Multi-Stage Readiness Gates**:
  1. API Server stability check requiring 5 consecutive successful pings (5s apart).
  2. `WaitForNodesReadyStep` which polls `kubectl get nodes` until all expected nodes report `Ready`.
- ✅ Verify worker VMs have network connectivity and cloud-init has finished executing.

### Header Displays "Control plane not ready" Despite Green Conditions

**Symptom**: Cluster detail header displays `Control plane not ready · Infrastructure not ready`, but condition cards show `ControlPlaneAvailable` and `InfrastructureReady` as green checkmarks.

**Cause**: Cluster API `v1beta2` deprecated top-level boolean fields `status.controlPlaneReady` and `status.infrastructureReady` in favor of `status.conditions`.

**Solution**:
- ✅ PVEKube parses `status.conditions` (`ControlPlaneAvailable` and `InfrastructureReady`) for full CAPI `v1beta2` compliance.
- ✅ Refresh browser page to update header display.

### Stale "Free to Allocate" Memory Values in Cluster Form

**Symptom**: After resizing or shutting down VMs on Proxmox, the cluster creation form still displays old memory availability (e.g., "32.0 GiB").

**Cause**: Datacenter discovery metrics were previously read from SQLite cache without expiration.

**Solution**:
- ✅ PVEKube bypasses cached discovery and executes a live Proxmox API call whenever the "Launch a New Cluster" panel is loaded.
- ✅ Simply navigate back to or refresh the "Clusters" tab to fetch real-time memory metrics.

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
