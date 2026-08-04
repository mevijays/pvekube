---
layout: default
title: PVEKube | Kubernetes on Proxmox
---

# PVEKube

**Single-binary Kubernetes cluster manager for Proxmox via Cluster API**

Manage production-grade Kubernetes clusters on Proxmox without touching the CLI. PVEKube is a self-contained web application that handles template building, cluster design, live status monitoring, and lifecycle operations — all from your browser.

## Key Features

✅ **Single Binary** — No external dependencies or asset directories  
✅ **Embedded Database** — SQLite with automatic schema migrations & persistent defaults  
✅ **Template Builder** — Automated Packer/Ansible image building via Proxmox API  
✅ **Fast Linked Clones** — Sub-second VM instantiation via copy-on-write linked clones  
✅ **Cluster Designer** — Real-time memory allocation checks, network validation, VIP probes  
✅ **Reliable Provisioning** — Multi-stage readiness gates (API stability & Node/CNI checks before addons)  
✅ **Live Status** — CAPI v1beta2 object monitoring with real-time condition evaluation  
✅ **Lifecycle Ops** — Scale workers/control-plane, teardown with residue scrubbing, rolling upgrades  
✅ **Secure** — Encrypted credential storage, CSRF tokens, session management  

## Quick Start

### Build
```bash
# On macOS
GOOS=linux GOARCH=amd64 go build -o pvekube ./cmd/pvekube

# On Linux
go build -o pvekube ./cmd/pvekube
```

### Run
```bash
mkdir -p ~/pvekube-data
./pvekube --data-dir ~/pvekube-data --listen 0.0.0.0:8080
```

### Access
Open your browser to `http://<vm-ip>:8080/setup`

**[→ Full Getting Started Guide](installation/)**

## Architecture

PVEKube bridges the gap between Proxmox and Kubernetes Cluster API (CAPI):

- **Management Cluster**: KIND (Kubernetes in Docker) on the PVEKube host
- **Infrastructure Provider**: CAPMOX v0.9.0 (Cluster API Provider for Proxmox)
- **Workload Clusters**: Launched as CAPI Cluster objects, provisioned on Proxmox VMs

All orchestration happens declaratively through Kubernetes resources — PVEKube is a UI over kubectl + clusterctl.

**[→ Architecture Deep-Dive](architecture/)**

## What It Does

### Phase 1-5: Foundation
- Web UI with auth, job engine with persistent state, SSE log streaming
- Prerequisites checklist with auto-remediation (kind, clusterctl, kubectl)
- Proxmox REST API client with token auth, live node/bridge/pool discovery
- KIND management cluster bootstrap with pinned CAPI v1.12.10

### Phase 6: Template Builder
- Automated image building via kubernetes-sigs/image-builder v0.1.55
- Real Packer/Ansible execution inside a containerized toolchain
- Support for Ubuntu 22.04, 24.04, Debian 11, 12, Flatcar, Rocky, Alma

### Phase 7: Cluster Designer
- IP network validator (subnet math, range overlap, ping probes)
- clusterctl manifest generator against live CAPMOX provider
- Preview before apply

### Phase 8: Cluster Status
- Live polling of Cluster API objects (Cluster, Machine, KubeadmControlPlane)
- Condition rendering with human-readable status
- Workload kubeconfig download

### Phase 9: Lifecycle
- Scale workers / control-plane (with quorum validation)
- Delete cluster (cascading teardown of VMs)
- Rolling upgrade via new ProxmoxMachineTemplate + KCP patch

**[→ Full Feature List](features/)**

## Requirements

- **Build**: Go 1.23+
- **Proxmox**: 7.x / 8.x / 9.x (tested on 9.1.4)
- **Ubuntu VM**: 22.04 LTS+, 2+ vCPU, 2+ GB RAM, Docker
- **Network**: Direct access to Proxmox API, cluster VMs on management bridge

## Documentation

- **[Getting Started](installation/)** — Build, deploy, and initial setup
- **[Architecture](architecture/)** — Design, component roles, data flow
- **[Features](features/)** — Detailed capability breakdown
- **[API Reference](api-reference/)** — HTTP endpoints and database schema
- **[Troubleshooting](troubleshooting/)** — Common issues and solutions
- **[Contributing](contributing/)** — Development setup and contribution guidelines

- **Upgrade path** not end-to-end tested (needs two templates on real Linux host)
- **Phase 10 enhancements** (self-managing cluster, CAPI backups, diagnostics) pending

## License

Apache 2.0

## Community

- **Issues**: [GitHub Issues](https://github.com/ionos-cloud/golang-proxmox-clusterctl-k8s/issues)
- **Discussions**: [GitHub Discussions](https://github.com/ionos-cloud/golang-proxmox-clusterctl-k8s/discussions)
- **Security**: Report to security@ionos.com

---

**Ready to get started?** [→ Installation Guide](installation/)
