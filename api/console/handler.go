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
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/internal/cellops"
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

	// DeleteTenant removes a tenant record by ID. The signup
	// handler calls this to roll back an in-flight signup whose
	// subsequent steps (CreateUser, AddAPIKey, IssueToken) failed
	// so a racing duplicate-email signup does not leave orphaned
	// tenant records behind. Implementations should treat a
	// missing tenantID as a no-op rather than an error.
	DeleteTenant(tenantID string) error
}

// APIKeyDescriptor is the non-secret view of an access key returned
// by GET /api/tenants/{id}/keys. SecretKey is deliberately absent —
// secrets are shown exactly once, at creation time.
type APIKeyDescriptor struct {
	AccessKey string    `json:"accessKey"`
	CreatedAt time.Time `json:"createdAt"`
}

// APIKeyLister is an optional extension of TenantStore that lets
// the console enumerate and revoke access keys for a tenant. When
// the backing store does not implement it, GET and DELETE on the
// keys subresource return 501 Not Implemented so the frontend can
// gracefully hide the list affordance.
type APIKeyLister interface {
	ListAPIKeys(tenantID string) ([]APIKeyDescriptor, error)
	DeleteAPIKey(tenantID, accessKey string) error
}

// BucketDescriptor mirrors the shape frontend/src/api/types.ts
// `Bucket` consumes. BytesStored / ObjectCount are Phase 3
// placeholders populated from the manifest store when available;
// in-memory stores report zero.
type BucketDescriptor struct {
	Name               string    `json:"name"`
	CreatedAt          time.Time `json:"createdAt"`
	PlacementPolicyRef string    `json:"placementPolicyRef"`
	ObjectCount        int64     `json:"objectCount"`
	BytesStored        int64     `json:"bytesStored"`
}

// BucketStore persists the tenant → bucket catalog the SPA reads
// to render BucketsPage. The S3 data plane does not (yet) auto-
// populate this store; the console writes into it as operators
// create buckets via POST /api/tenants/{id}/buckets.
type BucketStore interface {
	ListBuckets(ctx context.Context, tenantID string) ([]BucketDescriptor, error)
	CreateBucket(ctx context.Context, tenantID, name, placementPolicyRef string) (BucketDescriptor, error)
	DeleteBucket(ctx context.Context, tenantID, name string) error
}

// DedicatedCellDescriptor mirrors frontend/src/api/types.ts
// `DedicatedCell` for B2B tenants. An empty slice is a valid
// response for B2C tenants, which never see a dedicated cell.
type DedicatedCellDescriptor struct {
	ID                string  `json:"id"`
	Region            string  `json:"region"`
	Country           string  `json:"country"`
	Status            string  `json:"status"` // provisioning|active|decommissioning
	CapacityPetabytes float64 `json:"capacityPetabytes"`
	Utilization       float64 `json:"utilization"` // 0..1
}

