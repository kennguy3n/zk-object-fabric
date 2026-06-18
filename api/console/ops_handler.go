package console

// ops_handler.go exposes a small, read-only operations surface that
// the React console renders for SME operators who do not run
// Grafana/Prometheus. The endpoints aggregate data the gateway
// already tracks internally:
//
//	GET /api/v1/ops/health         — node health snapshot (internal/health.Monitor)
//	GET /api/v1/ops/cache-stats    — hot-object-cache counters + derived hit ratio
//	GET /api/v1/ops/wasabi-budgets — per-tenant Wasabi fair-use egress budgets
//
// Like the other console admin routes (see Config.AdminAuth and the
// dispatch gate in handler.go), every endpoint here is guarded by the
// caller-supplied AdminAuth hook. A nil hook disables the check
// (suitable for dev / tests, hostile otherwise) — identical semantics
// to the main Handler so a deployment wires one authenticator and
// gets the same posture across both surfaces.
//
// The handler holds no business logic of its own: each data source is
// an injected function (mirroring ForecastHandler.CapacityResolver),
// so production wires the live Monitor / cache / guardrail modules in
// cmd/gateway/main.go while tests inject fakes. A nil source makes its
// endpoint report 503 Service Unavailable rather than panicking, so a
// partially-configured gateway still serves the endpoints it can.

import (
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
	"github.com/kennguy3n/zk-object-fabric/internal/health"
)

// HealthReporter returns the current node health snapshot. The
// internal/health.Monitor's Snapshot method satisfies this directly.
type HealthReporter func() health.Snapshot

// CacheStatsReporter returns the hot-object-cache's aggregate
// counters. hot_object_cache.HotObjectCache's Stats method satisfies
// this directly.
type CacheStatsReporter func() hot_object_cache.Stats

// WasabiBudgetReporter returns the per-tenant Wasabi fair-use egress
// budgets. Production computes these from the Wasabi guardrail config
// (providers/wasabi.Guardrails) joined with the billing pipeline's
// per-tenant stored/egress counters; the handler only shapes the
// result for the SPA.
type WasabiBudgetReporter func() ([]WasabiBudget, error)

// OpsConfig wires the ops endpoints' data sources and auth gate. All
// fields are optional: a nil reporter makes its endpoint return 503,
// and a nil AdminAuth disables the admin check (dev only).
type OpsConfig struct {
	Health HealthReporter
	Cache  CacheStatsReporter
	Wasabi WasabiBudgetReporter

	// AdminAuth gates every ops endpoint. Wire it with the same
	// authenticator passed to console.Config.AdminAuth so the ops
	// surface and the tenant/usage/keys surface share one posture.
	AdminAuth func(r *http.Request) bool
}

// OpsHandler serves the read-only operations dashboard endpoints.
// Construct it directly with an OpsConfig and mount it with Register,
// or attach it to any router via ServeHTTP.
type OpsHandler struct {
	cfg OpsConfig
}

// NewOpsHandler returns an OpsHandler bound to cfg.
func NewOpsHandler(cfg OpsConfig) *OpsHandler {
	return &OpsHandler{cfg: cfg}
}

// Register mounts the ops routes on mux under /api/v1/ops/.
func (h *OpsHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/ops/", h.dispatch)
}

// ServeHTTP lets the handler be attached to a router or tested
// without routing through a ServeMux.
func (h *OpsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.dispatch(w, r)
}

func (h *OpsHandler) dispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Admin gate runs before any data source is touched so an
	// unauthenticated caller cannot probe which subsystems are
	// configured by observing 503-vs-200 responses.
	if h.cfg.AdminAuth != nil && !h.cfg.AdminAuth(r) {
		writeError(w, http.StatusUnauthorized, "admin authorization required")
		return
	}
	resource := strings.TrimPrefix(r.URL.Path, "/api/v1/ops/")
	// Reject nested paths (e.g. /api/v1/ops/health/extra) so a
	// confused client does not get a partial match.
	if strings.Contains(resource, "/") {
		http.NotFound(w, r)
		return
	}
	switch resource {
	case "health":
		h.getHealth(w, r)
	case "cache-stats":
		h.getCacheStats(w, r)
	case "wasabi-budgets":
		h.getWasabiBudgets(w, r)
	default:
		http.NotFound(w, r)
	}
}

// OpsHealthResponse is the payload for GET /api/v1/ops/health. Status
// is a coarse, SME-friendly rollup derived from the node state so the
// SPA can render a single green/amber badge without re-implementing
// the state machine; Node carries the full snapshot for operators who
// want the detail.
type OpsHealthResponse struct {
	Status string        `json:"status"`
	Node   OpsHealthNode `json:"node"`
}

// OpsHealthNode is the ops API's own camelCase projection of
// internal/health.Snapshot. Re-shaping here (instead of embedding the
// internal type) keeps every ops endpoint on one JSON convention
// (camelCase, matching OpsCacheStatsResponse and WasabiBudget) and
// decouples the public ops contract from internal/health's wire tags,
// so refactoring the monitor's snake_case JSON cannot silently break
// the SPA.
type OpsHealthNode struct {
	NodeID          string          `json:"nodeId"`
	CellID          string          `json:"cellId,omitempty"`
	State           string          `json:"state"`
	LastPoll        time.Time       `json:"lastPoll,omitempty"`
	StartedAt       time.Time       `json:"startedAt"`
	QuorumThreshold int             `json:"quorumThreshold"`
	HealthyPeers    map[string]bool `json:"healthyPeers,omitempty"`
	Inflight        int64           `json:"inflight"`
	DrainStart      time.Time       `json:"drainStart,omitempty"`
	DrainedAt       time.Time       `json:"drainedAt,omitempty"`
}

