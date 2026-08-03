# PVEKube — Web-driven Kubernetes on Proxmox

**Goal:** a single Go binary. Non-technical user runs it, opens a browser, and gets from
"bare Proxmox server" to "running Kubernetes cluster" without ever touching a CLI.

Target Proxmox: `root@172.16.1.101`

---

## 1. Core design decision: orchestrate, don't reimplement

The temptation is to reimplement Packer/clusterctl logic in Go against the Proxmox API.
That is a trap — the CAPI + image-builder ecosystem changes fast, and the hard-won
correctness (cloud-init contracts, containerd config, kubelet flags, CAPI CRD versions)
lives upstream.

**PVEKube is a supervisor, not a replacement.** It drives:

| Concern | Upstream tool | How we drive it |
|---|---|---|
| Build VM template | `kubernetes-sigs/image-builder` (Packer + Ansible) | official container image |
| Bootstrap cluster | `kind` | container / binary |
| Cluster lifecycle | `clusterctl` + CAPMOX | binary + client-go |
| Proxmox introspection | PVE REST API | native Go client |

Our value-add is everything upstream deliberately leaves out: **preflight validation,
credential handling, progress/log streaming, state persistence, error translation, and
a UI a non-technical person can follow.**

### Key architectural consequence
The user's host needs **only Docker**. image-builder, kind, and clusterctl all run
containerized or as fetched static binaries into our own data dir. We never
`apt install` onto the host beyond Docker itself.

---

## 2. Version matrix (pinned — this is load-bearing)

Researched 2026-08-02. **Do not "just use latest".**

| Component | Pin | Why |
|---|---|---|
| Cluster API core | **v1.12.10** | CAPMOX v0.9 supports CAPI v1.11/v1.12 **only**. Core latest is v1.13.4 — using it breaks. |
| CAPMOX (infra provider) | **v0.9.0** | v1alpha2 API. clusterctl's built-in entry points at `releases/latest`, so we pin explicitly. |
| IPAM in-cluster | **v1.1.0** | CAPMOX has no DHCP path for nodes; IPAM is mandatory. |
| kind | **v0.32.0** | bootstrap cluster |
| image-builder container | **v0.1.55** | `registry.k8s.io/scl-image-builder/cluster-node-image-builder-amd64` |
| Packer / proxmox plugin | 1.16.0 / 1.2.4 | supplied inside the image-builder container |

Pins live in a single `internal/versions/versions.go` with a compatibility check that
refuses to proceed on a known-bad combination and explains why in plain English.

---

## 3. Hard constraints to surface in the UI (not bury in docs)

These cause ~90% of real-world failures on this path. Each becomes a **preflight check
with a plain-language explanation**, not a stack trace at minute 28 of a build.

1. **App host must be Linux.** image-builder's Proxmox build needs `docker run --net=host`
   (Linux-only) because Packer serves the OS autoinstall config over HTTP *and the
   Proxmox VM must reach back to it*. On macOS/Windows this silently hangs.
   → Check OS; if not Linux, block template building with a clear message (cluster
   creation still works if a template already exists).
2. **Proxmox build VM needs DHCP** on the chosen bridge. image-builder does not support
   static IP at build time. → We probe and warn.
3. **Proxmox must be able to reach the app host** on the Packer HTTP port. → Active
   reachability probe, not an assumption.
4. **Workload nodes need static IPs** (via IPAM), and `CONTROL_PLANE_ENDPOINT_IP` must be
   on the same subnet as the control plane machines and **not** in the node IP range.
   → We validate the plan arithmetically *and* ping-sweep for live conflicts.
5. **Proxmox API token must be `-privsep 0`** with `PVEVMAdmin` on `/`. → We verify by
   calling the API and checking effective permissions, not by trusting the user.
6. **Storage pool must accept the right content types** (`iso` for ISO, `images` for
   disks) and ZFS/LVM pools need `raw` not `qcow2`. → Detected from the API and the
   disk format is auto-selected.

---

## 4. User journey (the screens)

```
First run
  └─> [0] Setup wizard: admin password + encryption passphrase
        └─> [1] Prerequisites checklist  ◄── the "gate" screen
              └─> [2] Proxmox connection & discovery
                    └─> [3] Template builder
                          └─> [4] Cluster designer
                                └─> [5] Cluster detail / lifecycle
```

### [0] First-run setup
Web UI holding Proxmox root-equivalent credentials must not be unauthenticated.
- Set admin password (argon2id), set/derive data-encryption key.
- Session cookies, CSRF, bind to `127.0.0.1` by default with an explicit
  `--listen 0.0.0.0:8080` opt-in that warns.

### [1] Prerequisites checklist — the centrepiece
A live table of checks. Each row: status icon, plain-English name, "what this means",
and — critically — a **`Fix it` button** where auto-remediation is safe.