// DedicatedCellStore lists the dedicated cells bound to a B2B
// tenant. Sovereign / b2b_dedicated contracts get one or more
// rows; b2b_shared / b2c_pooled get none.
type DedicatedCellStore interface {
	ListDedicatedCells(ctx context.Context, tenantID string) ([]DedicatedCellDescriptor, error)
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
	Tenants       TenantStore
	Usage         UsageQuery
	Placements    PlacementStore
	Buckets       BucketStore
	Cells         DedicatedCellStore
	DedupPolicies DedupPolicyStore

	// CellProvisioner accepts dedicated-cell provisioning
	// requests submitted via POST /api/tenants/{id}/dedicated-cells.
	// When nil the endpoint returns 503 so the operator workflow
	// is opt-in (Phase 3 wires this only when MetadataDSN is
	// set).
	CellProvisioner cellops.CellProvisioner

	// BillingSink receives a billing event with the
	// TenantCreated dimension after a successful signup so the
	// ClickHouse pipeline starts tracking the tenant from signup
	// time. A nil sink silently drops the event — acceptable for
	// tests but discouraged in production where gap-free metering
	// is load-bearing for invoice generation.
	BillingSink interface {
		Emit(event billing.UsageEvent)
	}

	// BillingProvider is the optional outbound integration to a
	// payment / subscription provider (Stripe, Chargebee, …). The
	// signup handler calls EnsureCustomer immediately after
	// CreateTenant commits so a freshly-minted tenant is reflected
	// on the provider before any usage event lands. A nil provider
	// skips the call — acceptable for dev and the HMAC-only Phase
	// 2 path; production wires cmd/gateway/main.go's provider.
	BillingProvider billing.BillingProvider

	// AdminAuth is the per-request admin-authorization check. The
	// tenant / usage / keys / placement routes all consult it
	// before serving; a nil hook disables the check (suitable for
	// dev, hostile otherwise). Operators wire a bearer-token or
	// HMAC verifier against cfg.Console.AdminToken in
	// cmd/gateway/main.go.
	//
	// The check runs only for tenant-subresource requests; the
	// public auth endpoints (/api/v1/auth/signup, /login) and the
	// usage-stream SSE endpoint enforce their own auth semantics
	// (signup is intentionally unauthenticated, login returns a
	// token, SSE checks the token on the query string).
	AdminAuth func(r *http.Request) bool

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
	// sseHandler serves the tenant-scoped usage stream alias
	// /api/tenants/{id}/usage/stream. Set in Register when Usage
	// is configured so dispatch can forward matching requests
	// without registering a second handler on /api/tenants/.
	sseHandler *UsageStreamHandler
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
//	/api/tenants/{id}/buckets
//	/api/tenants/{id}/dedicated-cells
//	/api/v1/auth/signup
//	/api/v1/auth/login
//	/api/v1/usage/stream/{id}
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/tenants/", h.dispatch)

	tokens := h.cfg.Tokens
	if tokens == nil {
		// MemoryTokenStore is process-local and loses every
		// issued token on restart; it is safe for dev / tests
		// but NOT for production deploys behind a load
		// balancer where different replicas must agree on
		// which tokens are valid. Log loudly so an operator
		// running the gateway without wiring Tokens in
		// config.yaml sees the warning at startup.
		log.Printf("console: Tokens not configured; falling back to in-memory MemoryTokenStore — DO NOT use in production")
		tokens = NewMemoryTokenStore()
	}
	if h.cfg.Auth != nil {
		if _, ok := h.cfg.Auth.(*MemoryAuthStore); ok {
			log.Printf("console: Auth is a MemoryAuthStore — DO NOT use in production; wire a persistent AuthStore")
		}
		auth := NewAuthHandler(AuthConfig{
			Tenants:         h.cfg.Tenants,
			Auth:            h.cfg.Auth,
			Tokens:          tokens,
			GenerateKey:     h.cfg.GenerateKey,
			NewTenantID:     h.cfg.NewTenantID,
			Hooks:           h.cfg.AuthHooks,
			BillingSink:     h.cfg.BillingSink,
			BillingProvider: h.cfg.BillingProvider,
			Now:             h.cfg.Now,
		})
		auth.Register(mux)
	}
	if h.cfg.Usage != nil {
		sse := NewUsageStreamHandler(UsageStreamConfig{
			Usage:    h.cfg.Usage,
			Tokens:   tokens,
			Interval: h.cfg.UsageStreamInterval,
			Window:   h.cfg.usageStreamWindowEffective(),
			Now:      h.cfg.Now,
		})
		sse.Register(mux)
		// dispatch forwards GET /api/tenants/{id}/usage/stream
		// requests to this same handler so the console mux
		// exposes the SSE feed under both the legacy v1 path
		// and the tenant-scoped form.
		h.sseHandler = sse
	}
	h.registerDedupRoutes(mux)
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
	// Phase 3.5: /api/v1/tenants/{tid}/buckets/{bucket}/dedup-policy
	// is registered in Register() under a different mux prefix
	// than the legacy /api/tenants/ surface. ServeHTTP callers
	// (test harnesses, custom routers) need the same routing
	// fan-out; check the prefix here so handler.ServeHTTP
	// matches handler.Register's behavior.
	if strings.HasPrefix(r.URL.Path, "/api/v1/tenants/") {
		h.dispatchDedup(w, r)
		return
	}
	h.dispatch(w, r)
}

