package console

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
)

// CostHandler exposes the unified per-tenant cost breakdown that
// collapses the Wasabi / Linode / AWS bills into one view.
//
// Route: GET /api/v1/tenants/{tid}/cost-breakdown[?month=YYYY-MM]
//
// The month query parameter is optional; when omitted the handler
// reports the current calendar month. The breakdown is the
// per-provider components plus the total and the dedup savings (see
// billing.CostBreakdown).
//
// Like ForecastHandler this is a standalone handler with its own
// Register / ServeHTTP so it can be mounted on a dedicated mux or
// driven directly from tests. It is admin-token gated via AdminAuth,
// the same hook shape as console.Config.AdminAuth: a non-nil hook
// must return true or the request is rejected 401; a nil hook
// disables the check (dev only), matching the gateway's other admin
// surfaces.
type CostHandler struct {
	// Reporter computes the breakdown. Required; a nil Reporter
	// makes the route reply 503.
	Reporter billing.CostReporter

	// AdminAuth gates every request when non-nil. Nil disables the
	// check (dev only).
	AdminAuth func(r *http.Request) bool

	// TenantExists, when non-nil, gates the breakdown on the tenant
	// actually existing so an unknown tenant gets a clean 404
	// instead of a zero-valued breakdown — consistent with the
	// other console tenant-subresource handlers (see
	// Handler.ensureTenantExists). Nil disables the check, which is
	// the standalone / test mounting that has no tenant store; the
	// console Handler wires it from Config.Tenants in Register.
	TenantExists func(tenantID string) bool
}

// Register mounts the cost-breakdown route on mux. It uses the
// method-specific pattern form (Go 1.22+) so it can coexist with the
// console Handler's broader "/api/v1/tenants/" subtree registration
// without a routing conflict.
func (h *CostHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/tenants/{tid}/cost-breakdown", func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, r.PathValue("tid"))
	})
}

// ServeHTTP lets the handler be attached to any router (or exercised
// by tests) without going through a pattern-routing ServeMux. It
// parses the tenant ID out of the path itself.
func (h *CostHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tid, ok := parseCostPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.serve(w, r, tid)
}

// parseCostPath extracts {tid} from
// /api/v1/tenants/{tid}/cost-breakdown.
func parseCostPath(p string) (tenantID string, ok bool) {
	const prefix = "/api/v1/tenants/"
	if !strings.HasPrefix(p, prefix) {
		return "", false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(p, prefix), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "cost-breakdown" || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

func (h *CostHandler) serve(w http.ResponseWriter, r *http.Request, tenantID string) {
	if h.AdminAuth != nil && !h.AdminAuth(r) {
		writeError(w, http.StatusUnauthorized, "admin authorization required")
		return
	}
	if tenantID == "" {
		http.NotFound(w, r)
		return
	}
	if h.TenantExists != nil && !h.TenantExists(tenantID) {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	if h.Reporter == nil {
		writeError(w, http.StatusServiceUnavailable, "cost reporter not configured")
		return
	}
	month := r.URL.Query().Get("month")
	if month != "" {
		if _, err := time.Parse("2006-01", month); err != nil {
			writeError(w, http.StatusBadRequest, "invalid month, expected YYYY-MM")
			return
		}
	}
	breakdown, err := h.Reporter.GetCostBreakdown(r.Context(), tenantID, month)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			writeError(w, 499, "client closed request")
			return
		}
		writeInternalError(w, "cost breakdown failed", err)
		return
	}
	writeJSON(w, http.StatusOK, breakdown)
}
