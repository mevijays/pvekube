---
layout: default
title: UI Gallery & Screenshots
permalink: /screenshots/
---

# UI Gallery & Screenshots

Explore the visual interface of PVEKube. Every screen is designed for simplicity, real-time feedback, and ease of operation.

---

## 1. Kubernetes Clusters Dashboard

The main control panel listing all provisioned Kubernetes clusters on your Proxmox infrastructure. 

![Kubernetes Clusters Dashboard]({{ '/assets/clusters-dashboard.png' | relative_url }})

**Key Features & Details:**
- **Live Cluster Status**: Displays current phase (`Provisioned`, `Provisioning`, `Deleting`).
- **One-Click Actions**: Access cluster details, trigger scaling operations, or launch new cluster creation workflows.
- **Resource Inspection Toggle**: Click the **Resources** button to instantly navigate to telemetry metrics for any cluster.

---

## 2. Cluster Details & Live CAPI Conditions

Detailed status view for an individual Kubernetes cluster.

![Cluster Details & Conditions]({{ '/assets/cluster-detail-conditions.png' | relative_url }})

**Key Features & Details:**
- **Status Header**: Displays overall readiness (`Control plane ready`, `Infrastructure ready`).
- **Kubeconfig Download**: One-click **Download kubeconfig** button to connect via `kubectl` on your local terminal.
- **Lifecycle Operations**: Scale worker nodes or control plane replicas up/down dynamically with quorum safety validation.
- **Real-Time Conditions List**: Displays Cluster API v1beta2 conditions (`Available`, `RemoteConnectionProbe`, `InfrastructureReady`, `ControlPlaneAvailable`, `WorkersAvailable`) with instant visual indicators.

---

## 3. Workload Resources & Telemetry Dashboard

Live metrics and telemetry for workloads running inside a selected Kubernetes cluster.

![Workload Resources Telemetry]({{ '/assets/cluster-resources-telemetry.png' | relative_url }})

**Key Features & Details:**
- **Resource Counter Cards**: Real-time count of Nodes, Namespaces, Pods, Deployments, StatefulSets, DaemonSets, ConfigMaps, Secrets, PVCs, PVs, and StorageClasses.
- **Capacity vs Allocation Charts**: Visual bar charts tracking CPU Cores and Memory (GiB) across `Capacity`, `Allocatable`, `Requests`, `Limits`, and `Used`.
- **Live Refresh**: Query metrics on demand or refresh automatically.

---

## 4. Workload Objects & Pods Table

Filtered tabular view of running workload objects inside the workload cluster.

![Workload Pods Table]({{ '/assets/cluster-pods-table.png' | relative_url }})

**Key Features & Details:**
- **Namespace Filtering**: Filter resources by specific Kubernetes namespaces (e.g. `default`, `kube-system`, `istio-system`).
- **Resource Search**: Instant text filtering by object name or node.
- **Detailed Object Metrics**: View Pod status, target Proxmox node, CPU requests/limits, Memory requests/limits, CPU/memory usage, and pod age.

---

## 5. Prerequisites Verification & Self-Healing

Self-contained system diagnostic screen ensuring your host environment is ready.

![Prerequisites Verification]({{ '/assets/prerequisites-verification.png' | relative_url }})

**Key Features & Details:**
- **Automated Checkers**: Verifies OS support, internet connectivity (`registry.k8s.io`, `github.com`), free disk space (40 GB+), Docker daemon status, and local binaries (`kind`, `clusterctl`, `kubectl`).
- **Auto-Remediation**: Downloads missing binaries and verifies SHA-256 checksums automatically.
- **Re-check All**: Instant re-validation trigger.

---

## 6. Proxmox Connection & API Token Audit

Connection setup and API permission verification for Proxmox VE.

![Proxmox Connection & Permissions]({{ '/assets/proxmox-connection-permissions.png' | relative_url }})

**Key Features & Details:**
- **Active Connection Card**: Shows target Proxmox VE IP (`172.16.1.101`), PVE version (e.g. `9.1.4`), and active API token ID.
- **Permissions Audit**: Verifies fine-grained PVE privileges (`Sys.Audit`, `Datastore.Audit`, `VM.Allocate`, `PVEVMAdmin`, `Datastore.AllocateSpace`, `SDN.Use`).
- **Credential Encryption**: Token secrets are stored encrypted at rest using AES-GCM.

---

## 7. VM Template Builder (Packer & Ansible)

Automated image builder for creating Kubernetes-ready OS templates on Proxmox.

![VM Template Builder]({{ '/assets/vm-templates-builder.png' | relative_url }})

**Key Features & Details:**
- **OS Flavors**: Build Ubuntu 22.04, 24.04, Debian 11/12, Rocky, Alma, or Flatcar templates.
- **Kubernetes Version Selection**: Specify exact K8s versions (e.g., `v1.31.4`).
- **Storage & Bridge Mapping**: Select source node, ISO storage pool, VM image storage pool, and network bridge.
- **Built Templates Table**: Tracks all generated templates with OS, K8s version, Proxmox node, VMID (e.g., `115`), and build timestamp.

---

## 8. Dark Mode Theme

PVEKube includes full native dark mode support tailored for low-light operator environments.

### Clusters Dashboard in Dark Mode

![Dark Mode Clusters Dashboard]({{ '/assets/dark-mode-clusters.png' | relative_url }})

### Prerequisites Checklist in Dark Mode

![Dark Mode Prerequisites]({{ '/assets/dark-mode-prerequisites.png' | relative_url }})

---

**[← Back to Overview]({{ '/' | relative_url }})** | **[Next: Getting Started]({{ '/installation/' | relative_url }})**