| Check | Auto-fix |
|---|---|
| OS is Linux, arch amd64/arm64 | ✗ (informational) |
| Internet reachability (registry.k8s.io, github.com) | ✗ |
| Docker installed | ✓ install via get.docker.com (with confirmation) |
| Docker daemon running & user in `docker` group | ✓ |
| Disk space ≥ 40 GB free | ✗ |
| `kind` binary present | ✓ download pinned, verify SHA256 |
| `clusterctl` binary present | ✓ download pinned, verify SHA256 |
| `kubectl` binary present | ✓ download pinned, verify SHA256 |
| image-builder image pulled | ✓ `docker pull` with progress |
| KIND bootstrap cluster running | ✓ create from generated config |
| CAPI providers initialised in KIND | ✓ `clusterctl init` |
| Proxmox reachable + creds valid | ✗ (form) |
| Proxmox permissions sufficient | ✗ (shows exact `pveum` commands to copy) |
| Proxmox → app-host reachability | ✗ (diagnostic output) |

Everything is **re-runnable and idempotent**. State stored in SQLite so a restart resumes.

> **Enhancement — "Fix everything" button.** Queues all safe remediations as one job with
> a single progress stream. This is the difference between a checklist and a product.

### [2] Proxmox connection & discovery
Form: URL, token ID, secret, TLS-skip toggle. On save we **discover and cache**:
nodes, storage pools (+ content types + type→disk format), network bridges, VLAN tags,
ISO images present, next free VMID, cluster quorum status, per-node CPU/RAM/disk headroom.

Everything downstream becomes a **dropdown populated from real data** — never a free-text
field the user can typo.

### [3] Template builder
- Pick OS (Ubuntu 22.04 / 24.04 / 24.04-EFI / 26.04 / 26.04-EFI / Rocky 9 / Flatcar) and
  Kubernetes version.
- Pick node, storage pool, bridge, VLAN — from discovery.
- **Dry-run first:** run `make validate-proxmox-<os>` (seconds) before committing to a
  ~30 minute build. Catches config errors immediately. This target exists upstream and is
  badly underused.
- Then `make build-proxmox-<os>` with live streamed logs, a step timeline
  (download ISO → boot → autoinstall → ansible → k8s components → convert to template),
  and a cancel button.
- On success, record template in a **Template Catalog**: VMID, OS, k8s version, node,
  build duration, log archive.

### [4] Cluster designer
Guided form → generates and applies the CAPI manifests.
- Name, Kubernetes version (constrained to what the selected template actually contains).
- Control plane count (1 or 3 — we explain why not 2), worker count.
- CPU / RAM / disk per role, with a live "will this fit?" calculation against discovered
  node headroom.
- CNI: Cilium / Calico / Cilium+MetalLB (maps to CAPMOX flavors).
- Network: bridge, VLAN, gateway, prefix, DNS, node IP range, control-plane VIP.
  - **IP Plan validator**: range size ≥ node count, VIP outside range, subnet math checks,
    live ping-sweep for occupied addresses.
- **Preview screen**: rendered YAML shown before apply, downloadable. Transparency builds
  trust and makes support tractable.
- Apply → watch CAPI resources via client-go, render a live machine-by-machine timeline.

### [5] Cluster detail / lifecycle
- Live topology: Cluster → KCP / MachineDeployment → Machines with phase + IP + node.
- **Download kubeconfig** button (the single most-wanted thing).
- Scale workers, upgrade Kubernetes version (rolling), delete with typed confirmation.
- Conditions surfaced as human sentences, not raw CAPI condition types.

---

## 5. Feature enhancements I recommend adding

Beyond the baseline brief:

1. **Pivot the management cluster** (`clusterctl move`). A KIND cluster on a laptop is a
   single point of failure — if it dies, CAPI can no longer reconcile the workload
   clusters. Offer a one-click "make this cluster self-managing" that moves CAPI state
   into the first created cluster. *Strongly recommended; this is the difference between
   a demo and something you can leave running.*
2. **Backup / restore of CAPI state** — `clusterctl move --to-directory` on a schedule
   into the app's data dir, plus restore. Cheap insurance.
3. **Doctor page** — one-click diagnostic bundle (versions, prereq results, recent job
   logs with secrets redacted) as a downloadable zip for support.
4. **Full audit + command log** — every shell command, args, exit code, and output stored.
   Nothing happens invisibly.
5. **Secrets sealed at rest** — AES-GCM, key from the first-run passphrase. Never log
   token secrets; a redaction layer scrubs them from all captured output.
