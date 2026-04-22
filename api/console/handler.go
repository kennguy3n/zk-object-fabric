// Package console implements the tenant-console HTTP API that the
// Phase 3 React frontend (in frontend/) consumes. The endpoints
// expose read/write operations on tenant records, per-tenant usage
// counters sourced from the ClickHouse billing pipeline, API-key
// management, and the tenant's placement policy.
//
// The console API is intentionally separate from the S3-compatible
// data-plane surface in api/s3compat. Operators wire it on its own
// HTTP mux (and typically its own listener, e.g. :8081 while the S3
// handler owns :8080) so that a runaway S3 workload cannot starve
// the control surface that operators use to diagnose it. Requests
// are authenticated via a caller-supplied AdminAuthenticator
// (typically an HMAC or bearer-token guard on the admin network);
// this package does not itself enforce auth, only routing and
// payload shape.
//
// Endpoints:
//
//	GET  /api/tenants/{id}            — tenant record
//	GET  /api/tenants/{id}/usage      — usage summary (dimension, period)
//	POST /api/tenants/{id}/keys       — create API key (one-time secret reveal)
//	GET  /api/tenants/{id}/placement  — placement policy
//	PUT  /api/tenants/{id}/placement  — replace placement policy
//
// Phase 3 ships a scaffold: TenantStore reads off the existing
// in-memory tenant store, UsageQuery is a thin interface the
// ClickHouse billing sink can satisfy, and PlacementStore is an
// in-memory policy store suitable for development and tests. The
// production binding is wired in cmd/gateway/main.go.
package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/placement_policy"
	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// TenantStore is the read surface the console needs to answer
// tenant-record queries and to register new API keys. The method
// set intentionally mirrors what the gateway's internal tenant
// directory already exposes so production can supply a single
// implementation.
type TenantStore interface {
	// LookupTenant returns the tenant record for tenantID. The
	// record must not include any secret material (API secret
	// keys, CMK material, etc.).
	LookupTenant(tenantID string) (tenant.Tenant, bool)

	// AddAPIKey associates (accessKey, secretKey) with tenantID.
	// Implementations should reject duplicate accessKey values.
	AddAPIKey(tenantID, accessKey, secretKey string) error

	// CreateTenant registers a new tenant record. The B2C
	// self-service signup handler calls this before minting an
	// initial API key pair. Implementations should reject a
	// tenant ID that is already registered.
	CreateTenant(t tenant.Tenant) error
}

// UsageQuery is the interface the console uses to summarize per-
// tenant usage for the dashboard. The ClickHouse billing sink can
// satisfy this directly; tests supply an in-memory stub.
type UsageQuery interface {
	// TenantUsage returns aggregated counters for tenantID over
	// the (start, end) period. The returned map is keyed by
	// billing.Dimension (as a string) and holds the total bytes
	// or request count for that dimension.
	TenantUsage(ctx context.Context, tenantID string, start, end time.Time) (map[string]uint64, error)
}

// PlacementStore persists per-tenant placement policies. The
// console exposes GET/PUT on /api/tenants/{id}/placement so the
// frontend's policy editor can round-trip through the store.
type PlacementStore interface {
	// GetPlacement returns the tenant's current placement policy.
	// A (zero-value, false) return means "no policy configured";
	// the handler renders that as an empty policy body so the
	// frontend editor can author from scratch.
	GetPlacement(ctx context.Context, tenantID string) (placement_policy.Policy, bool, error)

	// PutPlacement replaces the tenant's placement policy. The
	// handler calls Policy.Validate before calling PutPlacement,
	// so implementations may assume structural validity.
	PutPlacement(ctx context.Context, tenantID string, policy placement_policy.Policy) error
}

// Clock is the Now-function type the handler uses for timestamps.
// Tests override it to make responses deterministic.
type Clock func() time.Time

// KeyGenerator mints (accessKey, secretKey) pairs for new API keys.
// The default uses crypto/rand; tests override for determinism.
type KeyGenerator func() (accessKey, secretKey string, err error)

// Config collects the dependencies Handler needs.
type Config struct {
	Tenants    TenantStore
	Usage      UsageQuery
	Placements PlacementStore

	// Auth is the email → (password hash, tenant ID) store the
	// B2C signup / login handler reads and writes. When nil the
	// auth endpoints return 503 so the control plane can ship
	// without self-service onboarding until it is wired in.
	Auth AuthStore

	// Tokens mints and resolves the opaque bearer tokens the SPA
	// presents on subsequent requests. When nil, Register provides
	// an in-memory default (NewMemoryTokenStore) so dev / test
	// deployments do not need to set it explicitly.
	Tokens TokenStore

	// AuthHooks are the optional production integrations the
	// signup flow needs (CAPTCHA, verification email). All hooks
	// are no-ops by default.
	AuthHooks AuthHooks

	// UsageStreamInterval is the SSE usage-stream poll cadence
	// (see sse_handler.go). Defaults to 5 seconds.
	UsageStreamInterval time.Duration

	// UsageStreamWindow is the lookback window each SSE frame
	// reports over. Defaults to DefaultUsageWindow when zero.
	UsageStreamWindow time.Duration

	// Now returns the current time. Defaults to time.Now.
	Now Clock

	// GenerateKey mints access/secret pairs. Defaults to 20-byte
	// hex access keys and 40-byte hex secret keys.
	GenerateKey KeyGenerator

	// NewTenantID mints fresh tenant identifiers for signup.
	// Defaults to a 16-byte hex identifier prefixed with "t-".
	NewTenantID func() (string, error)

	// DefaultUsageWindow is the lookback window used when a GET
	// /usage request does not specify start/end query parameters.
	// Defaults to 30 days.
	DefaultUsageWindow time.Duration
}