// newOpsHealthNode projects an internal health snapshot onto the ops
// API's camelCase node shape.
func newOpsHealthNode(s health.Snapshot) OpsHealthNode {
	return OpsHealthNode{
		NodeID:          s.NodeID,
		CellID:          s.CellID,
		State:           string(s.State),
		LastPoll:        s.LastPoll,
		StartedAt:       s.StartedAt,
		QuorumThreshold: s.QuorumThreshold,
		HealthyPeers:    s.HealthyPeers,
		Inflight:        s.Inflight,
		DrainStart:      s.DrainStart,
		DrainedAt:       s.DrainedAt,
	}
}

// healthStatus collapses the node state machine into ok / degraded so
// the dashboard badge has a stable two-value contract even if new
// intermediate states are added to internal/health later.
func healthStatus(state health.State) string {
	if state == health.StateReady {
		return "ok"
	}
	return "degraded"
}

func (h *OpsHandler) getHealth(w http.ResponseWriter, _ *http.Request) {
	if h.cfg.Health == nil {
		writeError(w, http.StatusServiceUnavailable, "health monitor not configured")
		return
	}
	snap := h.cfg.Health()
	writeJSON(w, http.StatusOK, OpsHealthResponse{
		Status: healthStatus(snap.State),
		Node:   newOpsHealthNode(snap),
	})
}

// OpsCacheStatsResponse is the payload for GET /api/v1/ops/cache-stats.
// HitRatio and Utilization are derived server-side so every consumer
// (SPA gauge, future CLI) agrees on the definition.
type OpsCacheStatsResponse struct {
	Entries     int64   `json:"entries"`
	BytesUsed   int64   `json:"bytesUsed"`
	BytesLimit  int64   `json:"bytesLimit"`
	Hits        uint64  `json:"hits"`
	Misses      uint64  `json:"misses"`
	Evictions   uint64  `json:"evictions"`
	HitRatio    float64 `json:"hitRatio"`
	Utilization float64 `json:"utilization"`
}

func (h *OpsHandler) getCacheStats(w http.ResponseWriter, _ *http.Request) {
	if h.cfg.Cache == nil {
		writeError(w, http.StatusServiceUnavailable, "cache stats not configured")
		return
	}
	s := h.cfg.Cache()
	writeJSON(w, http.StatusOK, OpsCacheStatsResponse{
		Entries:     s.Entries,
		BytesUsed:   s.BytesUsed,
		BytesLimit:  s.BytesLimit,
		Hits:        s.Hits,
		Misses:      s.Misses,
		Evictions:   s.Evictions,
		HitRatio:    hitRatio(s.Hits, s.Misses),
		Utilization: utilization(s.BytesUsed, s.BytesLimit),
	})
}

// hitRatio returns hits/(hits+misses) in [0,1], or 0 when no lookups
// have been recorded yet (avoids a divide-by-zero NaN that would
// serialize as the invalid JSON token `NaN`).
func hitRatio(hits, misses uint64) float64 {
	total := hits + misses
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

// utilization returns used/limit in [0,1], clamped so a transient
// over-fill (used > limit before eviction catches up) does not report
// a >100% gauge. Returns 0 when no limit is configured.
func utilization(used, limit int64) float64 {
	if limit <= 0 || used <= 0 {
		return 0
	}
	r := float64(used) / float64(limit)
	if r > 1 {
		return 1
	}
	return r
}

// WasabiBudget is the per-tenant Wasabi fair-use egress view rendered
// by the WasabiHealthPage. EgressBudgetBytes is the tenant's stored
// volume times the fair-use ratio (1× by default); Status is a coarse
// rollup the dashboard colours from the configured warn/critical
// thresholds.
type WasabiBudget struct {
	TenantID          string  `json:"tenantId"`
	StoredBytes       uint64  `json:"storedBytes"`
	EgressBytes       uint64  `json:"egressBytes"`
	EgressBudgetBytes uint64  `json:"egressBudgetBytes"`
	EgressRatio       float64 `json:"egressRatio"`
	RemainingBytes    uint64  `json:"remainingBytes"`
	Status            string  `json:"status"`
}

// OpsWasabiBudgetsResponse is the payload for
// GET /api/v1/ops/wasabi-budgets.
type OpsWasabiBudgetsResponse struct {
	Budgets []WasabiBudget `json:"budgets"`
}

func (h *OpsHandler) getWasabiBudgets(w http.ResponseWriter, _ *http.Request) {
	if h.cfg.Wasabi == nil {
		writeError(w, http.StatusServiceUnavailable, "wasabi budgets not configured")
		return
	}
	budgets, err := h.cfg.Wasabi()
	if err != nil {
		writeError(w, http.StatusBadGateway, "wasabi budget query failed: "+err.Error())
		return
	}
	// Normalize nil to an empty slice so the SPA always receives a
	// JSON array (`[]`) rather than `null`, which simplifies the
	// client-side .map() without a guard.
	if budgets == nil {
		budgets = []WasabiBudget{}
	}
	writeJSON(w, http.StatusOK, OpsWasabiBudgetsResponse{Budgets: budgets})
}