6. **Resumable jobs** — a 30-minute Packer build must survive a browser refresh, and the
   UI must reattach to the running stream (that's why logs go to disk, not memory).
7. **Dark mode + mobile-responsive** — people watch 30-minute builds from their phone.
8. **Guided error translation** — a rules table mapping known failure signatures
   ("501 when uploading to storage", "no route to host", "VMID already exists") to a
   plain-English cause and fix. This is what actually makes it non-technical-user-proof.
9. **Cost/capacity guard** — refuse (with override) to design a cluster that exceeds
   discovered node capacity.
10. **Air-gap / mirror option** (later) — configurable registry mirror for the whole stack.

---

## 6. Technical stack

- **Go 1.26**, single binary, `embed.FS` for all UI assets.
- **HTTP:** stdlib `net/http` + `http.ServeMux` (Go 1.22+ routing) — minimal deps.
- **DB:** SQLite via `modernc.org/sqlite` (**pure Go, no CGO**) — keeps the binary
  trivially cross-compilable and truly single-file. Schema migrations embedded.
- **UI:** server-rendered `html/template` + **Tailwind CDN** + **Font Awesome 4.7** (as
  requested) + **HTMX** for partial updates + **SSE** for live log/progress streaming.
  No npm, no build step — consistent with the single-binary goal.
- **K8s:** `client-go` + CAPI/CAPMOX types for watching cluster state.
- **Proxmox:** thin native REST client (we only need read/introspection + verification).

### Package layout
```
cmd/pvekube/main.go
internal/
  versions/     pinned versions + compatibility matrix
  config/       flags, data dir, listen addr
  store/        sqlite, migrations, models, repositories
  secrets/      AES-GCM sealing, redaction registry
  runner/       context-aware command exec, streaming, log persistence
  jobs/         Job/Step engine: queue, cancel, resume, SSE fanout
  prereq/       check registry + remediations
  proxmox/      PVE API client + discovery
  imagebuilder/ container orchestration, env-file generation, dry-run
  bootstrap/    kind + clusterctl init
  capi/         manifest generation, apply, watch, lifecycle ops
  ipplan/       subnet math, conflict detection, ping sweep
  diag/         error signature → human explanation
  server/       routes, auth, CSRF, SSE, handlers
  ui/           templates/ + static/ (embedded)
```

### The job engine (the hardest part — build it first)
Every long operation is a `Job` with ordered `Step`s.
- Persisted to SQLite; logs appended to files under the data dir.
- SSE endpoint replays from offset then tails — refresh-safe.
- Cancellable via context; cleanup handlers (e.g. destroy the orphaned Packer VM).
- Idempotent restart: on boot, any `running` job is marked `interrupted` with a resume option.

---

## 7. Execution phases

| Phase | Deliverable | Verification |
|---|---|---|
| **1** | Skeleton: binary, embedded UI, SQLite, auth, base layout | binary serves login + shell |
| **2** | Job engine + command runner + SSE log streaming | a fake 60s job streams & survives refresh |
| **3** | Prerequisites screen + all checks + auto-remediations | green checklist on a clean Linux host |
| **4** | Proxmox client + connection form + discovery caching | real dropdowns from `172.16.1.101` |
| **5** | KIND bootstrap + `clusterctl init` (pinned versions) | providers `Running` in KIND |
| **6** | Template builder: dry-run, build, catalog | Ubuntu 24.04 template exists in Proxmox |
| **7** | IP plan validator + cluster designer + YAML preview | manifests generated & validated |
| **8** | Cluster apply + live machine watch + kubeconfig | 1 CP + 2 workers `Ready` |
| **9** | Lifecycle: scale / upgrade / delete | verified against the live cluster |
| **10** | Enhancements: pivot, backup, doctor, error translation, polish | — |

Phases 1–3 are pure local work. Phase 4 onward needs the Proxmox server and, for phase 6+,
a **Linux host** to run on.

---

## 8. Open questions (blocking beyond phase 3)

1. **What host will run this binary?** Template building requires Linux + Docker +
   network reachability from the Proxmox VM subnet. Your dev machine here is macOS —
   fine for phases 1–5, but phase 6 needs a Linux host (a VM on the Proxmox box itself
   is the natural answer).
2. **Management cluster:** keep KIND permanently, or pivot into the first workload
   cluster? (Recommendation: offer both, default to pivot.)
3. **Proxmox credentials:** create a dedicated `capmox@pve` API token, or use root token?
   (Recommendation: dedicated token; the app can print the exact `pveum` commands.)
4. **Network:** what bridge/VLAN, gateway, and free IP range should clusters use, and is
   there DHCP on the build network?

---

## References
- [image-builder — Proxmox](https://image-builder.sigs.k8s.io/capi/providers/proxmox)
- [CAPMOX (ionos-cloud/cluster-api-provider-proxmox)](https://github.com/ionos-cloud/cluster-api-provider-proxmox)
- [CAPMOX Usage docs](https://github.com/ionos-cloud/cluster-api-provider-proxmox/blob/main/docs/Usage.md)
- [Cluster API book](https://cluster-api.sigs.k8s.io/)
- [Packer Proxmox builder](https://developer.hashicorp.com/packer/integrations/hashicorp/proxmox)
