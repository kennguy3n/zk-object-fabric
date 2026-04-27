// Console handlers for per-bucket intra-tenant dedup policy.
//
// Routes (all served under /api/v1/tenants/{tid}/buckets/{bucket}):
//
//	POST   /dedup-policy   — enable dedup (or replace existing policy)
//	GET    /dedup-policy   — read the current policy
//	DELETE /dedup-policy   — disable dedup
//
// The handler reuses BucketStore for tenant/bucket existence checks
// and PlacementStore as the source of truth for dedup level
// validation: object+block dedup requires that the placement policy
// resolves to a Ceph RGW backend on a dedicated cell. Object-level
// dedup is portable across all backends.
//
// See docs/PROPOSAL.md §3.14.
package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kennguy3n/zk-object-fabric/metadata"
)

// DedupPolicyStore persists per-bucket dedup policies. Implementations
// are expected to be safe for concurrent use.
type DedupPolicyStore interface {
	// GetDedupPolicy returns the policy for (tenantID, bucket) or
	// (nil, nil) when none is configured. A non-nil error means
	// the lookup itself failed; the caller should surface 500.
	GetDedupPolicy(ctx context.Context, tenantID, bucket string) (*metadata.DedupPolicy, error)

	// PutDedupPolicy persists the policy for (tenantID, bucket),
	// replacing any previous record.
	PutDedupPolicy(ctx context.Context, tenantID, bucket string, policy metadata.DedupPolicy) error

	// DeleteDedupPolicy removes the policy for (tenantID, bucket).
	// Returns nil if no policy was set (idempotent disable).
	DeleteDedupPolicy(ctx context.Context, tenantID, bucket string) error
}

// dedupPolicyRequestBody is the JSON shape POSTed to enable dedup.
// Scope is currently constant ("intra_tenant") and is rejected if a
// caller sends anything else.
type dedupPolicyRequestBody struct {
	Enabled bool   `json:"enabled"`
	Scope   string `json:"scope"`
	Level   string `json:"level"`
}

const (
	dedupScopeIntraTenant = "intra_tenant"
	dedupLevelObject      = "object"
	dedupLevelObjectBlock = "object+block"
)

// registerDedupRoutes attaches the three dedup-policy verbs to the
// supplied mux under /api/v1/tenants/. Called from Handler.Register.
func (h *Handler) registerDedupRoutes(mux *http.ServeMux) {
	if h.cfg.DedupPolicies == nil {
		return
	}
	mux.HandleFunc("/api/v1/tenants/", h.dispatchDedup)
}

func (h *Handler) dispatchDedup(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AdminAuth != nil && !h.cfg.AdminAuth(r) {
		writeError(w, http.StatusUnauthorized, "admin authorization required")
		return
	}
	tenantID, bucket, ok := parseDedupPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid path, expected /api/v1/tenants/{tid}/buckets/{bucket}/dedup-policy")
		return
	}
	if !h.ensureTenantExists(w, tenantID) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.getDedupPolicy(w, r, tenantID, bucket)
	case http.MethodPost, http.MethodPut:
		h.putDedupPolicy(w, r, tenantID, bucket)
	case http.MethodDelete:
		h.deleteDedupPolicy(w, r, tenantID, bucket)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// parseDedupPath extracts (tenantID, bucket) from
// /api/v1/tenants/{tid}/buckets/{bucket}/dedup-policy.
func parseDedupPath(p string) (tenantID, bucket string, ok bool) {
	const prefix = "/api/v1/tenants/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(p, prefix)
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 4 {
		return "", "", false
	}
	if parts[1] != "buckets" || parts[3] != "dedup-policy" {
		return "", "", false
	}
	tenantID = parts[0]
	bucket = parts[2]
	if tenantID == "" || bucket == "" {
		return "", "", false
	}
	return tenantID, bucket, true
}

func (h *Handler) getDedupPolicy(w http.ResponseWriter, r *http.Request, tenantID, bucket string) {
	policy, err := h.cfg.DedupPolicies.GetDedupPolicy(r.Context(), tenantID, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "dedup policy lookup failed: "+err.Error())
		return
	}
	if policy == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"tenant_id": tenantID,
			"bucket":    bucket,
			"enabled":   false,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": tenantID,
		"bucket":    bucket,
		"enabled":   policy.Enabled,
		"scope":     policy.Scope,
		"level":     policy.Level,
	})
}

