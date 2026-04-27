package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/kennguy3n/zk-object-fabric/billing"
)

// ForecastHandler exposes capacity-forecast results for cells.
//
// Route: GET /api/v1/cells/{cellId}/forecast
//
// CapacityResolver is the bridge to the cell registry: given a
// cellID, it returns the cell's declared capacity in bytes.
// Returning (0, false) means "cell not found"; the handler then
// responds with 404. A non-nil error is treated as 500.
type ForecastHandler struct {
	Forecaster *billing.Forecaster
	CapacityResolver func(ctx context.Context, cellID string) (capacityBytes uint64, ok bool, err error)
}

// Register mounts the forecast route on mux.
func (h *ForecastHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/cells/", h.dispatch)
}

// ServeHTTP exists so the handler can be tested without routing
// through ServeMux.
func (h *ForecastHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.dispatch(w, r)
}

func (h *ForecastHandler) dispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/cells/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[1] != "forecast" {
		http.NotFound(w, r)
		return
	}
	cellID := parts[0]
	if cellID == "" {
		http.NotFound(w, r)
		return
	}
	if h.Forecaster == nil {
		http.Error(w, "forecaster not configured", http.StatusServiceUnavailable)
		return
	}
	if h.CapacityResolver == nil {
		http.Error(w, "capacity resolver not configured", http.StatusServiceUnavailable)
		return
	}
	capacity, ok, err := h.CapacityResolver(r.Context(), cellID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	res, err := h.Forecaster.Forecast(r.Context(), cellID, capacity)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			http.Error(w, err.Error(), 499)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}
