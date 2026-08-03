package server

import (
	"net/http"
	"os"

	"pvekube/internal/bootstrap"
)

// handleManagementKubeconfig lets the operator download the KIND management
// cluster's kubeconfig for their own kubectl/k9s/Lens if they want to poke
// around directly — PVEKube doesn't require it, but hiding it entirely
// would make this feel like a black box.
func (s *Server) handleManagementKubeconfig(w http.ResponseWriter, r *http.Request) {
	path := bootstrap.KubeconfigPath(s.dataDir)
	b, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "management cluster kubeconfig not found yet — create it from the Prerequisites page first", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="pvekube-management-kubeconfig.yaml"`)
	w.Write(b)
}