func (h *Handler) dispatch(w http.ResponseWriter, r *http.Request) {
	tenantID, suffix, sub, ok := parsePath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid path, expected /api/tenants/{id}[/subresource]")
		return
	}
	// The SSE alias /api/tenants/{id}/usage/stream MUST run before
	// AdminAuth: EventSource cannot send an Authorization header,
	// so this path enforces its own per-tenant auth via the
	// ?token= query param resolved against TokenStore (see
	// UsageStreamHandler.ServeHTTP). Gating it behind AdminAuth
	// would make the alias non-functional for the SPA — see
	// Config.AdminAuth doc and the legacy /api/v1/usage/stream/
	// path which is registered directly on the mux for the same
	// reason.
	if suffix == "usage" && sub == "stream" {
		if h.sseHandler == nil {
			writeError(w, http.StatusServiceUnavailable, "usage stream not configured")
			return
		}
		h.sseHandler.ServeHTTP(w, r)
		return
	}
	if h.cfg.AdminAuth != nil && !h.cfg.AdminAuth(r) {
		writeError(w, http.StatusUnauthorized, "admin authorization required")
		return
	}
	switch suffix {
	case "":
		if sub != "" {
			writeError(w, http.StatusNotFound, "unknown subresource "+sub)
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.getTenant(w, r, tenantID)
	case "usage":
		// usage/stream is handled before AdminAuth above so
		// EventSource (which cannot send Authorization) can
		// reach the SSE feed; only the synchronous /usage GET
		// is dispatched here.
		if sub != "" {
			writeError(w, http.StatusNotFound, "unknown subresource usage/"+sub)
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.getUsage(w, r, tenantID)
	case "keys":
		switch r.Method {
		case http.MethodPost:
			if sub != "" {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			h.createKey(w, r, tenantID)
		case http.MethodGet:
			if sub != "" {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			h.listKeys(w, r, tenantID)
		case http.MethodDelete:
			if sub == "" {
				writeError(w, http.StatusBadRequest, "access key required")
				return
			}
			h.deleteKey(w, r, tenantID, sub)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "placement":
		if sub != "" {
			writeError(w, http.StatusNotFound, "unknown subresource placement/"+sub)
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.getPlacement(w, r, tenantID)
		case http.MethodPut:
			h.putPlacement(w, r, tenantID)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "buckets":
		switch r.Method {
		case http.MethodGet:
			if sub != "" {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			h.listBuckets(w, r, tenantID)
		case http.MethodPost:
			if sub != "" {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			h.createBucket(w, r, tenantID)
		case http.MethodDelete:
			if sub == "" {
				writeError(w, http.StatusBadRequest, "bucket name required")
				return
			}
			h.deleteBucket(w, r, tenantID, sub)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "dedicated-cells":
		if sub != "" {
			writeError(w, http.StatusNotFound, "unknown subresource dedicated-cells/"+sub)
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.listDedicatedCells(w, r, tenantID)
		case http.MethodPost:
			h.provisionDedicatedCell(w, r, tenantID)
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
	TenantID  string    `json:"tenantId"`
	AccessKey string    `json:"accessKey"`
	SecretKey string    `json:"secretKey"`
	CreatedAt time.Time `json:"createdAt"`
}

// listKeys handles GET /api/tenants/{id}/keys.
func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request, tenantID string) {
	if h.cfg.Tenants == nil {
		writeError(w, http.StatusServiceUnavailable, "tenant store not configured")
		return
	}
	if _, ok := h.cfg.Tenants.LookupTenant(tenantID); !ok {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	lister, ok := h.cfg.Tenants.(APIKeyLister)
	if !ok {
		writeError(w, http.StatusNotImplemented, "tenant store does not support listing keys")
		return
	}
	keys, err := lister.ListAPIKeys(tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list keys: "+err.Error())
		return
	}
	if keys == nil {
		keys = []APIKeyDescriptor{}
	}
	writeJSON(w, http.StatusOK, keys)
}

// deleteKey handles DELETE /api/tenants/{id}/keys/{accessKey}.
func (h *Handler) deleteKey(w http.ResponseWriter, r *http.Request, tenantID, accessKey string) {
	if h.cfg.Tenants == nil {
		writeError(w, http.StatusServiceUnavailable, "tenant store not configured")
		return
	}
	if _, ok := h.cfg.Tenants.LookupTenant(tenantID); !ok {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	lister, ok := h.cfg.Tenants.(APIKeyLister)
	if !ok {
		writeError(w, http.StatusNotImplemented, "tenant store does not support revoking keys")
		return
	}
	if err := lister.DeleteAPIKey(tenantID, accessKey); err != nil {
		writeError(w, http.StatusInternalServerError, "delete key: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listBuckets handles GET /api/tenants/{id}/buckets.
func (h *Handler) listBuckets(w http.ResponseWriter, r *http.Request, tenantID string) {
	if !h.ensureTenantExists(w, tenantID) {
		return
	}
	if h.cfg.Buckets == nil {
		writeJSON(w, http.StatusOK, []BucketDescriptor{})
		return
	}
	buckets, err := h.cfg.Buckets.ListBuckets(r.Context(), tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list buckets: "+err.Error())
		return
	}
	if buckets == nil {
		buckets = []BucketDescriptor{}
	}
	writeJSON(w, http.StatusOK, buckets)
}

// createBucketRequest is the POST body of
// /api/tenants/{id}/buckets.
type createBucketRequest struct {
	Name               string `json:"name"`
	PlacementPolicyRef string `json:"placementPolicyRef"`
}

const maxBucketPayloadBytes int64 = 8 * 1024

// createBucket handles POST /api/tenants/{id}/buckets.
func (h *Handler) createBucket(w http.ResponseWriter, r *http.Request, tenantID string) {
	if !h.ensureTenantExists(w, tenantID) {
		return
	}
	if h.cfg.Buckets == nil {
		writeError(w, http.StatusServiceUnavailable, "bucket store not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBucketPayloadBytes)
	var req createBucketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if _, tooLarge := err.(*http.MaxBytesError); tooLarge {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("bucket payload exceeds %d bytes", maxBucketPayloadBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "decode bucket: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "bucket name is required")
		return
	}
	// parsePath splits DELETE /api/tenants/{id}/buckets/{name} on "/"
	// and Go's net/http decodes %2F to "/" before ServeHTTP sees the
	// path, so a name containing "/" would be creatable but never
	// deletable through the console. Reject the separator (and
	// backslash, for symmetry on Windows-style inputs) up front so
	// the lifecycle stays round-trippable.
	if strings.ContainsAny(req.Name, "/\\") {
		writeError(w, http.StatusBadRequest, "bucket name must not contain '/' or '\\'")
		return
	}
	bucket, err := h.cfg.Buckets.CreateBucket(r.Context(), tenantID, req.Name, req.PlacementPolicyRef)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create bucket: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, bucket)
}

// deleteBucket handles DELETE /api/tenants/{id}/buckets/{name}.
func (h *Handler) deleteBucket(w http.ResponseWriter, r *http.Request, tenantID, name string) {
	if !h.ensureTenantExists(w, tenantID) {
		return
	}
	if h.cfg.Buckets == nil {
		writeError(w, http.StatusServiceUnavailable, "bucket store not configured")
		return
	}
	if err := h.cfg.Buckets.DeleteBucket(r.Context(), tenantID, name); err != nil {
		writeError(w, http.StatusInternalServerError, "delete bucket: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ProvisionDedicatedCellRequest is the JSON body POST /dedicated-cells
// accepts. The tenant_id field is ignored — the URL path tenant
// always wins so a forged body cannot bind a cell to a different
// tenant.
type ProvisionDedicatedCellRequest struct {
	Region            string  `json:"region"`
	Country           string  `json:"country"`
	CapacityPetabytes float64 `json:"capacity_petabytes"`
	ErasureProfile    string  `json:"erasure_profile"`
	NodeCount         int     `json:"node_count"`
}

// maxCellProvisionPayloadBytes caps the request body the dedicated-cell
// provisioning endpoint will decode. The payload is a small operator
// request (region, country, capacity, erasure profile, node count) so
// the cap intentionally matches the bucket-creation cap rather than the
// larger placement-policy cap.
const maxCellProvisionPayloadBytes int64 = 8 * 1024

// provisionDedicatedCell handles POST /api/tenants/{id}/dedicated-cells.
// The endpoint authenticates via the existing AdminAuth gate (the
// signup self-service surface lives under /api/v1/) and delegates
// to the wired CellProvisioner. The response is the operator-facing
// cellops.CellStatus so a tenant or operator can poll for the
// transition from "provisioning" to "active".
func (h *Handler) provisionDedicatedCell(w http.ResponseWriter, r *http.Request, tenantID string) {
	if !h.ensureTenantExists(w, tenantID) {
		return
	}
	if h.cfg.CellProvisioner == nil {
		writeError(w, http.StatusServiceUnavailable, "cell provisioner not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCellProvisionPayloadBytes)
	var body ProvisionDedicatedCellRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if _, tooLarge := err.(*http.MaxBytesError); tooLarge {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("cell provision payload exceeds %d bytes", maxCellProvisionPayloadBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	req := cellops.CellRequest{
		TenantID:          tenantID,
		Region:            strings.TrimSpace(body.Region),
		Country:           strings.TrimSpace(body.Country),
		CapacityPetabytes: body.CapacityPetabytes,
		ErasureProfile:    strings.TrimSpace(body.ErasureProfile),
		NodeCount:         body.NodeCount,
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status, err := h.cfg.CellProvisioner.ProvisionCell(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "provision cell: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

// listDedicatedCells handles GET /api/tenants/{id}/dedicated-cells.
func (h *Handler) listDedicatedCells(w http.ResponseWriter, r *http.Request, tenantID string) {
	if !h.ensureTenantExists(w, tenantID) {
		return
	}
	if h.cfg.Cells == nil {
		writeJSON(w, http.StatusOK, []DedicatedCellDescriptor{})
		return
	}
	cells, err := h.cfg.Cells.ListDedicatedCells(r.Context(), tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list dedicated cells: "+err.Error())
		return
	}
	if cells == nil {
		cells = []DedicatedCellDescriptor{}
	}
	writeJSON(w, http.StatusOK, cells)
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

// ensureTenantExists is the shared 404 guard for tenant-scoped
// handlers. Every /api/tenants/{id}/... route bottoms out in a store
// that would otherwise silently accept data for a tenant the gateway
// never minted — orphaning buckets, keys, or placement rows. Centralise
// the lookup so callers only have to branch on a single bool and all
// routes stay consistent with createKey/listKeys/deleteKey which
// already enforce this.
func (h *Handler) ensureTenantExists(w http.ResponseWriter, tenantID string) bool {
	if h.cfg.Tenants == nil {
		writeError(w, http.StatusServiceUnavailable, "tenant store not configured")
		return false
	}
	if _, ok := h.cfg.Tenants.LookupTenant(tenantID); !ok {
		writeError(w, http.StatusNotFound, "tenant not found")
		return false
	}
	return true
}

// parsePath splits /api/tenants/{id}[/suffix[/sub]] into
// (id, suffix, sub, ok). `sub` is the single segment after suffix
// (e.g. access key for keys/{accessKey}, bucket name for
// buckets/{name}); trailing segments beyond sub are rejected so a
// confused client does not walk into a partially-matched route.
func parsePath(p string) (tenantID, suffix, sub string, ok bool) {
	const prefix = "/api/tenants/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(p, prefix)
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", "", "", false
	}
	tenantID = parts[0]
	if len(parts) == 1 {
		return tenantID, "", "", true
	}
	suffix = parts[1]
	if len(parts) == 2 {
		return tenantID, suffix, "", true
	}
	if len(parts) == 3 {
		return tenantID, suffix, parts[2], true
	}
	return "", "", "", false
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