func (h *Handler) putDedupPolicy(w http.ResponseWriter, r *http.Request, tenantID, bucket string) {
	var body dedupPolicyRequestBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if !body.Enabled {
		// Treat POST { enabled: false } as a delete so the SPA
		// has a single endpoint that toggles on/off.
		if err := h.cfg.DedupPolicies.DeleteDedupPolicy(r.Context(), tenantID, bucket); err != nil {
			writeError(w, http.StatusInternalServerError, "dedup policy delete failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"tenant_id": tenantID,
			"bucket":    bucket,
			"enabled":   false,
		})
		return
	}
	scope := body.Scope
	if scope == "" {
		scope = dedupScopeIntraTenant
	}
	if scope != dedupScopeIntraTenant {
		writeError(w, http.StatusBadRequest, "scope must be \"intra_tenant\"; cross-tenant dedup is permanently excluded")
		return
	}
	level := body.Level
	if level == "" {
		level = dedupLevelObject
	}
	if level != dedupLevelObject && level != dedupLevelObjectBlock {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("level must be %q or %q", dedupLevelObject, dedupLevelObjectBlock))
		return
	}
	if level == dedupLevelObjectBlock {
		ok, reason := h.bucketResolvesToCephRGW(r.Context(), tenantID, bucket)
		if !ok {
			writeError(w, http.StatusBadRequest, "level \"object+block\" requires the bucket's placement to resolve to a Ceph RGW backend on a dedicated cell: "+reason)
			return
		}
	}
	policy := metadata.DedupPolicy{
		Enabled: true,
		Scope:   scope,
		Level:   level,
	}
	if err := h.cfg.DedupPolicies.PutDedupPolicy(r.Context(), tenantID, bucket, policy); err != nil {
		writeError(w, http.StatusInternalServerError, "dedup policy write failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": tenantID,
		"bucket":    bucket,
		"enabled":   true,
		"scope":     scope,
		"level":     level,
	})
}

func (h *Handler) deleteDedupPolicy(w http.ResponseWriter, r *http.Request, tenantID, bucket string) {
	if err := h.cfg.DedupPolicies.DeleteDedupPolicy(r.Context(), tenantID, bucket); err != nil {
		writeError(w, http.StatusInternalServerError, "dedup policy delete failed: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bucketResolvesToCephRGW is the Phase 3.5 guardrail for the
// "object+block" dedup level: only Ceph RGW deployments on
// dedicated cells expose RADOS-tier chunk dedup, so accepting the
// level on a placement that resolves anywhere else (Wasabi, B2,
// Storj, local-fs-dev) would silently downgrade the customer
// expectation. Callers reject the request with 400 when this
// returns false.
//
// The check is conservative: it returns false (with a reason) for
// any tenant whose placement is missing, points at a non-Ceph
// backend, or whose Cells store has no record of a dedicated cell.
// A tenant on a shared cell must first request a dedicated cell
// (POST /api/tenants/{id}/dedicated-cells) before this guardrail
// will accept "object+block".
func (h *Handler) bucketResolvesToCephRGW(ctx context.Context, tenantID, bucket string) (ok bool, reason string) {
	if h.cfg.Placements == nil {
		return false, "placement store not configured"
	}
	policy, found, err := h.cfg.Placements.GetPlacement(ctx, tenantID)
	if err != nil {
		return false, "placement lookup failed: " + err.Error()
	}
	if !found {
		return false, "tenant has no placement policy"
	}
	// Walk the placement's allowed backends; we accept the
	// "object+block" upgrade only when at least one resolves to a
	// Ceph RGW provider (provider name prefixed with "ceph").
	hasCeph := false
	for _, provider := range policy.Spec.Placement.Provider {
		if strings.HasPrefix(strings.ToLower(provider), "ceph") {
			hasCeph = true
			break
		}
	}
	if !hasCeph {
		return false, "placement allowed_backends do not include a Ceph RGW provider"
	}
	if h.cfg.Cells == nil {
		return false, "dedicated cell store not configured (object+block dedup requires a dedicated cell)"
	}
	cells, err := h.cfg.Cells.ListDedicatedCells(ctx, tenantID)
	if err != nil {
		return false, "dedicated cell lookup failed: " + err.Error()
	}
	if len(cells) == 0 {
		return false, "tenant has no dedicated cell provisioned"
	}
	_ = bucket // bucket is part of the route shape but the placement is currently tenant-scoped.
	return true, ""
}

// listDedupPoliciesIfImplemented is a hook the SPA can call once
// per session to bulk-load every bucket's dedup policy. It is not
// in the Phase 3.5 task list but is a natural extension; left as a
// TODO for the next phase.
//
// TODO(phase4): GET /api/v1/tenants/{tid}/dedup-policies returns
// every (bucket, policy) tuple for the tenant.
var _ = errors.New
