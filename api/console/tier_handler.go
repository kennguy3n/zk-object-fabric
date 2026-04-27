package console

import (
	"encoding/json"
	"net/http"

	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// TierHandler exposes the canonical product-tier configuration
// to the management console. Routes:
//
//	GET /api/v1/tiers
type TierHandler struct{}

// Register mounts the route on mux.
func (h *TierHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/tiers", h.serveList)
}

// ServeHTTP exists so the handler can be tested directly.
func (h *TierHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.serveList(w, r) }

func (h *TierHandler) serveList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tenant.DefaultTierConfigs())
}
