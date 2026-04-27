package console

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/zk-object-fabric/internal/auth"
)

// LegalHoldHandler exposes operator endpoints for asserting and
// releasing legal holds.
//
// Routes:
//
//	POST /api/v1/tenants/{tid}/legal-hold       — issue a hold
//	GET  /api/v1/tenants/{tid}/legal-hold       — list tenant holds
//	POST /api/v1/tenants/{tid}/legal-hold/{id}/release — release
//
// The handler does not authenticate the operator; that is left
// to the upstream console auth middleware so the same shape can
// run behind tenant-admin or platform-admin auth depending on
// deployment.
type LegalHoldHandler struct {
	Store auth.LegalHoldStore
	Now   func() time.Time
}

// Register mounts the routes on mux.
func (h *LegalHoldHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/tenants/", h.dispatch)
}

// ServeHTTP supports test routing.
func (h *LegalHoldHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.dispatch(w, r) }

func (h *LegalHoldHandler) dispatch(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/tenants/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[1] != "legal-hold" {
		http.NotFound(w, r)
		return
	}
	tenantID := parts[0]
	if tenantID == "" {
		http.NotFound(w, r)
		return
	}
	if h.Store == nil {
		http.Error(w, "legal-hold store not configured", http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.Method == http.MethodPost && len(parts) == 2:
		h.issue(w, r, tenantID)
	case r.Method == http.MethodGet && len(parts) == 2:
		h.list(w, r, tenantID)
	case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "release":
		h.release(w, r, tenantID, parts[2])
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// CreateRequest is the JSON body for POST .../legal-hold.
type CreateRequest struct {
	Bucket    string    `json:"bucket"`
	ObjectKey string    `json:"object_key"`
	Reason    string    `json:"reason"`
	CaseID    string    `json:"case_id"`
	IssuedBy  string    `json:"issued_by"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (h *LegalHoldHandler) issue(w http.ResponseWriter, r *http.Request, tenantID string) {
	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Reason == "" || req.IssuedBy == "" {
		http.Error(w, "reason and issued_by are required", http.StatusBadRequest)
		return
	}
	now := h.now()
	hold := auth.LegalHold{
		ID:        newHoldID(),
		TenantID:  tenantID,
		Bucket:    req.Bucket,
		ObjectKey: req.ObjectKey,
		Reason:    req.Reason,
		CaseID:    req.CaseID,
		IssuedBy:  req.IssuedBy,
		ExpiresAt: req.ExpiresAt,
		CreatedAt: now,
	}
	if err := h.Store.Create(r.Context(), hold); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(hold)
}

func (h *LegalHoldHandler) list(w http.ResponseWriter, r *http.Request, tenantID string) {
	holds, err := h.Store.List(r.Context(), tenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(holds)
}

func (h *LegalHoldHandler) release(w http.ResponseWriter, r *http.Request, tenantID, id string) {
	// Look up the hold first so an operator authenticated for
	// tenant A cannot release tenant B's hold by guessing or
	// scraping its ID. The path-level tenant must match the
	// stored tenant on the hold.
	hold, err := h.Store.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, auth.ErrLegalHoldNotFound) {
			http.Error(w, "legal hold not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if hold.TenantID != tenantID {
		// Return 404 (not 403) so this endpoint cannot be used
		// to enumerate hold IDs across tenants.
		http.Error(w, "legal hold not found", http.StatusNotFound)
		return
	}
	if err := h.Store.Release(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *LegalHoldHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func newHoldID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "hold-" + hex.EncodeToString(b[:])
}
