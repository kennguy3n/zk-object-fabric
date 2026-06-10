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
//
// The fleet queue is operator-only data: a job record carries the
// tenant ID, source/destination cell IDs, and backend names for
// every tenant being migrated, so it must not be readable by an
// ordinary tenant session. It is therefore gated by the same
// AdminAuth hook as the cost-breakdown and tenant-subresource
// surfaces (see console.Config.AdminAuth and CostHandler) rather
// than left ungated like the public TierHandler price book.
type MigrationHandler struct {
	Orchestrator *migration.FleetOrchestrator

	// AdminAuth gates every request when non-nil: the hook must
	// return true or the request is rejected 401. A nil hook
	// disables the check (dev / standalone tests only), matching
	// the nil-disables-auth convention the rest of the console
	// uses (Config.AdminAuth, CostHandler.AdminAuth).
	AdminAuth func(r *http.Request) bool
}

// authorized reports whether the request clears the AdminAuth gate.
// A nil hook means "auth disabled" (dev only) and always passes,
// mirroring CostHandler.serve and Handler.dispatch.
func (h *MigrationHandler) authorized(w http.ResponseWriter, r *http.Request) bool {
	if h.AdminAuth != nil && !h.AdminAuth(r) {
		// writeError (JSON {"error":...}) rather than http.Error
		// (text/plain) so the 401 matches the console's API error
		// contract — identical to CostHandler.serve and
		// Handler.dispatch, which the SPA's ApiError parsing and
		// isFeatureUnavailable degradation both assume.
		writeError(w, http.StatusUnauthorized, "admin authorization required")
		return false
	}
	return true
}

// Register mounts the routes on mux.
func (h *MigrationHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/migrations", h.list)
	mux.HandleFunc("/api/v1/migrations/", h.dispatch)
}

// ServeHTTP routes the same way Register's mux mount does, so the two
// surfaces resolve identically: the exact "/api/v1/migrations" lists,
// and the "/api/v1/migrations/" subtree dispatches a single job.
// dispatch() folds the bare-slash case (empty job id) back to list(),
// which is exactly what the mux's subtree handler does for that path —
// matching paths, not just outcomes.
func (h *MigrationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/migrations" {
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
	if !h.authorized(w, r) {
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
		// Folds to list(), which re-checks AdminAuth; do not
		// duplicate the gate here so the two paths share one
		// authorization point.
		h.list(w, r)
		return
	}
	if !h.authorized(w, r) {
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
