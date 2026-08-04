package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"pvekube/internal/capi"
	"pvekube/internal/ui"
)

func (s *Server) handleClusterResourcesPage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ui.Render(w, "cluster_resources", map[string]any{"ClusterName": name})
}

// handleClusterResourcesData is the JSON API the resource viewer page's JS
// fetches once on load. All namespace/text filtering happens client-side
// against this one payload — see resources_view.go's package comment.
func (s *Server) handleClusterResourcesData(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	view, err := capi.GetResourcesView(ctx, s.dataDir, s.binDir, name)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(view)
}
