package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"pvekube/internal/capi"
	"pvekube/internal/jobs"
	"pvekube/internal/proxmox"
	"pvekube/internal/ui"
)

// workerDiskPolicy is the extra data disk a cluster's workers should have.
type workerDiskPolicy struct {
	Storage string
	SizeGiB int
}

func (s *Server) getWorkerDiskPolicy(name string) (workerDiskPolicy, bool) {
	var p workerDiskPolicy
	err := s.db.QueryRow(`SELECT storage, size_gib FROM cluster_worker_disk WHERE cluster_name = ?`, name).Scan(&p.Storage, &p.SizeGiB)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			slog.Warn("reading worker disk policy", "cluster", name, "error", err)
		}
		return workerDiskPolicy{}, false
	}
	return p, true
}

func (s *Server) setWorkerDiskPolicy(name string, p workerDiskPolicy) error {
	_, err := s.db.Exec(`INSERT INTO cluster_worker_disk (cluster_name, storage, size_gib, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(cluster_name) DO UPDATE SET storage = excluded.storage, size_gib = excluded.size_gib, updated_at = CURRENT_TIMESTAMP`,
		name, p.Storage, p.SizeGiB)
	return err
}

// clusterProxmox builds a Proxmox client for the connection a cluster lives on.
func (s *Server) clusterProxmox(name string) (*proxmox.Client, int64, error) {
	var connID int64
	if err := s.db.QueryRow(`SELECT connection_id FROM clusters WHERE name = ?`, name).Scan(&connID); err != nil {
		return nil, 0, fmt.Errorf("PVEKube has no record of cluster %q", name)
	}
	conn, err := s.getConnectionByID(connID)
	if err != nil {
		return nil, 0, fmt.Errorf("the Proxmox connection of %q no longer exists", name)
	}
	pve, err := s.proxmoxClientFor(conn)
	if err != nil {
		return nil, 0, fmt.Errorf("opening the Proxmox credentials: %w", err)
	}
	return pve, connID, nil
}

// attachWorkerDisks appends the "attach data disk" step to an upgrade, resize
// or worker-scale job when the cluster has a disk policy, so replacement
// workers get their disk without anyone having to remember.
func (s *Server) attachWorkerDisks(name string, spec *jobs.Spec) *jobs.Spec {
	p, ok := s.getWorkerDiskPolicy(name)
	if !ok {
		return spec
	}
	pve, _, err := s.clusterProxmox(name)
	if err != nil {
		slog.Warn("worker disk policy not applied", "cluster", name, "error", err)
		return spec
	}
	return capi.AppendWorkerDiskStep(spec, s.dataDir, s.binDir, name, pve, p.Storage, p.SizeGiB)
}

// parseWorkerDiskForm validates the posted storage and size.
func parseWorkerDiskForm(form url.Values) (workerDiskPolicy, error) {
	storage := strings.TrimSpace(form.Get("storage"))
	raw := strings.TrimSpace(form.Get("size_gib"))
	if storage == "" {
		return workerDiskPolicy{}, errors.New("choose the Proxmox storage the new disk should be created on")
	}
	if raw == "" {
		return workerDiskPolicy{}, errors.New("enter the disk size in GiB")
	}
	size, err := strconv.Atoi(raw)
	if err != nil {
		return workerDiskPolicy{}, fmt.Errorf("disk size must be a whole number of GiB, got %q", raw)
	}
	if err := proxmox.ValidateExtraDisk(storage, size); err != nil {
		return workerDiskPolicy{}, err
	}
	return workerDiskPolicy{Storage: storage, SizeGiB: size}, nil
}

func (s *Server) renderWorkerDiskPanel(w http.ResponseWriter, r *http.Request, name, errMsg string, posted url.Values) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	data := map[string]any{"ClusterName": name, "CSRF": s.csrfFor(session), "Error": errMsg}

	var storages []string
	var connID int64
	if err := s.db.QueryRow(`SELECT connection_id FROM clusters WHERE name = ?`, name).Scan(&connID); err != nil {
		data["Error"] = "PVEKube has no record of this cluster."
		ui.RenderPartial(w, "cluster_worker_disk", data)
		return
	}
	if snap := s.loadCachedDiscovery(connID); snap != nil {
		for _, st := range snap.Storage {
			if st.SupportsVM {
				storages = append(storages, st.ID)
			}
		}
	}
	data["Storages"] = storages
	data["InStorage"] = posted.Get("storage")
	data["InSize"] = posted.Get("size_gib")

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if vms, skipped, err := capi.WorkerVMs(ctx, s.dataDir, s.binDir, name); err != nil {
		data["Error"] = err.Error()
	} else {
		data["WorkerCount"] = len(vms)
		data["Skipped"] = skipped
	}
	if p, ok := s.getWorkerDiskPolicy(name); ok {
		data["Policy"] = p
	}
	ui.RenderPartial(w, "cluster_worker_disk", data)
}

func (s *Server) handleClusterWorkerDiskForm(w http.ResponseWriter, r *http.Request) {
	s.renderWorkerDiskPanel(w, r, r.PathValue("name"), "", nil)
}

func (s *Server) handleClusterWorkerDisk(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	name := r.PathValue("name")
	if !s.checkCSRF(r, session) {
		s.renderWorkerDiskPanel(w, r, name, "Security token mismatch — please refresh the page and try again.", nil)
		return
	}
	r.ParseForm()

	p, err := parseWorkerDiskForm(r.Form)
	if err != nil {
		s.renderWorkerDiskPanel(w, r, name, err.Error(), r.Form)
		return
	}
	pve, _, err := s.clusterProxmox(name)
	if err != nil {
		s.renderWorkerDiskPanel(w, r, name, err.Error(), r.Form)
		return
	}

	spec := capi.WorkerDiskSpec(s.dataDir, s.binDir, name, pve, p.Storage, p.SizeGiB)
	jobID, err := s.jobs.StartExclusive(spec, `{"cluster":"`+name+`"}`, clusterOperationLock(name))
	if err != nil {
		s.renderWorkerDiskPanel(w, r, name, clusterOperationError(err), r.Form)
		return
	}
	// Remember it so replacement workers get the same disk. Saved after the job
	// is accepted so a refused request leaves the previous policy intact.
	if err := s.setWorkerDiskPolicy(name, p); err != nil {
		slog.Warn("saving worker disk policy", "cluster", name, "error", err)
	}
	ui.RenderPartial(w, "job_progress", map[string]any{
		"JobID": jobID, "Title": spec.Title,
		"WrapperID": "cluster-upgrade-panel", "ReloadURL": "/clusters/" + name + "/status", "ReloadTarget": "#cluster-status",
	})
}

// handleClusterWorkerDiskForget stops the automatic re-attach. It does not
// remove any disk from any VM.
func (s *Server) handleClusterWorkerDiskForget(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(ctxSessionKey{}).(string)
	name := r.PathValue("name")
	if !s.checkCSRF(r, session) {
		s.renderWorkerDiskPanel(w, r, name, "Security token mismatch — please refresh the page and try again.", nil)
		return
	}
	if _, err := s.db.Exec(`DELETE FROM cluster_worker_disk WHERE cluster_name = ?`, name); err != nil {
		s.renderWorkerDiskPanel(w, r, name, "Could not clear the policy: "+err.Error(), nil)
		return
	}
	s.renderWorkerDiskPanel(w, r, name, "", nil)
}
