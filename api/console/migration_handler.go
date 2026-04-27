package console

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kennguy3n/zk-object-fabric/migration"
)

// MigrationHandler exposes fleet-migration progress to the
// management console.
//
// Routes:
//
//	GET /api/v1/migrations          — list every job
//	GET /api/v1/migrations/{jobId}  — single job
type MigrationHandler struct {
	Orchestrator *migration.FleetOrchestrator
}

// Register mounts the routes on mux.
func (h *MigrationHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/migrations", h.list)
	mux.HandleFunc("/api/v1/migrations/", h.dispatch)
}

// ServeHTTP supports test routing.
func (h *MigrationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.TrimRight(r.URL.Path, "/") == "/api/v1/migrations" {
		h.list(w, r)
		return
	}
	h.dispatch(w, r)
}

func (h *MigrationHandler) list(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Orchestrator == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.Orchestrator.Jobs())
}

func (h *MigrationHandler) dispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/migrations/")
	if id == "" {
		h.list(w, r)
		return
	}
	if h.Orchestrator == nil {
		http.NotFound(w, r)
		return
	}
	j, ok := h.Orchestrator.Job(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(j)
}