// Handler routes console-API requests. Use New to construct, then
// call Register with an http.ServeMux (or attach to your own
// router via ServeHTTP).
type Handler struct {
	cfg Config
}

// New returns a Handler with cfg defaults filled in.
func New(cfg Config) *Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.GenerateKey == nil {
		cfg.GenerateKey = defaultKeyGenerator
	}
	if cfg.DefaultUsageWindow <= 0 {
		cfg.DefaultUsageWindow = 30 * 24 * time.Hour
	}
	return &Handler{cfg: cfg}
}

// Register attaches the console routes to mux. Route prefixes:
//
//	/api/tenants/{id}
//	/api/tenants/{id}/usage
//	/api/tenants/{id}/keys
//	/api/tenants/{id}/placement
//	/api/v1/auth/signup
//	/api/v1/auth/login
//	/api/v1/usage/stream/{id}
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/tenants/", h.dispatch)

	tokens := h.cfg.Tokens
	if tokens == nil {
		tokens = NewMemoryTokenStore()
	}
	if h.cfg.Auth != nil {
		auth := NewAuthHandler(AuthConfig{
			Tenants:     h.cfg.Tenants,
			Auth:        h.cfg.Auth,
			Tokens:      tokens,
			GenerateKey: h.cfg.GenerateKey,
			NewTenantID: h.cfg.NewTenantID,
			Hooks:       h.cfg.AuthHooks,
			Now:         h.cfg.Now,
		})
		auth.Register(mux)
	}
	if h.cfg.Usage != nil {
		sse := NewUsageStreamHandler(UsageStreamConfig{
			Usage:    h.cfg.Usage,
			Interval: h.cfg.UsageStreamInterval,
			Window:   h.cfg.usageStreamWindowEffective(),
			Now:      h.cfg.Now,
		})
		sse.Register(mux)
	}
}

// usageStreamWindowEffective returns UsageStreamWindow, falling back
// to DefaultUsageWindow when the operator did not set one explicitly.
func (c Config) usageStreamWindowEffective() time.Duration {
	if c.UsageStreamWindow > 0 {
		return c.UsageStreamWindow
	}
	return c.DefaultUsageWindow
}

// ServeHTTP lets callers attach the handler to any http.Handler
// surface (reverse proxy, chi router, etc.) without going through a
// ServeMux.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.dispatch(w, r)
}

func (h *Handler) dispatch(w http.ResponseWriter, r *http.Request) {
	tenantID, suffix, ok := parsePath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid path, expected /api/tenants/{id}[/subresource]")
		return
	}
	switch suffix {
	case "":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.getTenant(w, r, tenantID)
	case "usage":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.getUsage(w, r, tenantID)
	case "keys":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.createKey(w, r, tenantID)
	case "placement":
		switch r.Method {
		case http.MethodGet:
			h.getPlacement(w, r, tenantID)
		case http.MethodPut:
			h.putPlacement(w, r, tenantID)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	default:
		writeError(w, http.StatusNotFound, "unknown subresource "+suffix)
	}
}

// getTenant handles GET /api/tenants/{id}.
func (h *Handler) getTenant(w http.ResponseWriter, r *http.Request, tenantID string) {
	if h.cfg.Tenants == nil {
		writeError(w, http.StatusServiceUnavailable, "tenant store not configured")
		return
	}
	t, ok := h.cfg.Tenants.LookupTenant(tenantID)
	if !ok {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// UsageResponse is the payload returned by GET /api/tenants/{id}/usage.
type UsageResponse struct {
	TenantID string            `json:"tenant_id"`
	Start    time.Time         `json:"start"`
	End      time.Time         `json:"end"`
	Counters map[string]uint64 `json:"counters"`
}

// getUsage handles GET /api/tenants/{id}/usage.
func (h *Handler) getUsage(w http.ResponseWriter, r *http.Request, tenantID string) {
	if h.cfg.Usage == nil {
		writeError(w, http.StatusServiceUnavailable, "usage query not configured")
		return
	}
	now := h.cfg.Now()
	end := now
	start := now.Add(-h.cfg.DefaultUsageWindow)
	q := r.URL.Query()
	if s := q.Get("start"); s != "" {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid start: "+err.Error())
			return
		}
		start = parsed
	}
	if s := q.Get("end"); s != "" {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid end: "+err.Error())
			return
		}
		end = parsed
	}
	if !start.Before(end) {
		writeError(w, http.StatusBadRequest, "start must be before end")
		return
	}
	counters, err := h.cfg.Usage.TenantUsage(r.Context(), tenantID, start, end)
	if err != nil {
		writeError(w, http.StatusBadGateway, "usage query failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, UsageResponse{
		TenantID: tenantID,
		Start:    start,
		End:      end,
		Counters: counters,
	})
}

