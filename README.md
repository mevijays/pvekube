# PVEKube

[![Go Reference](https://img.shields.io/badge/go-1.23%2B-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![GitHub Release](https://img.shields.io/github/v/release/mevijays/pvekube?color=indigo)](https://github.com/mevijays/pvekube/releases)
[![Documentation](https://img.shields.io/badge/docs-GitHub%20Pages-indigo)](https://mevijays.github.io/pvekube/)

**Single-binary Kubernetes cluster manager for Proxmox VE powered by Cluster API (CAPI).**

PVEKube is a self-contained web application that handles template building, cluster design, live status monitoring, and lifecycle operations for Kubernetes on Proxmox VE — all from your browser without touching the CLI.

---

![PVEKube Clusters Dashboard](https://raw.githubusercontent.com/mevijays/pvekube/main/docs/assets/clusters-dashboard.png)

---

## ✨ Features

- ⚡ **Single Binary Distribution**: Zero external runtime dependencies; embedded SQLite database with automatic migrations and embedded UI templates.
- 🚀 **Fast Sub-Second Linked Clones**: Provisions workload nodes instantly using copy-on-write Linked Clones (`full: false`) to avoid slow disk copying.
- 🛠️ **Automated VM Template Builder**: Semi-automated OS image builder (Packer + Ansible inside Docker) supporting Ubuntu, Debian, Flatcar, Rocky, and Alma.
- 🧠 **Real-Time Hardware Discovery**: Live, un-cached memory accounting for Proxmox nodes so "Free to allocate" reflects guest resizings instantly.
- 🔒 **Multi-Stage Provisioning Readiness**: Enforces 5-consecutive-success API server stability checks and node readiness polling (`kubectl get nodes`) before installing post-provisioning manifests (`metrics-server`, `Istio`, `MetalLB`).
- 📊 **Telemetry & Workload Monitoring**: Live CPU Cores and Memory capacity vs. allocation charts, workload pod counts, and resource tables.
- 🌙 **Native Dark Mode**: Sleek dark and light UI themes tailored for operator environments.

---

## 🚀 Quick Start

### 1. Download Prebuilt Binary

Download the latest release binary for your OS and architecture from [GitHub Releases](https://github.com/mevijays/pvekube/releases):

```bash
# Example for Linux x86_64
curl -LO https://github.com/mevijays/pvekube/releases/latest/download/pvekube-linux-amd64
chmod +x pvekube-linux-amd64
mv pvekube-linux-amd64 pvekube
```

### 2. Or Build From Source

```bash
git clone https://github.com/mevijays/pvekube.git
cd pvekube

# Build for Linux x86_64
GOOS=linux GOARCH=amd64 go build -o pvekube ./cmd/pvekube
```

### 3. Run PVEKube

```bash
mkdir -p ~/pvekube-data
./pvekube --data-dir ~/pvekube-data --listen 0.0.0.0:8080
```

Open your browser and navigate to `http://<your-ip>:8080/setup` to complete initial administrator setup.

---

## 🏛️ Architecture

PVEKube bridges Proxmox VE and Cluster API (CAPI):

```
┌─────────────────────────────────────────────────────────────┐
│                    PVEKube Host Machine                     │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  PVEKube Web Server (Go binary + SQLite DB)           │  │
│  └───────────────────────────────────────────────────────┘  │
│                             │                               │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  KIND Management Cluster (Docker)                     │  │
│  │  ├─ CAPI Core Controller (v1.12.10)                   │  │
│  │  └─ CAPMOX Infrastructure Provider (v0.9.0)           │  │
│  └───────────────────────────────────────────────────────┘  │
└──────────────────────────────┬──────────────────────────────┘
                               │ API Calls
┌──────────────────────────────▼──────────────────────────────┐
│                    Proxmox VE Hypervisor                    │
│  ├─ Linked Clones from Node Template VM                      │
│  └─ Workload VMs (Control Plane & Workers)                   │
└─────────────────────────────────────────────────────────────┘
```

---

## 📖 Documentation

Full documentation, architecture deep-dives, API references, and troubleshooting guides are available on our documentation site:

👉 **[https://mevijays.github.io/pvekube/](https://mevijays.github.io/pvekube/)**

- [Getting Started Guide](https://mevijays.github.io/pvekube/installation/)
- [Architecture & Sequence Flows](https://mevijays.github.io/pvekube/architecture/)
- [Feature Details](https://mevijays.github.io/pvekube/features/)
- [UI Screenshot Gallery](https://mevijays.github.io/pvekube/screenshots/)
- [API & Database Reference](https://mevijays.github.io/pvekube/api-reference/)
- [Troubleshooting](https://mevijays.github.io/pvekube/troubleshooting/)

---

## 🤝 Contributing

Contributions are welcome! Please check out our [Contributing Guide](https://mevijays.github.io/pvekube/contributing/) before submitting issues or pull requests.

## 📄 License

PVEKube is released under the [Apache 2.0 License](LICENSE).