// CreateKeyResponse is the payload returned by POST
// /api/tenants/{id}/keys. The SecretKey is only returned on creation
// and never again; the frontend surfaces a one-time reveal so the
// operator can copy it before it disappears.
type CreateKeyResponse struct {
	TenantID  string    `json:"tenant_id"`
	AccessKey string    `json:"access_key"`
	SecretKey string    `json:"secret_key"`
	CreatedAt time.Time `json:"created_at"`
}

// createKey handles POST /api/tenants/{id}/keys.
func (h *Handler) createKey(w http.ResponseWriter, r *http.Request, tenantID string) {
	if h.cfg.Tenants == nil {
		writeError(w, http.StatusServiceUnavailable, "tenant store not configured")
		return
	}
	if _, ok := h.cfg.Tenants.LookupTenant(tenantID); !ok {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	accessKey, secretKey, err := h.cfg.GenerateKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate key: "+err.Error())
		return
	}
	if err := h.cfg.Tenants.AddAPIKey(tenantID, accessKey, secretKey); err != nil {
		writeError(w, http.StatusInternalServerError, "register key: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, CreateKeyResponse{
		TenantID:  tenantID,
		AccessKey: accessKey,
		SecretKey: secretKey,
		CreatedAt: h.cfg.Now(),
	})
}

// getPlacement handles GET /api/tenants/{id}/placement.
func (h *Handler) getPlacement(w http.ResponseWriter, r *http.Request, tenantID string) {
	if h.cfg.Placements == nil {
		writeError(w, http.StatusServiceUnavailable, "placement store not configured")
		return
	}
	pol, ok, err := h.cfg.Placements.GetPlacement(r.Context(), tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "placement query failed: "+err.Error())
		return
	}
	if !ok {
		// Render an empty shell so the frontend's editor can
		// author a policy from scratch without a second round-trip.
		pol = placement_policy.Policy{Tenant: tenantID}
	}
	writeJSON(w, http.StatusOK, pol)
}

// maxPlacementPolicyBytes caps the request body size the console
// will decode on PUT /api/tenants/{id}/placement. Placement policies
// are small structured documents (backends, routing rules, retention
// windows); 64 KiB is three orders of magnitude above a realistic
// policy and keeps a pathological or hostile client from exhausting
// gateway memory by streaming a large JSON payload at the console.
const maxPlacementPolicyBytes int64 = 64 * 1024

// putPlacement handles PUT /api/tenants/{id}/placement.
func (h *Handler) putPlacement(w http.ResponseWriter, r *http.Request, tenantID string) {
	if h.cfg.Placements == nil {
		writeError(w, http.StatusServiceUnavailable, "placement store not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPlacementPolicyBytes)
	var pol placement_policy.Policy
	if err := json.NewDecoder(r.Body).Decode(&pol); err != nil {
		if _, tooLarge := err.(*http.MaxBytesError); tooLarge {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("placement policy exceeds %d bytes", maxPlacementPolicyBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "decode policy: "+err.Error())
		return
	}
	// Path-binding takes precedence so a URL /api/tenants/acme/placement
	// whose body carries a different tenant does not silently
	// overwrite the wrong tenant's policy.
	pol.Tenant = tenantID
	if err := pol.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.cfg.Placements.PutPlacement(r.Context(), tenantID, pol); err != nil {
		writeError(w, http.StatusInternalServerError, "persist policy: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

// parsePath splits /api/tenants/{id}[/suffix] into (id, suffix, ok).
// The trailing suffix is returned as a single string so callers can
// switch on it; multi-segment suffixes are not supported.
func parsePath(p string) (tenantID, suffix string, ok bool) {
	const prefix = "/api/tenants/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(p, prefix)
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		return "", "", false
	}
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return rest, "", true
	}
	tenantID = rest[:slash]
	suffix = rest[slash+1:]
	if tenantID == "" {
		return "", "", false
	}
	if strings.Contains(suffix, "/") {
		return "", "", false
	}
	return tenantID, suffix, true
}

// defaultKeyGenerator mints a 20-byte hex access key and a 40-byte
// hex secret key. The access key is short enough to read aloud
// during support calls; the secret key is long enough that brute-
// forcing it is uneconomic.
func defaultKeyGenerator() (string, string, error) {
	access := make([]byte, 10)
	if _, err := rand.Read(access); err != nil {
		return "", "", fmt.Errorf("rand access: %w", err)
	}
	secret := make([]byte, 20)
	if _, err := rand.Read(secret); err != nil {
		return "", "", fmt.Errorf("rand secret: %w", err)
	}
	return hex.EncodeToString(access), hex.EncodeToString(secret), nil
}

// errorResponse is the JSON body returned for all non-2xx responses.
type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
