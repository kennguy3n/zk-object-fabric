// Package s3compat is the S3-compatible HTTP handler surface for the
// Linode-hosted ZK Gateway. See docs/PROPOSAL.md §3.1.
//
// Phase 2 wires the HTTP surface to the control-plane ManifestStore
// and the data-plane StorageProvider registry. Request parsing is
// path-style (/{bucket}/{key...}); authentication is still a stub
// (see Authenticator) and multipart, versioning, and cache promotion
// are tracked as their own Phase 2 gates.
package s3compat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
	"github.com/kennguy3n/zk-object-fabric/internal/requestid"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/pieceintegrity"
	"github.com/kennguy3n/zk-object-fabric/migration/lazy_read_repair"
	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/wasabi"
	"github.com/zeebo/blake3"
	"golang.org/x/sync/semaphore"
)

// Authenticator verifies the identity claimed by an S3 request and
// returns the tenant ID bound to it. A nil Authenticator causes the
// handler to fall back to AnonymousTenant (used only in dev).
type Authenticator interface {
	Authenticate(r *http.Request) (tenantID string, err error)
}

// TenantResolver is an optional capability an Authenticator may
// implement to resolve the tenant a request's credentials *claim*
// WITHOUT verifying the SigV4 signature. It exists for the CORS
// preflight (OPTIONS): a browser never signs a preflight, so
// Authenticate always fails on one — yet a presigned-URL preflight
// still carries X-Amz-Credential in the query string, which names the
// tenant whose bucket CORS rules apply. The browser sends the
// preflight to the same URL (query string included) as the follow-up
// actual request, so the credential is present.
//
// Resolving a tenant here grants no access: the preflight only tells
// the browser whether it may attempt the cross-origin request; the
// follow-up actual request is fully authenticated as usual. ok is
// false when the request carries no recognisable access key.
//
// This capability is also what lets applyCORS attach CORS headers to
// an actual request that fails authentication, so a browser SPA reads
// the real error instead of an opaque CORS failure. An Authenticator
// that does NOT implement TenantResolver fails closed: preflights get
// 403 and auth-failure responses carry no CORS headers. A third-party
// Authenticator therefore must implement TenantResolver for CORS to
// work on presigned-URL flows. The production HMACAuthenticator does.
type TenantResolver interface {
	ResolveTenantUnverified(r *http.Request) (tenantID string, ok bool)
}

// AnonymousTenant is the tenant ID the handler uses when no
// Authenticator is configured. Deployments MUST configure an
// Authenticator in production.
const AnonymousTenant = "anonymous"

// ErrAuthMisconfigured is the sentinel authenticate returns when
// RequireAuth is true and the handler has no Authenticator wired.
// Call sites translate it into a 500 InternalAuthMisconfigured S3
// error via writeAuthError so the production-mode safety net
// surfaces as an operator-visible failure instead of a silent
// anonymous-tenant fallthrough.
var ErrAuthMisconfigured = errors.New("s3compat: authenticator not configured but RequireAuth=true")

// MaxInMemoryObjectBytes caps the size of an object the handler is
// willing to buffer into memory on the request goroutine. The
// remaining non-streaming paths that have to honour this ceiling are:
//
//   - The gateway-encrypted Range GET path
//     (bufferedGatewayDecryptedGet). Slicing an arbitrary byte
//     range out of a chunk-framed plaintext requires the whole
//     plaintext in memory; chunk-level range seek lands in v0.2.0.
//     Non-range gateway-encrypted GETs use the streaming path
//     (streamGatewayDecryptedGet) and have NO ceiling.
//
//   - The inline cache-warming path in fetchPiece, which buffers a
//     piece into memory so it can hand the bytes both to the
//     hot-object cache and to the client.
//
// Above the cap the buffered Range GET returns 507
// InsufficientStorage with a descriptive message that points the
// caller at the unbounded streaming full-object GET path; the
// cache-warming path skips inline warming and publishes a
// PromotionSignal so the async worker can decide whether to warm
// the piece. Both ceilings match the multipart in-memory pre-fetch
// cap (maxMultipartInMemoryBytes) so operators have a single knob
// to reason about.
//
// The constant lives here rather than in config so tests can
// reference it without round-tripping through a Config field; a
// follow-up commit can promote it to a tunable when chunk-level
// range seek and streaming EC land.
const MaxInMemoryObjectBytes int64 = 256 * 1024 * 1024

// PlacementEngine chooses the storage backend for a new object. Phase
// 2 supplies a concrete implementation; the handler treats the engine
// as a black box that resolves (tenant, bucket, key) to a backend
// name registered in the StorageProvider registry.
type PlacementEngine interface {
	ResolveBackend(tenantID, bucket, objectKey string) (string, metadata.PlacementPolicy, error)
}

// BillingSink receives UsageEvent emissions from the handler. The
// billing pipeline aggregates them into Counter rows. A nil sink
// disables metering (used in tests).
type BillingSink interface {
	Emit(event billing.UsageEvent)
}

// IntegrityFailureSink receives observability events from the
// piece-integrity verifier. cmd/gateway wires this to
// internal/metrics so operators can alert on either channel
// without api/s3compat importing internal/metrics directly.
//
// Two channels — semantically distinct — both fire from
// fetchPiece / getMultipart / getErasureCoded:
//
//   - Inc(backend) fires when the bytes the backend returned
//     failed to match a recognised integrity claim. This is a
//     hard-fail signal: the gateway has refused to serve the
//     piece, returned 502 IntegrityCheckFailed, and is shouting
//     for an operator. Any sustained non-zero rate here is a
//     bit-rot / tampering / wrong-hash bug.
//
//   - IncUnrecognized(backend) fires when the manifest's hash is
//     present but not in any recognised format (e.g. a legacy
//     multipart / copy / dedup manifest still has the backend's
//     opaque ETag in Hash). The bytes were served because we
//     cannot prove anything about them, but the verifier wants
//     operators to see the count so they can plan / drive a
//     one-shot rewrite to populate ProviderETag + clear Hash.
//     A steady non-zero rate here is a migration TODO, not an
//     incident.
type IntegrityFailureSink interface {
	Inc(backend string)
	IncUnrecognized(backend string)
}

// Config collects the dependencies Handler needs.
type Config struct {
	// Manifests is the encrypted-manifest store (Phase 2: Postgres).
	Manifests manifest_store.ManifestStore

	// Providers is the backend registry keyed by provider name
	// (e.g. "wasabi", "ceph_rgw").
	Providers map[string]providers.StorageProvider

	// Placement resolves a new object to a backend. Required for PUT.
	Placement PlacementEngine

	// Auth verifies the requester's tenant binding. Optional; nil
	// means AnonymousTenant.
	Auth Authenticator

	// VerifiedCheck, when non-nil, is consulted on PUT to gate
	// the tenant's first upload on email verification. It returns
	// (verified, tracked): tracked=false means the tenant was
	// not created via the B2C console signup flow (e.g. an HMAC
	// binding loaded from a JSON file) and the gate is skipped;
	// tracked=true with verified=false makes Put return 403 with
	// an EmailNotVerified error code so the SPA can prompt the
	// user to click the verification link before retrying.
	VerifiedCheck func(tenantID string) (verified, tracked bool)

	// Billing receives usage events. Optional.
	Billing BillingSink

	// Notifications receives object-level events (PUT / DELETE /
	// COPY / CompleteMultipartUpload) for asynchronous fan-out to
	// each bucket's configured webhook destinations (WS8.6). The
	// handler calls Emit on the success path only; Emit must be
	// non-blocking so a slow destination never adds latency to the
	// S3 request. Optional; nil disables event notifications (the
	// bucket's configuration is still stored, just never delivered).
	Notifications NotificationEmitter

	// Multipart is the server-side multipart-upload session store.
	// Required for CreateMultipartUpload / UploadPart /
	// CompleteMultipartUpload / AbortMultipartUpload. A nil store
	// causes those endpoints to return 501 NotImplemented.
	Multipart multipart.Store

	// BucketConfig persists per-bucket S3 configuration
	// sub-resources (today: versioning state — WS8.4). A nil store
	// makes PutBucketVersioning / GetBucketVersioning return 501
	// NotImplemented and leaves DeleteObject on its non-versioned
	// (permanent-delete) path.
	BucketConfig bucket_config.Store

	// ErasureCoding is the registry of erasure-coding profiles the
	// handler can use when a placement policy names an
	// ErasureProfile. A nil registry disables EC: writes that ask
	// for it surface an InvalidPlacement error so the misconfig is
	// caught at PUT time rather than silently demoted to a
	// single-piece write.
	ErasureCoding *erasure_coding.Registry

	// Encryption configures gateway-side encryption for "managed"
	// and "public_distribution" modes. Required when any tenant
	// policy uses those modes; a nil value causes managed-mode
	// PUTs to fail with EncryptionNotConfigured rather than
	// silently storing plaintext.
	Encryption *GatewayEncryption

	// Cache is the L0/L1 hot object cache consulted on the GET path.
	// Optional; nil disables caching.
	Cache hot_object_cache.HotObjectCache

	// CachePublisher, when non-nil, receives PromotionSignals on
	// every cache miss so the promotion worker can decide what to
	// warm. A *hot_object_cache.SignalBus satisfies this. Optional.
	CachePublisher hot_object_cache.SignalPublisher

	// ReadRepair, when non-nil, is consulted on the GET path when
	// the primary backend cannot serve a piece and the manifest's
	// MigrationState indicates a migration is in progress
	// (Generation > 1). It fetches the piece from the secondary
	// backend, copies it to the new primary, and returns the bytes
	// for the handler to serve. Optional; nil disables the
	// read-repair fallback and backend GET errors surface as 502.
	ReadRepair *lazy_read_repair.ReadRepair

	// NodeID identifies the gateway node emitting billing events.
	NodeID string

	// RequireAuth, when true, makes the handler refuse every
	// request with 500 InternalAuthMisconfigured if Auth is
	// nil — instead of silently serving every request under
	// AnonymousTenant. cmd/gateway sets this whenever the
	// deployment env is "production" so a misconfiguration that
	// drops the authenticator can never silently grant
	// world-write to the anonymous tenant.
	RequireAuth bool

	// ContentIndex is the intra-tenant deduplication content
	// index. When non-nil and the resolved placement policy has
	// DedupPolicy.Enabled, the PUT path looks up
	// (tenant_id, content_hash) and either bumps the refcount on
	// an existing piece or registers a new entry. The DELETE
	// path mirrors the lookup so deletes that hit a deduped
	// object decrement the refcount instead of removing the
	// piece. A nil store makes every PUT a fresh write
	// regardless of policy. See docs/PROPOSAL.md §3.14.
	ContentIndex content_index.Store

	// Compliance hooks the gateway up to the data-residency
	// pre-flight check and audit trail. Both fields are optional;
	// when nil the gateway behaves as it did before Phase 4.
	Compliance ComplianceHooks

	// IntegrityFailures, when non-nil, is invoked once per piece
	// the handler observed with a mismatched content hash. The
	// handler also logs the failure at ERROR level and refuses to
	// cache or serve the bad bytes regardless of whether a sink
	// is wired. Hooked from cmd/gateway/main.go to
	// internal/metrics's IncIntegrityFailure so operators can
	// alert on the rate. Optional.
	IntegrityFailures IntegrityFailureSink

	// MaxRequestBytes caps the size of the request body the
	// gateway is willing to consume on PUT (object write,
	// CompleteMultipartUpload XML, dedup-aware POST) and
	// UploadPart. When > 0 the handler wraps r.Body in an
	// http.MaxBytesReader before any io.ReadAll / TeeReader
	// touches it, so an oversized stream surfaces a 413
	// RequestEntityTooLarge instead of running OOM kills or
	// silently truncating. A zero value disables the cap (the
	// pre-PR-10 behaviour). cmd/gateway populates this from
	// GatewayConfig.MaxRequestBytes; the package default in
	// internal/config is 5 GiB.
	MaxRequestBytes int64

	// Now, if set, returns the current time. Tests override it to
	// make manifests deterministic.
	Now func() time.Time

	// CacheWarmingMemoryBudget caps total bytes the handler will
	// buffer simultaneously across all in-flight cache-warming
	// operations. Zero (default) applies DefaultCacheWarmingBudget;
	// a negative value disables the budget guard (the pre-PR-7
	// behaviour, intended only for regression tests). See the
	// matching field on internal/config.GatewayConfig for the
	// operator-facing tunable.
	CacheWarmingMemoryBudget int64

	// OnCacheWarmingBudgetExhausted, when non-nil, is invoked
	// with the piece backend and size every time fetchPiece would
	// have warmed the cache inline but the budget guard rejected
	// the acquire. The backend label lets operators correlate
	// budget pressure with specific providers — the same
	// labelling convention as zkof_integrity_failure_total — so a
	// chronically misbehaving backend stands out in the rate
	// breakdown instead of being averaged into the fleet-wide
	// counter. Production wires this into a labelled Prometheus
	// counter. The hook runs on the request goroutine and must be
	// cheap (atomic Add, no blocking calls).
	OnCacheWarmingBudgetExhausted func(backend string, pieceSize int64)
}

// DefaultCacheWarmingBudget is the default total memory budget
// the handler allocates to in-flight cache-warming buffers when
// Config.CacheWarmingMemoryBudget is zero. 512 MiB is a balance
// between letting bursty cache-miss traffic warm the cache
// effectively and bounding the resident-set growth on a small
// fleet of gateway pods.
const DefaultCacheWarmingBudget int64 = 512 * 1024 * 1024

// ComplianceHooks is the optional residency / audit surface
// supplied by cmd/gateway. The handler depends only on minimal
// interfaces so the compliance package does not have to import
// api/s3compat (avoiding an import cycle).
type ComplianceHooks struct {
	// Residency is the pre-flight check called from the PUT path.
	// nil disables the check.
	Residency ResidencyChecker

	// Audit, when non-nil, receives one Record per PUT / DELETE /
	// GET that the handler successfully services.
	Audit AuditRecorder

	// LegalHoldStore, when non-nil, is consulted on the DELETE
	// path to block deletion of objects under an active legal hold.
	LegalHoldStore LegalHoldChecker
}

// ResidencyChecker matches *compliance.ResidencyEnforcer.Check.
type ResidencyChecker interface {
	Check(tenantID, backendCountry string, policyResidency []string) error
}

// AuditRecorder is the subset of compliance.AuditStore the
// handler uses on the hot path.
type AuditRecorder interface {
	Record(ctx context.Context, entry AuditEntry) error
}

// LegalHoldChecker is the subset of auth.LegalHoldStore the
// DELETE path consults to block deletion of held objects.
type LegalHoldChecker interface {
	Active(ctx context.Context, tenantID, bucket, objectKey string) ([]LegalHoldEntry, error)
}

// NotificationEmitter receives one ObjectEvent per successfully
// serviced object mutation, for asynchronous fan-out to the bucket's
// configured webhook destinations (WS8.6). Emit must be non-blocking:
// the implementation (internal/notifications.Dispatcher) enqueues the
// event and returns immediately, so a slow or unreachable destination
// can never add latency to — or fail — the originating S3 request.
// The handler depends only on this minimal interface so the s3compat
// package does not import internal/notifications (cmd/gateway adapts
// the two via a thin shim, mirroring the audit wiring).
type NotificationEmitter interface {
	Emit(event ObjectEvent)
}

// ObjectEvent mirrors the fields of notifications.Event that the
// handler can populate from the request path. Defined here as a
// shape-compatible struct so the s3compat package does not import
// internal/notifications directly. EventName carries the specific
// leaf S3 event name (e.g. "s3:ObjectCreated:Put"); the dispatcher
// matches it against each rule's subscribed events, including the
// wildcard classes.
type ObjectEvent struct {
	TenantID  string
	Bucket    string
	ObjectKey string
	EventName string
	SizeBytes int64
	ETag      string
	VersionID string
	RequestID string
	SourceIP  string
}

// Leaf S3 event names the handler emits. They are kept as plain string
// constants (rather than importing metadata/notification) so the
// s3compat package stays free of that dependency; cmd/gateway's shim
// converts them to notification.EventType. They must stay in sync with
// the leaf constants in metadata/notification.
const (
	eventObjectCreatedPut          = "s3:ObjectCreated:Put"
	eventObjectCreatedCopy         = "s3:ObjectCreated:Copy"
	eventObjectCreatedCompleteMPU  = "s3:ObjectCreated:CompleteMultipartUpload"
	eventObjectRemovedDelete       = "s3:ObjectRemoved:Delete"
	eventObjectRemovedDeleteMarker = "s3:ObjectRemoved:DeleteMarkerCreated"
)

// LegalHoldEntry mirrors auth.LegalHold so the s3compat package
// does not import internal/auth directly.
type LegalHoldEntry struct {
	ID string
}

// AuditEntry mirrors compliance.AuditEntry. Defined here as a
// shape-compatible struct so the s3compat package does not have
// to import internal/compliance directly. cmd/gateway adapts the
// two via a thin shim.
type AuditEntry struct {
	TenantID       string
	Operation      string
	Bucket         string
	ObjectKey      string
	PieceID        string
	PieceBackend   string
	BackendCountry string
	Timestamp      time.Time
	RequestID      string
}

// Handler routes S3-compatible requests to the gateway's internal
// pipeline.
type Handler struct {
	cfg Config

	// cacheWarmSem bounds the total bytes the handler is willing
	// to buffer simultaneously across all in-flight
	// fetchPiece-warming operations. The semaphore is initialised
	// to cfg.CacheWarmingMemoryBudget (or DefaultCacheWarmingBudget
	// when that field is zero). A negative budget leaves the
	// field nil — the pre-PR-7 unbounded behaviour, used only by
	// regression tests that exercise the old code path.
	cacheWarmSem *semaphore.Weighted

	// cacheWarmBudget remembers the resolved budget so fetchPiece
	// can short-circuit pieces larger than the entire budget
	// (which would never succeed at TryAcquire) and skip straight
	// to the promotion signal path.
	cacheWarmBudget int64
}

// New returns a Handler ready to be wired into an HTTP mux.
//
// CacheWarmingMemoryBudget resolves into one of three modes:
//
//   - budget == 0  → DefaultCacheWarmingBudget (the production
//     default; the typical case for a Handler constructed via
//     internal/config's Default()).
//   - budget > 0   → honoured verbatim as the semaphore weight.
//   - budget < 0   → guard disabled (cacheWarmSem stays nil).
//     This is the pre-PR-7 unbounded path and is intended only
//     for regression tests that exercise the legacy behaviour.
//
// The flow is written as plain if/else (no fallthrough) so the
// three branches read top-to-bottom: an idiomatic Go reviewer
// should not need to remember switch-fallthrough semantics to
// verify that "zero falls back to default and then initialises".
func New(cfg Config) *Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	h := &Handler{cfg: cfg}
	budget := cfg.CacheWarmingMemoryBudget
	if budget == 0 {
		budget = DefaultCacheWarmingBudget
	}
	if budget > 0 {
		h.cacheWarmSem = semaphore.NewWeighted(budget)
		h.cacheWarmBudget = budget
	}
	// budget < 0 leaves cacheWarmSem == nil → unbounded path.
	return h
}

// Register attaches the S3-compatible routes to mux. Route parsing
// follows S3 path-style addressing (/{bucket}/{key...}).
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/", h.dispatch)
}

// capRequestBody installs the MaxRequestBytes cap on r.Body when
// the request carries a body and the handler has been configured
// with a non-zero cap. Returns true when the wrap was applied,
// false when it was skipped (cap disabled, or r.Body is nil /
// http.NoBody).
//
// The wrap is method-agnostic: GET and HEAD never have a body
// (net/http guarantees r.Body == http.NoBody for them, which the
// guard below filters out), so the wrap only fires on methods
// that actually carry bytes. DELETE in well-behaved S3 clients
// carries no body, but a misbehaving client that sends one will
// have its body capped instead of silently consumed by an
// unsuspecting future handler — defence in depth.
//
// Method-specific allowlists were tempting (just PUT/POST,
// because those are the only methods that legitimately read the
// body today) but they create a future trap: any handler that
// starts consuming the body on a method outside the allowlist
// would silently bypass the cap and reintroduce the
// OOM/truncation hazard MaxRequestBytes exists to prevent.
// Wrapping every non-empty body means a future contributor
// adding (say) a DELETE-with-XML extension does not have to
// remember to update this dispatch.
//
// CopyObject is a PUT with `x-amz-copy-source` set and no client
// body — the source bytes are streamed from the source backend.
// The wrap is a no-op for those requests because r.Body is
// http.NoBody when no Content-Length / Transfer-Encoding was
// sent.
//
// The function is factored out of dispatch so tests can verify
// the wrap is installed without going through the full handler
// chain (which would require a body-reading sub-handler to
// surface the 413 — the current Delete handler does not read
// r.Body, so the cap is structurally present but inert at the
// response layer for DELETE today).
func (h *Handler) capRequestBody(w http.ResponseWriter, r *http.Request) bool {
	if h.cfg.MaxRequestBytes <= 0 {
		return false
	}
	if r.Body == nil || r.Body == http.NoBody {
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxRequestBytes)
	return true
}

// unsupportedSubresources lists S3 sub-resource query keys (the part
// after `?`) that the gateway does not implement yet. When a request
// carries one of these keys, the dispatcher rejects it with
// 501 NotImplemented + the canonical AWS error code (so SDKs surface
// it cleanly) BEFORE routing — otherwise a request like
// `PUT /bucket/key?acl` would fall through to the regular PUT handler
// and silently overwrite the object body with the caller's ACL XML.
//
// The map value is the S3 error code AWS uses for the same operation,
// so an SDK that receives the error sees a familiar surface
// (NotImplemented + Resource=request path) and can report it as a
// gap rather than as a generic 5xx. Operation names map 1:1 to the
// AWS API:
//
//	acl                ACL operations (GetObjectAcl, PutObjectAcl,
//	                   GetBucketAcl, PutBucketAcl)
//	lifecycle          Bucket lifecycle configuration
//	policy             Bucket policy document
//	logging            Bucket logging configuration
//	replication        Cross-region replication configuration
//	accelerate         Transfer-acceleration toggle
//	requestPayment     Requester-pays configuration
//	website            Static-website hosting configuration
//	inventory          Bucket inventory configuration
//	metrics            Bucket metrics configuration
//	analytics          Bucket analytics configuration
//	intelligent-tiering Auto-tiering configuration
//	publicAccessBlock  Block-public-access settings
//	ownershipControls  Object Ownership settings
//
// The conformance harness in `tests/s3_conformance` asserts every
// entry here returns 4xx (specifically 501); a future implementation
// that wires up a sub-resource removes its key from this map and adds
// routing in the dispatch switch below. Object tagging (WS8.1) and
// bucket versioning (WS8.4) both followed exactly this path: their
// keys (`tagging`, `versioning`) were removed here and `?tagging` /
// `?versioning` routing was added to the dispatch switch. Object
// Lock (WS8.3) followed the same path for `object-lock`, `retention`,
// and `legal-hold`, bucket CORS (WS8.5) for `cors`, bucket
// lifecycle (WS8.2) for `lifecycle`, bucket event notifications
// (WS8.6) for `notification`, and bucket default encryption
// (WS8.7) for `encryption`.
//
// Rejection is method-agnostic: the moment a sub-resource key is in
// this map, requests for that key are refused regardless of HTTP
// method. This is the right semantics for the current state of the
// gateway because every entry here is unsupported across ALL methods
// (e.g. neither GET ?acl nor PUT ?acl is implemented). If partial
// per-method support is ever added — say, GET ?acl works but PUT
// ?acl does not — the corresponding key MUST be removed from this
// map entirely (which unblocks BOTH methods) and the method-specific
// rejection moved into the relevant dispatch arm (in this example,
// the PUT arm would emit 501 for ?acl and the GET arm would handle
// it). Leaving a key in this map while wiring up one method would
// produce a confusing 501-on-GET response after the GET handler
// exists, because rejectUnsupportedSubresource runs before the
// per-method dispatch and would never give the GET handler a chance
// to run.
//
// `delete` is intentionally NOT in this map: it is the
// POST DeleteObjects (bulk delete) endpoint, which the dispatcher's
// POST arm routes explicitly. Adding `delete` here would
// incorrectly reject bulk-delete requests as unsupported.
var unsupportedSubresources = map[string]string{
	"acl":                 "NotImplemented",
	"policy":              "NotImplemented",
	"logging":             "NotImplemented",
	"replication":         "NotImplemented",
	"accelerate":          "NotImplemented",
	"requestPayment":      "NotImplemented",
	"website":             "NotImplemented",
	"inventory":           "NotImplemented",
	"metrics":             "NotImplemented",
	"analytics":           "NotImplemented",
	"publicAccessBlock":   "NotImplemented",
	"ownershipControls":   "NotImplemented",
	"intelligent-tiering": "NotImplemented",
}

// unsupportedSubresourceKeys is the lexicographically-sorted view of
// unsupportedSubresources's keys. We precompute it once at package
// init so the per-request rejection path can iterate in a stable
// order. Without this, `for key := range unsupportedSubresources`
// picks whichever key Go's randomised map iteration hits first,
// which makes error messages non-deterministic when a request
// carries multiple unsupported keys (e.g. `?acl&policy`). Stable
// ordering also lets the conformance harness snapshot error bodies.
var unsupportedSubresourceKeys = func() []string {
	out := make([]string, 0, len(unsupportedSubresources))
	for k := range unsupportedSubresources {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}()

// rejectUnsupportedSubresource returns true (and emits a 501 response)
// if the request carries any sub-resource we have not implemented.
// The check is intentionally before authentication: a 501 is more
// useful to the SDK than a 403, and we never inspect request body
// or headers beyond the URL when deciding. When a request carries
// multiple unsupported sub-resource keys, the error names the
// lexicographically-first matching key so the response body is
// deterministic for snapshot testing.
//
// The check is also method-agnostic: it fires for any HTTP method
// against the listed sub-resources. See the doc on
// unsupportedSubresources above for the trap this creates if partial
// per-method support is ever wired up for one of the listed keys.
func (h *Handler) rejectUnsupportedSubresource(w http.ResponseWriter, r *http.Request, q url.Values) bool {
	for _, key := range unsupportedSubresourceKeys {
		if !q.Has(key) {
			continue
		}
		writeError(w, http.StatusNotImplemented, unsupportedSubresources[key],
			fmt.Sprintf("the %q sub-resource is not implemented by this gateway", key),
			r.URL.Path)
		return true
	}
	return false
}

func (h *Handler) dispatch(w http.ResponseWriter, r *http.Request) {
	// Attach a per-request auth memo so the at-most-two authenticate
	// calls a cross-origin request makes (applyCORS, then the operation
	// handler) resolve the tenant once instead of recomputing the
	// SigV4 HMAC twice. Scoped to this request: a fresh cell per
	// dispatch, read sequentially within the single request goroutine.
	r = withAuthMemo(r)
	q := r.URL.Query()
	h.capRequestBody(w, r)
	// An OPTIONS request is a CORS preflight (WS8.5); answer it before
	// the unsupported-subresource check and the method switch, since it
	// is unauthenticated and never carries a real S3 operation.
	if r.Method == http.MethodOptions {
		h.handleCORSPreflight(w, r)
		return
	}
	// Attach cross-origin response headers (WS8.5) before routing so
	// they are present on the actual request's response, including
	// error responses (e.g. the 501 from an unsupported sub-resource —
	// otherwise a browser would surface a CORS error instead of the
	// real status). No-op when the request carries no Origin.
	h.applyCORS(w, r)
	if h.rejectUnsupportedSubresource(w, r, q) {
		return
	}
	switch r.Method {
	case http.MethodPut:
		if q.Get("uploadId") != "" && q.Get("partNumber") != "" {
			h.UploadPart(w, r)
			return
		}
		// Object tagging (?tagging) — WS8.1. Checked before the
		// implicit-CreateBucket and CopyObject branches because it is
		// a distinct sub-resource operation, not an object write.
		if q.Has("tagging") {
			h.PutObjectTagging(w, r)
			return
		}
		// Bucket versioning config (PUT /{bucket}?versioning) is a
		// bucket-level sub-resource and must route before the
		// implicit-CreateBucket branch below.
		if q.Has("versioning") {
			h.PutBucketVersioning(w, r)
			return
		}
		// Object Lock sub-resources (WS8.3) route before the
		// implicit-CreateBucket / CopyObject / Put branches because
		// they are distinct config operations, not object writes.
		// ?object-lock is bucket-level; ?retention and ?legal-hold are
		// object-level (their handlers validate the path).
		if q.Has("object-lock") {
			h.PutObjectLockConfiguration(w, r)
			return
		}
		// Bucket CORS config (PUT /{bucket}?cors) — WS8.5. Bucket-level
		// sub-resource; must route before the implicit-CreateBucket /
		// CopyObject / Put branches.
		if q.Has("cors") {
			h.PutBucketCors(w, r)
			return
		}
		// Bucket lifecycle config (PUT /{bucket}?lifecycle) — WS8.2.
		// Bucket-level sub-resource; route before the
		// implicit-CreateBucket / CopyObject / Put branches.
		if q.Has("lifecycle") {
			h.PutBucketLifecycleConfiguration(w, r)
			return
		}
		// Bucket event notifications (PUT /{bucket}?notification) —
		// WS8.6. Bucket-level sub-resource; route before the
		// implicit-CreateBucket / CopyObject / Put branches.
		if q.Has("notification") {
			h.PutBucketNotificationConfiguration(w, r)
			return
		}
		// Bucket default encryption (PUT /{bucket}?encryption) — WS8.7.
		// Bucket-level sub-resource; route before the
		// implicit-CreateBucket / CopyObject / Put branches.
		if q.Has("encryption") {
			h.PutBucketEncryption(w, r)
			return
		}
		if q.Has("retention") {
			h.PutObjectRetention(w, r)
			return
		}
		if q.Has("legal-hold") {
			h.PutObjectLegalHold(w, r)
			return
		}
		// Bucket-level PUT (s3 mb / CreateBucket). Buckets in this
		// gateway are implicit — they come into existence the first
		// time an object is written to them — so CreateBucket is a
		// no-op that just returns 200 OK with a Location header so
		// standard S3 clients like the AWS CLI see a successful
		// response.
		if _, key := parseBucketKey(r.URL.Path); key == "" {
			h.PutBucket(w, r)
			return
		}
		// CopyObject is a PUT with x-amz-copy-source set. The
		// header carries the source /{bucket}/{key} so the
		// destination request body is empty.
		if r.Header.Get("x-amz-copy-source") != "" {
			h.Copy(w, r)
			return
		}
		h.Put(w, r)
	case http.MethodPost:
		if q.Has("uploads") {
			h.CreateMultipartUpload(w, r)
			return
		}
		if q.Get("uploadId") != "" {
			h.CompleteMultipartUpload(w, r)
			return
		}
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported POST operation", r.URL.Path)
	case http.MethodGet:
		bucket, key := parseBucketKey(r.URL.Path)
		// Object tagging (?tagging) — WS8.1.
		if q.Has("tagging") {
			h.GetObjectTagging(w, r)
			return
		}
		// Object Lock sub-resources (WS8.3). ?object-lock is
		// bucket-level (guard key=="" so GET /{bucket}/{key}?object-lock
		// falls through to the object GET); ?retention and ?legal-hold
		// are object-level.
		if key == "" && q.Has("object-lock") {
			h.GetObjectLockConfiguration(w, r, bucket)
			return
		}
		// Bucket CORS config (GET /{bucket}?cors) — WS8.5. Guard on
		// key=="" so GET /{bucket}/{key}?cors falls through to the
		// object GET rather than returning the bucket CORS document.
		if key == "" && q.Has("cors") {
			h.GetBucketCors(w, r, bucket)
			return
		}
		// Bucket lifecycle config (GET /{bucket}?lifecycle) — WS8.2.
		// Guard on key=="" so GET /{bucket}/{key}?lifecycle falls
		// through to the object GET.
		if key == "" && q.Has("lifecycle") {
			h.GetBucketLifecycleConfiguration(w, r, bucket)
			return
		}
		// Bucket event notifications (GET /{bucket}?notification) —
		// WS8.6. Guard on key=="" so GET /{bucket}/{key}?notification
		// falls through to the object GET.
		if key == "" && q.Has("notification") {
			h.GetBucketNotificationConfiguration(w, r, bucket)
			return
		}
		// Bucket default encryption (GET /{bucket}?encryption) — WS8.7.
		// Guard on key=="" so GET /{bucket}/{key}?encryption falls
		// through to the object GET.
		if key == "" && q.Has("encryption") {
			h.GetBucketEncryption(w, r, bucket)
			return
		}
		if key != "" && q.Has("retention") {
			h.GetObjectRetention(w, r)
			return
		}
		if key != "" && q.Has("legal-hold") {
			h.GetObjectLegalHold(w, r)
			return
		}
		if key == "" && q.Has("uploads") {
			h.ListMultipartUploads(w, r, bucket)
			return
		}
		// Bucket versioning config (GET /{bucket}?versioning). Guard
		// on key=="" like the other bucket-level sub-resources so
		// GET /{bucket}/{key}?versioning falls through to the object
		// GET rather than returning the bucket versioning document.
		if key == "" && q.Has("versioning") {
			h.GetBucketVersioning(w, r, bucket)
			return
		}
		// ListObjectVersions is bucket-level GET ?versions.
		if key == "" && q.Has("versions") {
			h.ListObjectVersions(w, r, bucket)
			return
		}
		// LIST is a bucket-level GET (no key, or ?list-type=2).
		if key == "" || q.Has("list-type") {
			h.listBucket(w, r, bucket)
			return
		}
		h.Get(w, r)
	case http.MethodHead:
		// Bucket-level HEAD (HeadBucket) is used by S3 clients to
		// probe existence. Buckets are implicit, so authenticate the
		// caller and return 200 OK.
		if _, key := parseBucketKey(r.URL.Path); key == "" {
			h.HeadBucket(w, r)
			return
		}
		h.Head(w, r)
	case http.MethodDelete:
		if q.Get("uploadId") != "" {
			h.AbortMultipartUpload(w, r)
			return
		}
		// Object tagging (?tagging) — WS8.1.
		if q.Has("tagging") {
			h.DeleteObjectTagging(w, r)
			return
		}
		// Bucket CORS config (DELETE /{bucket}?cors) — WS8.5.
		if q.Has("cors") {
			h.DeleteBucketCors(w, r)
			return
		}
		// Bucket lifecycle config (DELETE /{bucket}?lifecycle) — WS8.2.
		if q.Has("lifecycle") {
			h.DeleteBucketLifecycleConfiguration(w, r)
			return
		}
		// Bucket default encryption (DELETE /{bucket}?encryption) — WS8.7.
		if q.Has("encryption") {
			h.DeleteBucketEncryption(w, r)
			return
		}
		// Bucket event notifications (DELETE /{bucket}?notification) —
		// WS8.6. S3 has no DeleteBucketNotification operation; a
		// configuration is cleared by PUTting an empty
		// <NotificationConfiguration/>. Intercept the bucket-level case
		// (key=="") so it returns this explicit guidance instead of
		// falling through to h.Delete, which would reject it with the
		// generic "path must be /{bucket}/{key...}" 400.
		if bucket, key := parseBucketKey(r.URL.Path); key == "" && q.Has("notification") {
			writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed",
				"S3 has no DeleteBucketNotification; clear the configuration by PUT /"+bucket+"?notification with an empty <NotificationConfiguration/>",
				r.URL.Path)
			return
		}
		h.Delete(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed", r.URL.Path)
	}
}

// PutBucket is a no-op CreateBucket handler. Buckets in this gateway
// are implicit — they come into existence the first time an object is
// written — so CreateBucket exists only so standard S3 tooling (the
// AWS CLI's `s3 mb`, terraform, etc.) see a successful response. The
// call must still authenticate so unauthenticated callers cannot use
// it to enumerate bucket names.
func (h *Handler) PutBucket(w http.ResponseWriter, r *http.Request) {
	if _, err := h.authenticate(r); err != nil {
		writeAuthError(w, r, err)
		return
	}
	bucket, _ := parseBucketKey(r.URL.Path)
	if bucket == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "path must be /{bucket}", r.URL.Path)
		return
	}
	w.Header().Set("Location", "/"+bucket)
	w.WriteHeader(http.StatusOK)
}

// HeadBucket is a no-op bucket existence probe. Since buckets are
// implicit, an authenticated HEAD always returns 200 OK.
func (h *Handler) HeadBucket(w http.ResponseWriter, r *http.Request) {
	if _, err := h.authenticate(r); err != nil {
		writeAuthError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Put handles S3 PUT object. It reads the request body, writes it to
// the storage backend chosen by the placement engine, constructs an
// ObjectManifest, and persists it to the manifest store.
//
// When the resolved PlacementPolicy names a registered ErasureProfile
// the handler diverts to putErasureCoded, which shards the object
// into k + m pieces per stripe. Otherwise a single piece is written
// and the provider's native durability carries the object.
func (h *Handler) Put(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	if h.cfg.VerifiedCheck != nil {
		if verified, tracked := h.cfg.VerifiedCheck(tenantID); tracked && !verified {
			writeError(w, http.StatusForbidden, "EmailNotVerified",
				"verify your email address before uploading; check your inbox or POST /api/v1/auth/verify with your tenantId",
				r.URL.Path)
			return
		}
	}
	bucket, key := parseBucketKey(r.URL.Path)
	if bucket == "" || key == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "path must be /{bucket}/{key...}", r.URL.Path)
		return
	}
	if h.cfg.Manifests == nil || h.cfg.Placement == nil {
		writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable", "manifest store or placement engine not configured", r.URL.Path)
		return
	}

	// Inline object tagging (x-amz-tagging): validate the header up front
	// so a malformed tag set fails with 400 before any backend write. The
	// single-piece / erasure-coded / dedup manifest paths below re-read the
	// validated value via requestObjectTags.
	if _, verr := parseObjectTaggingHeader(r.Header.Get("x-amz-tagging")); verr != nil {
		writeError(w, verr.code, verr.s3code, verr.msg, r.URL.Path)
		return
	}

	// Object metadata (x-amz-meta-*): reject an oversized user-metadata
	// set up front, before any backend write, matching AWS's 2 KB limit.
	if verr := validateRequestObjectMetadata(r.Header); verr != nil {
		writeError(w, verr.code, verr.s3code, verr.msg, r.URL.Path)
		return
	}

	// Object Lock overwrite enforcement (WS8.3): when versioning is
	// NOT Enabled, a PUT replaces the current version in place, which
	// would destroy a locked version. Refuse the overwrite up-front
	// (before touching the backend) if the current version is locked.
	// With versioning Enabled the PUT creates a new version and the
	// locked one is preserved, so no check is needed — and Object Lock
	// requires versioning, so this guard only bites the suspended/unset
	// edge. The helper writes the response on block/error.
	if !h.allowObjectLockOverwrite(w, r, tenantID, bucket, key) {
		return
	}

	backendName, policy, err := h.cfg.Placement.ResolveBackend(tenantID, bucket, key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidPlacement", err.Error(), r.URL.Path)
		return
	}
	provider, ok := h.cfg.Providers[backendName]
	if !ok {
		writeError(w, http.StatusInternalServerError, "BackendNotRegistered", "backend "+backendName+" is not in the provider registry", r.URL.Path)
		return
	}

	if h.cfg.Compliance.Residency != nil {
		if err := h.cfg.Compliance.Residency.Check(
			tenantID, provider.PlacementLabels().Country, policy.Residency,
		); err != nil {
			writeError(w, http.StatusForbidden, "DataResidencyViolation", err.Error(), r.URL.Path)
			return
		}
	}

	// Layer the bucket default-encryption configuration (WS8.7) over
	// the placement policy: when the policy leaves the mode empty, a
	// configured bucket default promotes the write to managed. Done
	// before the EC / dedup branches so every write path sees the same
	// effective mode.
	effMode, err := h.effectiveEncryptionMode(r.Context(), tenantID, bucket, policy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "EncryptionNotConfigured", err.Error(), r.URL.Path)
		return
	}
	policy.EncryptionMode = effMode

	if policy.ErasureProfile != "" {
		h.putErasureCoded(w, r, tenantID, bucket, key, backendName, provider, policy)
		return
	}

	encMode := policy.EncryptionMode

	// Dedup path: when policy enables intra-tenant dedup AND the
	// gateway has a content_index store wired, route through the
	// pattern-specific lookup/register flow before touching the
	// backend. EC-coded objects are excluded above so the dedup
	// flow always runs against a single piece. Empty encryption
	// mode is intentionally NOT routed here: prepareDedupedPut
	// only knows the managed/public_distribution (Pattern B) and
	// client_side (Pattern C) flows and would 500 on an empty
	// mode — leave plaintext-on-backend tenants on the legacy
	// non-dedup path.
	if h.dedupEnabled(policy) && (IsGatewayEncrypted(encMode) || encMode == "client_side") {
		h.putDeduped(w, r, tenantID, bucket, key, backendName, provider, policy)
		return
	}

	// Generate the version (pieceID) BEFORE encryption so the AAD
	// v1 binding can fix it into every chunk's tag. The manifest
	// records this same VersionID, so the GET path rebuilds the
	// identical AAD; deferring it until after the encrypt stream
	// (as the pre-AAD code did) would leave nothing to bind to.
	pieceID := newPieceID(tenantID, bucket, key, h.cfg.Now())
	encID := aadIdentity{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: hashObjectKey(key),
		VersionID:     pieceID,
	}

	encCfg, body, contentLength, plaintextSizeFn, ok := h.prepareSinglePieceEncryption(w, r, encMode, encID)
	if !ok {
		return
	}

	// Tee the body through a BLAKE3 hasher so we record an
	// independent content-integrity hash that does not rely on
	// the storage provider's ETag.
	blake3Hasher := blake3.New()
	teeBody := io.TeeReader(body, blake3Hasher)

	putRes, err := provider.PutPiece(r.Context(), pieceID, teeBody, providers.PutOptions{
		ContentLength: contentLength,
		ContentType:   r.Header.Get("Content-Type"),
	})
	if err != nil {
		// PutPiece reads from the MaxBytesReader-wrapped body
		// transitively; client overflows surface here as
		// *http.MaxBytesError and writePutPieceError converts
		// them to 413 EntityTooLarge. All other errors are
		// genuine backend failures and map to 502.
		writePutPieceError(w, r, err)
		return
	}
	blake3Hash := "blake3:" + hex.EncodeToString(blake3Hasher.Sum(nil))

	// Resolve actual plaintext size NOW that the encrypt stream
	// has been drained by PutPiece. For the streaming PUT path
	// this is the byteCountingReader's final count (ground truth);
	// for the buffered path it is len(plaintext) captured at
	// io.ReadAll time. The closure pattern lets the helper
	// expose ground truth uniformly regardless of which path
	// produced the body. See prepareSinglePieceEncryption docs.
	plaintextSize := plaintextSizeFn()

	// Post-flight Content-Length validation: when the client
	// declared a Content-Length up-front, the gateway advertised
	// EncryptedSize(plaintextSize, …) to the backend on the basis
	// of that claim. If the client lied (sent fewer bytes), we
	// would otherwise persist a manifest whose ObjectSize does
	// not match the bytes actually stored. Roll back the backend
	// piece and reject so the client sees an explicit error
	// instead of a silently corrupted manifest. Only enforced
	// when the request had a known Content-Length AND the
	// encryption mode pulls ObjectSize from plaintextSize (the
	// gateway-encrypted modes); for StrictZK and legacy the
	// gateway does not own the plaintext invariant.
	if IsGatewayEncrypted(encMode) && r.ContentLength >= 0 && plaintextSize != r.ContentLength {
		_ = provider.DeletePiece(r.Context(), pieceID)
		writeError(w, http.StatusBadRequest, "IncompleteBody",
			"Content-Length declared by client does not match plaintext bytes received", r.URL.Path)
		return
	}

	// ObjectSize must reflect what the client will read back, not
	// the bytes we actually wrote to the backend. For managed /
	// public_distribution modes we unseal on the GET path so
	// ObjectSize is the plaintext size; for client_side the client
	// stores and retrieves the bytes verbatim.
	objectSize := putRes.SizeBytes
	if IsGatewayEncrypted(encMode) {
		objectSize = plaintextSize
	}

	manifest := &metadata.ObjectManifest{
		TenantID:        tenantID,
		Bucket:          bucket,
		ObjectKey:       key,
		ObjectKeyHash:   hashObjectKey(key),
		VersionID:       pieceID,
		ObjectSize:      objectSize,
		ChunkSize:       putRes.SizeBytes,
		Encryption:      encCfg,
		PlacementPolicy: policy,
		Pieces: []metadata.Piece{{
			PieceID:      putRes.PieceID,
			Backend:      backendName,
			Locator:      putRes.Locator,
			Hash:         blake3Hash,
			ProviderETag: putRes.ETag,
			SizeBytes:    putRes.SizeBytes,
			State:        "active",
		}},
		Tags: requestObjectTags(r.Header.Get("x-amz-tagging")),
		MigrationState: metadata.MigrationState{
			Generation:     1,
			PrimaryBackend: backendName,
		},
		CreatedAt: h.cfg.Now(),
	}
	applyRequestObjectMetadata(manifest, r.Header)
	if err := h.applyDefaultObjectLockRetention(r.Context(), tenantID, bucket, manifest); err != nil {
		_ = provider.DeletePiece(r.Context(), pieceID)
		writeError(w, http.StatusInternalServerError, "ObjectLockGetFailed", err.Error(), r.URL.Path)
		return
	}
	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: manifest.ObjectKeyHash,
		VersionID:     manifest.VersionID,
	}
	if err := h.cfg.Manifests.Put(r.Context(), mkey, manifest); err != nil {
		// Best-effort rollback of the piece to avoid orphaning
		// backend state when the control plane refuses the manifest.
		_ = provider.DeletePiece(r.Context(), pieceID)
		writeError(w, http.StatusInternalServerError, "ManifestPutFailed", err.Error(), r.URL.Path)
		return
	}

	h.emit(tenantID, bucket, billing.PutRequests, 1)
	if putRes.SizeBytes > 0 {
		h.emit(tenantID, bucket, billing.StorageBytesSeconds, uint64(putRes.SizeBytes))
	}
	h.audit(r, "PUT", tenantID, bucket, key, pieceID, backendName, provider.PlacementLabels().Country)
	h.notify(r, eventObjectCreatedPut, tenantID, bucket, key, putRes.ETag, manifest.VersionID, manifest.ObjectSize)

	w.Header().Set("ETag", quote(putRes.ETag))
	w.Header().Set("x-amz-version-id", manifest.VersionID)
	w.WriteHeader(http.StatusOK)
}

// audit writes a single AuditEntry through the configured
// compliance hook, if any. Errors are logged but never propagate
// to the caller — auditing is best-effort.
func (h *Handler) audit(r *http.Request, op, tenantID, bucket, key, pieceID, backend, country string) {
	if h.cfg.Compliance.Audit == nil {
		return
	}
	_ = h.cfg.Compliance.Audit.Record(r.Context(), AuditEntry{
		TenantID:       tenantID,
		Operation:      op,
		Bucket:         bucket,
		ObjectKey:      key,
		PieceID:        pieceID,
		PieceBackend:   backend,
		BackendCountry: country,
		Timestamp:      h.cfg.Now(),
		// Pull the id from the request context, not from
		// the request header. requestid.Middleware
		// installs the id in the context (and in the
		// RESPONSE header), but never mutates the request
		// header — so a header-only lookup misses every
		// server-generated id and only catches the
		// upstream-supplied case. FromContext returns the
		// empty string when called without the middleware
		// (unit tests, internal goroutines), which the
		// audit consumer already treats as "no id".
		RequestID: requestid.FromContext(r.Context()),
	})
}

// condETagForRead returns the quoted ETag the gateway advertises for a
// resolved object's conditional-request comparison, or "" when the
// object exposes none. Erasure-coded and multipart objects have only
// per-shard / per-part piece hashes, not a per-object ETag, so they
// participate in conditional reads only through the "*" wildcard and
// the date-based validators.
func condETagForRead(manifest *metadata.ObjectManifest, piece metadata.Piece) string {
	if isErasureCodedManifest(manifest) || isMultipartManifest(manifest) {
		return ""
	}
	if e := pieceETag(piece); e != "" {
		return quote(e)
	}
	return ""
}

// applyReadConditionals emits the object's Last-Modified header and
// evaluates the RFC 7232 conditional-request headers shared by GET and
// HEAD. It returns true when it has fully written the response (a 304
// Not Modified or 412 Precondition Failed short-circuit), so the caller
// must return without touching the backend; false means the read should
// proceed normally.
//
// It runs at the post-resolve / pre-dispatch chokepoint so a satisfied
// precondition short-circuits before any backend fetch, EC decode, or
// multipart assembly — the whole point of conditional reads. The
// advertised object ETag drives entity-tag comparison: the single-piece
// read paths set ETag = pieceETag(piece), while erasure-coded and
// multipart objects expose no per-object ETag, so for them only the "*"
// wildcard and the date-based conditionals apply.
//
// A 304/412 short-circuit is not separately metered, mirroring the
// other pre-body early returns (e.g. an invalid Range 416).
func (h *Handler) applyReadConditionals(w http.ResponseWriter, r *http.Request, manifest *metadata.ObjectManifest, piece metadata.Piece) bool {
	condETag := condETagForRead(manifest, piece)
	w.Header().Set("Last-Modified", manifest.CreatedAt.UTC().Format(http.TimeFormat))

	status, diverted := evaluateConditionalRead(r.Header, condETag, manifest.CreatedAt)
	if !diverted {
		// RFC 7233 §3.2: a Range paired with a now-stale If-Range
		// validator must fall back to a full 200, so normalise the
		// Range header here — before any single-piece / EC / multipart
		// dispatch path parses it.
		applyIfRange(r, condETag, manifest.CreatedAt)
		return false
	}
	if status == http.StatusNotModified {
		// RFC 7232 §4.1: a 304 carries the validators it would have
		// sent with the 200 so the client can refresh cached metadata.
		if condETag != "" {
			w.Header().Set("ETag", condETag)
		}
		w.Header().Set("x-amz-version-id", manifest.VersionID)
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	writeError(w, http.StatusPreconditionFailed, "PreconditionFailed",
		"the conditional request precondition failed", r.URL.Path)
	return true
}

// Get handles S3 GET object.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	manifest, pieceProvider, piece, tenantID, bucket, err := h.resolve(r)
	if err != nil {
		writeResolveError(w, r, err)
		return
	}

	if h.applyReadConditionals(w, r, manifest, piece) {
		return
	}

	// Single chokepoint for object metadata: emit Content-Type and the
	// stored system / x-amz-meta-* headers before dispatching to any
	// read path, so single-piece, erasure-coded, multipart, and
	// gateway-encrypted GETs all return the same metadata.
	setObjectMetadataHeaders(w, manifest)

	if isErasureCodedManifest(manifest) {
		h.getErasureCoded(w, r, manifest, tenantID, bucket)
		return
	}

	if isMultipartManifest(manifest) {
		h.getMultipart(w, r, manifest, tenantID, bucket)
		return
	}

	var byteRange *providers.ByteRange
	if hdr := r.Header.Get("Range"); hdr != "" {
		rng, perr := parseHTTPRange(hdr, manifest.ObjectSize)
		if perr != nil {
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", perr.Error(), r.URL.Path)
			return
		}
		byteRange = rng
	}

	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: manifest.ObjectKeyHash,
		VersionID:     manifest.VersionID,
	}

	// Branch on (encrypted, range) before we touch the backend.
	// Four paths:
	//
	//   1. Not gateway-encrypted, any range:
	//      fetchPiece handles ciphertext-vs-clear-bytes
	//      transparently; the body it returns is what we serve.
	//   2. Gateway-encrypted, no range:
	//      Streaming decryption path — pull ciphertext as an
	//      io.Reader, TeeReader through a BLAKE3 hasher for
	//      post-EOF integrity verification, then chain through
	//      streamDecryptFromStorage so plaintext flows straight
	//      to the client. No MaxInMemoryObjectBytes ceiling: a
	//      multi-GiB object uses a few hundred KiB of buffers
	//      (one DecryptObject chunk frame + os pipes).
	//   3. Gateway-encrypted, range request:
	//      Buffered decrypt path. We need the full plaintext in
	//      memory to slice an arbitrary byte range; chunk-level
	//      range seek lands in v0.2.0. Until then the
	//      MaxInMemoryObjectBytes ceiling still applies to
	//      protect the gateway from OOM on a 4 GiB Range GET.
	if IsGatewayEncrypted(manifest.Encryption.Mode) {
		if byteRange == nil {
			h.streamGatewayDecryptedGet(w, r, mkey, manifest, piece, pieceProvider, tenantID, bucket)
			return
		}
		h.bufferedGatewayDecryptedGet(w, r, mkey, manifest, piece, pieceProvider, byteRange, tenantID, bucket)
		return
	}

	body, served, err := h.fetchPiece(r, mkey, manifest, piece, pieceProvider, byteRange, tenantID, bucket)
	if err != nil {
		if errors.Is(err, pieceintegrity.ErrIntegrityCheckFailed) {
			writeError(w, http.StatusBadGateway, "IntegrityCheckFailed",
				"backend returned a piece whose content hash did not match the manifest; refusing to serve",
				r.URL.Path)
			return
		}
		writeError(w, http.StatusBadGateway, "BackendGetFailed", err.Error(), r.URL.Path)
		return
	}
	defer body.Close()

	if etag := pieceETag(piece); etag != "" {
		w.Header().Set("ETag", quote(etag))
	}
	w.Header().Set("x-amz-version-id", manifest.VersionID)
	status := http.StatusOK
	if byteRange != nil {
		end := byteRange.End
		if end < 0 {
			end = manifest.ObjectSize - 1
		}
		w.Header().Set("Content-Range", formatContentRange(byteRange, manifest.ObjectSize))
		w.Header().Set("Content-Length", strconv.FormatInt(end-byteRange.Start+1, 10))
		status = http.StatusPartialContent
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(manifest.ObjectSize, 10))
	}
	w.WriteHeader(status)
	n, _ := io.Copy(w, body)

	h.emit(tenantID, bucket, billing.GetRequests, 1)
	if n > 0 {
		h.emit(tenantID, bucket, billing.EgressBytes, uint64(n))
		if !served {
			// A cache miss counts toward origin egress; hits are
			// served from the gateway's local cache and do not
			// touch the origin.
			h.emit(tenantID, bucket, billing.OriginEgressBytes, uint64(n))
		}
	}
	h.audit(r, "GET", tenantID, bucket, manifest.ObjectKey, piece.PieceID, piece.Backend, pieceProvider.PlacementLabels().Country)
}

// bufferedGatewayDecryptedGet serves a gateway-encrypted object
// with a Range header. We must materialise the full plaintext to
// slice an arbitrary range (chunk-level range seek is a v0.2.0
// follow-up), so the MaxInMemoryObjectBytes ceiling still applies
// here as a hard OOM guard.
func (h *Handler) bufferedGatewayDecryptedGet(
	w http.ResponseWriter,
	r *http.Request,
	mkey manifest_store.ManifestKey,
	manifest *metadata.ObjectManifest,
	piece metadata.Piece,
	pieceProvider providers.StorageProvider,
	byteRange *providers.ByteRange,
	tenantID, bucket string,
) {
	if manifest.ObjectSize > MaxInMemoryObjectBytes {
		// TODO(v0.2.0): chunk-level range seek using the SDK's
		// 64 KiB chunk-frame headers lets us decrypt only the
		// chunks overlapping byteRange instead of the full
		// object. Until that lands, very large gateway-encrypted
		// objects with Range headers are refused outright; the
		// non-range GET path uses streamGatewayDecryptedGet and
		// has no ceiling.
		writeError(w, http.StatusInsufficientStorage, "RangeRequestTooLargeForBufferedDecrypt",
			fmt.Sprintf("range GET on gateway-encrypted object of %d bytes exceeds in-memory decrypt ceiling of %d bytes; full-object GETs stream without this ceiling",
				manifest.ObjectSize, MaxInMemoryObjectBytes),
			r.URL.Path)
		return
	}

	// The fetchPiece path returns a buffered + integrity-verified
	// ciphertext for pieces inside the ceiling, exactly what we
	// need to feed decryptFromStorage. Forcing byteRange=nil here
	// (instead of pre-nilling the caller's byteRange like the old
	// code did) keeps the cache and integrity logic uniform with
	// the unencrypted path.
	body, served, err := h.fetchPiece(r, mkey, manifest, piece, pieceProvider, nil, tenantID, bucket)
	if err != nil {
		if errors.Is(err, pieceintegrity.ErrIntegrityCheckFailed) {
			writeError(w, http.StatusBadGateway, "IntegrityCheckFailed",
				"backend returned a piece whose content hash did not match the manifest; refusing to serve",
				r.URL.Path)
			return
		}
		writeError(w, http.StatusBadGateway, "BackendGetFailed", err.Error(), r.URL.Path)
		return
	}
	defer body.Close()

	ciphertext, rerr := io.ReadAll(body)
	if rerr != nil {
		writeError(w, http.StatusBadGateway, "BackendGetFailed", rerr.Error(), r.URL.Path)
		return
	}
	plaintext, derr := h.decryptFromStorage(ciphertext, manifest.Encryption, aadIdentityOf(manifest))
	if derr != nil {
		writeError(w, http.StatusInternalServerError, "DEKUnwrapFailed", derr.Error(), r.URL.Path)
		return
	}
	end := byteRange.End
	if end < 0 || end >= int64(len(plaintext)) {
		end = int64(len(plaintext)) - 1
	}
	if byteRange.Start < 0 || byteRange.Start > end+1 {
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "range out of bounds", r.URL.Path)
		return
	}
	sliced := plaintext[byteRange.Start : end+1]

	if etag := pieceETag(piece); etag != "" {
		w.Header().Set("ETag", quote(etag))
	}
	w.Header().Set("x-amz-version-id", manifest.VersionID)
	w.Header().Set("Content-Range", formatContentRange(byteRange, manifest.ObjectSize))
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(sliced)), 10))
	w.WriteHeader(http.StatusPartialContent)
	n, _ := w.Write(sliced)

	h.emit(tenantID, bucket, billing.GetRequests, 1)
	if n > 0 {
		h.emit(tenantID, bucket, billing.EgressBytes, uint64(n))
		if !served {
			h.emit(tenantID, bucket, billing.OriginEgressBytes, uint64(n))
		}
	}
	h.audit(r, "GET", tenantID, bucket, manifest.ObjectKey, piece.PieceID, piece.Backend, pieceProvider.PlacementLabels().Country)
}

// streamGatewayDecryptedGet serves a gateway-encrypted, non-range
// GET by piping ciphertext directly through the SDK's chunk-frame
// decryptor. No MaxInMemoryObjectBytes ceiling here — a multi-GiB
// object uses a constant ~64 KiB chunk buffer plus a small TeeReader
// + BLAKE3 hasher.
//
// Integrity verification is post-stream: we Tee the ciphertext into
// a BLAKE3 hasher before it hits the decryptor and compare the
// final digest against piece.Hash after io.Copy returns. Unlike the
// buffered path (which can return 502 before any bytes reach the
// client) we may have already written plaintext to the response by
// the time a mismatch is detected. The trade-off is documented in
// the v0.1.0 hardening plan; the alternatives — buffer the full
// object before sending or refuse multi-GiB GETs — both regress
// the throughput goal of this PR. On mismatch we record the
// failure (Prometheus + ERROR log) and stop writing, which truncates
// the response and surfaces to the client as either a
// Content-Length mismatch (chunked transfer or otherwise) or a
// shorter body than declared.
//
// Read-repair: if the primary backend GetPiece fails, we fall back
// to tryReadRepair which buffers the repaired piece in memory and
// returns it with preVerified=true. We stream-decrypt the buffered
// bytes and skip our own hash check (ReadRepair already verified).
//
// Cache: a cache hit returns the cached ciphertext, which the cache
// only stores after a successful prior verification, so we skip
// the hash check on hits. On a miss we do NOT inline-warm the
// cache from the streaming path — buffering the full ciphertext
// just to warm the cache would defeat the purpose of streaming.
// We publish a promotion signal so the async worker can decide.
func (h *Handler) streamGatewayDecryptedGet(
	w http.ResponseWriter,
	r *http.Request,
	mkey manifest_store.ManifestKey,
	manifest *metadata.ObjectManifest,
	piece metadata.Piece,
	pieceProvider providers.StorageProvider,
	tenantID, bucket string,
) {
	ciphertextSrc, served, verifyFn, err := h.openCiphertextStream(r, mkey, manifest, piece, pieceProvider, tenantID, bucket)
	if err != nil {
		writeError(w, http.StatusBadGateway, "BackendGetFailed", err.Error(), r.URL.Path)
		return
	}
	defer ciphertextSrc.Close()

	plaintext, derr := h.streamDecryptFromStorage(ciphertextSrc, manifest.Encryption, aadIdentityOf(manifest))
	if derr != nil {
		writeError(w, http.StatusInternalServerError, "DEKUnwrapFailed", derr.Error(), r.URL.Path)
		return
	}

	if etag := pieceETag(piece); etag != "" {
		w.Header().Set("ETag", quote(etag))
	}
	w.Header().Set("x-amz-version-id", manifest.VersionID)
	// manifest.ObjectSize is the plaintext length on
	// gateway-encrypted objects, so Content-Length is honest
	// without buffering the decrypted bytes. The client uses it
	// to detect truncation if we abort mid-stream on integrity
	// failure.
	w.Header().Set("Content-Length", strconv.FormatInt(manifest.ObjectSize, 10))
	w.WriteHeader(http.StatusOK)
	// Wrap the response writer so we can distinguish read-side
	// errors (decryptor / chunk-AEAD reject — a real corruption
	// signal) from write-side errors (client RST, broken pipe — a
	// transport hiccup). io.Copy returns whichever error happened
	// first, but only the wrapper sees write failures directly.
	// The branching below uses this to decide whether to call
	// verifyFn at all: on a write-side failure the TeeReader has
	// only hashed a partial ciphertext prefix, so calling verifyFn
	// would always mismatch and produce a false-positive
	// zkof_integrity_failure_total tick that trains operators to
	// ignore the counter.
	ew := &writeErrCapturingWriter{w: w}
	n, copyErr := io.Copy(ew, plaintext)

	// Three terminal states; each chooses how to interact with the
	// integrity counter so the metric stays a high-fidelity signal
	// for actual content corruption:
	//
	//   1. copyErr == nil — full stream landed cleanly. Run the
	//      post-EOF BLAKE3 check on the ciphertext the TeeReader
	//      observed and record the appropriate counter on a
	//      mismatch (or unrecognised legacy claim).
	//
	//   2. ew.writeErr != nil — client side aborted before we
	//      finished writing. The TeeReader has only hashed a
	//      ciphertext prefix; calling verifyFn would always
	//      mismatch. Skip the integrity check entirely; the WARN
	//      log below still surfaces the truncation. This is the
	//      false-positive case Devin Review flagged.
	//
	//   3. copyErr != nil && ew.writeErr == nil — decryptor /
	//      chunk-AEAD rejected a frame. The error itself is the
	//      integrity signal (Poly1305 said the bytes are wrong),
	//      so we record an integrity failure with the underlying
	//      error rather than re-deriving the same conclusion via
	//      a partial-hash mismatch. This preserves the
	//      tamper-detection contract pinned by
	//      TestManagedEncryption_StreamingGet_TamperedPieceDetected.
	switch {
	case copyErr == nil:
		if verifyFn != nil {
			if verr := verifyFn(); verr != nil {
				if errors.Is(verr, pieceintegrity.ErrIntegrityClaimUnrecognized) {
					h.recordIntegrityUnrecognized(piece, verr)
				} else {
					// Mismatch on the streaming path is a
					// detection (we cannot un-send the bytes the
					// client already has), but we still:
					//   (a) emit the Prometheus counter so
					//       operators see it on dashboards;
					//   (b) log at ERROR with piece + backend +
					//       expected/got so on-call can pivot.
					h.recordIntegrityFailure(piece, verr)
				}
			}
		}
	case ew.writeErr != nil:
		// Client disconnect / broken pipe. Don't ring the
		// integrity bell — partial hash would lie. WARN log
		// below carries the bytes-sent + cause.
	default:
		// Read-side error: decryptor or chunk-AEAD reject. The
		// error itself is the integrity signal. Skip the
		// post-EOF BLAKE3 check (it would either mismatch on
		// the same partial-hash basis or be redundant with this
		// stronger signal) and record the counter directly so
		// the failure shows up on dashboards with the
		// underlying error for diagnosis.
		h.recordIntegrityFailure(piece, copyErr)
	}

	if copyErr != nil {
		// Mid-stream failure: chunk-AEAD reject, client RST,
		// broken pipe, etc. The status line and any headers
		// already went out, and n bytes of plaintext were
		// committed to the wire before the failure, so the
		// client sees a truncated stream and the gateway has
		// burned real egress. Surface it at WARN with piece +
		// backend + bytes-sent so on-call can correlate the
		// billing emission below against the failure mode. The
		// write_err field distinguishes client disconnects
		// (writeErr != nil) from read-side decrypt failures
		// (writeErr == nil) so on-call doesn't need to grep
		// chunk-AEAD error strings to triage.
		log.Printf("s3compat: WARN streaming_get_truncated: piece=%s backend=%s bytes_sent=%d write_err=%v err=%v",
			piece.PieceID, piece.Backend, n, ew.writeErr, copyErr)
	}
	h.emit(tenantID, bucket, billing.GetRequests, 1)
	// Bill on bytes actually written to the response, regardless
	// of whether io.Copy returned an error. A mid-stream failure
	// still committed n bytes of plaintext to the wire —
	// gating on copyErr == nil here would silently undercount
	// egress on flaky clients or tampered ciphertext. Every
	// other GET path (fetchPiece-backed, EC, multipart) bills on
	// n > 0 unconditionally; this matches that invariant.
	if n > 0 {
		h.emit(tenantID, bucket, billing.EgressBytes, uint64(n))
		if !served {
			h.emit(tenantID, bucket, billing.OriginEgressBytes, uint64(n))
		}
	}
	h.audit(r, "GET", tenantID, bucket, manifest.ObjectKey, piece.PieceID, piece.Backend, pieceProvider.PlacementLabels().Country)
}

// openCiphertextStream is the streaming-path counterpart to
// fetchPiece for gateway-encrypted GETs. It consults the cache,
// falls back to the backend, falls back to read-repair, and wraps
// the resulting reader with a BLAKE3 TeeReader so the caller can
// stream the bytes downstream while still checking integrity after
// EOF.
//
// Return values:
//   - body: io.ReadCloser yielding ciphertext. Caller MUST close.
//   - served: true on cache hit (for billing).
//   - verifyFn: nil if the source is already verified (cache hit
//     or repair-supplied buffer); otherwise a function the caller
//     invokes AFTER draining body to EOF, which returns nil on a
//     match, pieceintegrity.ErrIntegrityCheckFailed on a mismatch,
//     or pieceintegrity.ErrIntegrityClaimUnrecognized on a legacy
//     manifest with an unparseable Hash field.
//   - err: opening the stream failed entirely.
func (h *Handler) openCiphertextStream(
	r *http.Request,
	mkey manifest_store.ManifestKey,
	manifest *metadata.ObjectManifest,
	piece metadata.Piece,
	pieceProvider providers.StorageProvider,
	tenantID, bucket string,
) (io.ReadCloser, bool, func() error, error) {
	if h.cfg.Cache != nil {
		cached, _, cerr := h.cfg.Cache.Get(r.Context(), piece.PieceID)
		if cerr == nil {
			h.emit(tenantID, bucket, billing.CacheHits, 1)
			return cached, true, nil, nil
		}
	}
	if h.cfg.Cache != nil {
		h.emit(tenantID, bucket, billing.CacheMisses, 1)
	}
	body, err := pieceProvider.GetPiece(r.Context(), piece.PieceID, nil)
	if err != nil {
		repaired, repairedVerified, repairErr := h.tryReadRepair(r, mkey, manifest, nil)
		if repairErr != nil || repaired == nil {
			return nil, false, nil, err
		}
		// Read-repair returns a buffered + verified body. Stream
		// from it directly with no TeeReader: the verifier is
		// implicit in the preVerified flag.
		if repairedVerified {
			return repaired, false, nil, nil
		}
		body = repaired
	}

	pieceSize := piece.SizeBytes
	if pieceSize <= 0 {
		pieceSize = manifest.ObjectSize
	}
	if h.cfg.Cache != nil {
		// Publish a promotion signal so the async worker can
		// decide whether to fetch + cache the ciphertext on its
		// own schedule. We do NOT inline-warm here because that
		// would force us to buffer the full ciphertext for the
		// cache Put, defeating the streaming path's purpose.
		h.signalPromotion(piece, tenantID, pieceSize, pieceSize)
	}

	// pieceintegrity.Hasher returns (a) an io.Writer keyed off
	// piece.Hash's format (BLAKE3 / legacy SHA-256 / unrecognised)
	// and (b) a Check closure that returns nil on a match,
	// pieceintegrity.ErrIntegrityCheckFailed on a content
	// mismatch, or pieceintegrity.ErrIntegrityClaimUnrecognized
	// on a legacy unparseable manifest. TeeReader fans every byte
	// our caller reads into the hasher, so by the time the body
	// is drained the Check closure can fire without us having
	// kept a copy of the ciphertext.
	hasher, check := pieceintegrity.Hasher(piece)
	teed := io.TeeReader(body, hasher)
	return &teeingCloser{r: teed, c: body}, false, check, nil
}

// teeingCloser wraps a TeeReader so the gateway's caller can defer
// .Close() on a single io.ReadCloser. The TeeReader itself is not
// a Closer; we delegate to the underlying body so the backend
// connection is released when the request handler unwinds.
type teeingCloser struct {
	r io.Reader
	c io.Closer
}

func (t *teeingCloser) Read(p []byte) (int, error) { return t.r.Read(p) }
func (t *teeingCloser) Close() error               { return t.c.Close() }

// fetchPiece consults the hot object cache (if configured) before
// hitting the backend. Range requests bypass the cache because the
// cache is keyed by piece, not by byte range. The second return
// value reports whether the piece came from the cache.
//
// When the primary backend fails and the manifest sits in a
// migration-in-progress state (Generation > 1), fetchPiece falls
// back to the configured ReadRepair to fetch the piece from the
// secondary backend, copy it to the new primary, and serve the
// repaired body to the caller.
//
// Integrity verification: on a cache miss the handler re-hashes
// the piece body and compares it against piece.Hash before
// caching or serving. On a mismatch fetchPiece returns an error
// wrapping pieceintegrity.ErrIntegrityCheckFailed so Get() can
// map it to 502 IntegrityCheckFailed — the bad bytes are NEVER
// cached and NEVER served. The handler also emits the
// zkof_integrity_failure_total{backend="..."} counter and logs
// the failure at ERROR level with piece id, backend, expected
// hash, and computed hash so an on-call can correlate.
//
// Range requests on a cache miss fetch the full piece, verify
// it, then slice the range out of the verified buffer. This
// keeps the integrity guarantee end-to-end at the cost of
// reading the whole piece on a range cache miss; the cost is
// reclaimed on the next hit because the verified piece is also
// promoted into the cache (subject to the same
// MaxInMemoryObjectBytes ceiling as the cache-warming path).
// When the piece exceeds the ceiling we cannot verify in place
// without a streaming hasher — fall back to the legacy
// range-forward behaviour, publish a promotion signal, and
// surface a structured warning so operators can spot the
// unverified path in their logs. Streaming verify is delivered
// in the streaming-decryption PR.
func (h *Handler) fetchPiece(
	r *http.Request,
	mkey manifest_store.ManifestKey,
	manifest *metadata.ObjectManifest,
	piece metadata.Piece,
	pieceProvider providers.StorageProvider,
	byteRange *providers.ByteRange,
	tenantID, bucket string,
) (io.ReadCloser, bool, error) {
	objectSize := manifest.ObjectSize
	if h.cfg.Cache != nil {
		cached, _, err := h.cfg.Cache.Get(r.Context(), piece.PieceID)
		if err == nil {
			h.emit(tenantID, bucket, billing.CacheHits, 1)
			if byteRange == nil {
				return cached, true, nil
			}
			// Range request on a cache hit. The cache only
			// ever stores fully-verified pieces (the cache
			// put below gates on a successful Verify), so
			// no second integrity check is needed. Read the
			// cached body once and slice the requested
			// range out of it; the slice is bounded by the
			// per-piece cache ceiling (== MaxInMemoryObject
			// Bytes), so this cannot OOM. Without this hit
			// path every range request paid a backend
			// round-trip even when the bytes were already
			// hot — which was the pre-fix behaviour the
			// integrity rework inadvertently locked in
			// (fetchPiece now buffers full pieces for any
			// request that goes upstream, so the cache hit
			// pays off on the first range request after a
			// full-piece warm).
			buf, rerr := io.ReadAll(cached)
			_ = cached.Close()
			if rerr != nil {
				return nil, false, rerr
			}
			end := byteRange.End
			if end < 0 || end >= int64(len(buf)) {
				end = int64(len(buf)) - 1
			}
			if byteRange.Start < 0 || byteRange.Start > end+1 {
				return nil, false, fmt.Errorf("s3compat: range %d-%d out of bounds for piece %s (%d bytes)", byteRange.Start, byteRange.End, piece.PieceID, len(buf))
			}
			sliced := buf[byteRange.Start : end+1]
			return io.NopCloser(bytes.NewReader(sliced)), true, nil
		}
		// Cache miss: emit the counter once here for both
		// range and non-range. The legacy duplicate emission
		// below the wantFullPiece branch only fired on
		// byteRange == nil, which let range cache misses go
		// uncounted; consolidating it here fixes both gaps
		// without double-counting.
		h.emit(tenantID, bucket, billing.CacheMisses, 1)
	}

	pieceSize := piece.SizeBytes
	if pieceSize <= 0 {
		pieceSize = objectSize
	}

	// Decide up front whether we will buffer + verify or stream
	// raw. The cache-warming branch already wanted the full
	// piece, and the integrity branch on a range cache miss
	// likewise needs the whole piece so it can re-hash before
	// slicing. Both paths converge on the same
	// MaxInMemoryObjectBytes ceiling so a single very large GET
	// cannot OOM the gateway.
	wantFullPiece := byteRange == nil || (piece.Hash != "" && pieceSize <= MaxInMemoryObjectBytes)

	fetchRange := byteRange
	if wantFullPiece {
		fetchRange = nil
	}

	// preVerified is set when the body we end up serving has
	// already been integrity-checked upstream so we can skip the
	// redundant BLAKE3 re-hash below. Today only the
	// lazy_read_repair path satisfies this contract —
	// tryReadRepair returns the flag explicitly (not inferred at
	// the call site) so any future repair-path change that drops
	// or weakens its internal Verify call must also update the
	// returned flag, which would surface as a test failure rather
	// than a silent regression to serving unverified bytes.
	preVerified := false
	body, err := pieceProvider.GetPiece(r.Context(), piece.PieceID, fetchRange)
	if err != nil {
		repaired, repairedVerified, repairErr := h.tryReadRepair(r, mkey, manifest, fetchRange)
		if repairErr != nil || repaired == nil {
			return nil, false, err
		}
		body = repaired
		preVerified = repairedVerified
	}

	if !wantFullPiece {
		// Range cache-miss path for pieces too large to buffer:
		// fall back to the legacy stream-the-range behaviour. We
		// cannot verify the slice against a full-piece hash
		// without the streaming hasher (delivered in the
		// streaming-decryption PR), so log a structured warning
		// and proceed. Open-ended ranges (End == -1) resolve
		// against the object size so the published ReadBytes is
		// never negative.
		if piece.Hash != "" {
			log.Printf("s3compat: integrity check skipped: piece=%s backend=%s reason=oversize_range size_bytes=%d ceiling=%d",
				piece.PieceID, piece.Backend, pieceSize, MaxInMemoryObjectBytes)
		}
		if byteRange != nil {
			end := byteRange.End
			if end < 0 {
				end = objectSize - 1
			}
			h.signalPromotion(piece, tenantID, end-byteRange.Start+1, objectSize)
		}
		return body, false, nil
	}

	// Pieces above the in-memory ceiling skip buffer-and-verify to
	// keep a single very large GET from OOMing the request
	// goroutine. We fall back to streaming the raw body straight to
	// the client (the pre-PR behaviour). This guard fires whether
	// or not a hot cache is configured: the OOM risk is identical,
	// and the cache only changes whether we also emit a promotion
	// signal once the piece is back in budget. piece.SizeBytes is
	// populated for multipart and EC pieces; legacy single-piece
	// manifests fall back to ObjectSize, which equals the piece
	// size for non-multipart, non-EC objects.
	if pieceSize > MaxInMemoryObjectBytes {
		if piece.Hash != "" {
			log.Printf("s3compat: integrity check skipped: piece=%s backend=%s reason=oversize_full_piece size_bytes=%d ceiling=%d",
				piece.PieceID, piece.Backend, pieceSize, MaxInMemoryObjectBytes)
		}
		if h.cfg.Cache != nil && byteRange == nil {
			h.signalPromotion(piece, tenantID, pieceSize, pieceSize)
		}
		return body, false, nil
	}

	// The cache-warming memory-budget semaphore caps the total
	// bytes the gateway will buffer at once across every
	// concurrent fetchPiece-warming pass. The oversize-piece
	// short-circuit above caught pieces larger than the
	// in-memory ceiling; this guard handles pieces that fit the
	// per-piece ceiling but would push the aggregate budget
	// over. Two cases short-circuit to the promotion-signal
	// path:
	//
	//   1. The piece by itself is larger than the entire budget
	//      — TryAcquire would never succeed, so we skip without
	//      contending for the semaphore.
	//   2. The semaphore is fully consumed by other in-flight
	//      warmers — we fall back to the async-promotion path
	//      instead of blocking the request goroutine.
	//
	// A negative cfg.CacheWarmingMemoryBudget leaves
	// cacheWarmSem nil so the unbounded pre-PR-7 behaviour is
	// preserved for regression tests.
	//
	// Correctness for range cache misses: when the wantFullPiece
	// branch fired above (piece.Hash != "" AND pieceSize fits in
	// MaxInMemoryObjectBytes), GetPiece was called with
	// fetchRange == nil — the body in scope here is the FULL
	// piece, not the requested slice. If the budget rejects we
	// cannot just hand that full body back to Get(); the caller
	// already wrote Content-Range / Content-Length for the
	// range slice, so io.Copy(w, body) would emit the wrong
	// bytes and overrun Content-Length. The budget-rejected
	// branches below close the full-piece body and re-fetch
	// with the actual byteRange so the caller's stream-and-copy
	// path serves the correct slice. The extra round-trip is
	// the price of maintaining the budget invariant strictly;
	// the alternative (waive the budget for hashed-and-ranged
	// pieces) is rejected because a pathological client could
	// then DoS the gateway by issuing concurrent range GETs
	// on N distinct hashed pieces and blow past the operator's
	// configured memory ceiling.
	if h.cacheWarmSem != nil {
		tooBig := pieceSize > h.cacheWarmBudget
		if tooBig || !h.cacheWarmSem.TryAcquire(pieceSize) {
			if piece.Hash != "" {
				if tooBig {
					log.Printf("s3compat: integrity check skipped: piece=%s backend=%s reason=cache_warm_budget_too_small size_bytes=%d budget=%d",
						piece.PieceID, piece.Backend, pieceSize, h.cacheWarmBudget)
				} else {
					log.Printf("s3compat: integrity check skipped: piece=%s backend=%s reason=cache_warm_budget_exhausted size_bytes=%d",
						piece.PieceID, piece.Backend, pieceSize)
				}
			}
			h.notifyCacheWarmingExhausted(piece.Backend, pieceSize)
			// Publish promotion regardless of byteRange so the
			// async worker can warm the cache off the request
			// goroutine even when this request cannot. The
			// signalPromotion implementation is a no-op when
			// CachePublisher is nil, so it's safe to call
			// unconditionally.
			h.signalPromotion(piece, tenantID, pieceSize, pieceSize)
			// Critical: if the caller asked for a byteRange we
			// MUST NOT return the full-piece body — Get() will
			// emit a Content-Length sized for the slice and
			// the client would consume a truncated/wrong
			// payload (the data-corruption bug fixed by
			// d94e15e). There are two cases:
			//
			//   (1) preVerified == false. body came directly
			//   from pieceProvider.GetPiece(fetchRange=nil).
			//   The provider succeeded; closing and re-fetching
			//   with the actual byteRange is a clean way to
			//   shed the unwanted prefix — one extra round-trip
			//   to the same backend that just answered.
			//
			//   (2) preVerified == true. body came from
			//   tryReadRepair, which only fires when the
			//   ORIGINAL pieceProvider.GetPiece returned an
			//   error. The repaired bytes were copied from the
			//   secondary backend and the manifest's piece.Backend
			//   was updated to point at the new primary, but
			//   pieceProvider in this scope is still the
			//   pre-repair lookup — same failed backend that
			//   triggered the repair in the first place. A
			//   close-and-refetch from pieceProvider would
			//   target the failed backend again, almost
			//   certainly fail, and turn a successful
			//   read-repair into a hard 502 for the client.
			//   We avoid that by slicing the already-buffered
			//   repaired body (tryReadRepair returns a
			//   NopCloser(bytes.NewReader(...)) — the bytes are
			//   in memory). The budget guard's purpose is to
			//   bound the BUFFERED piece bytes across concurrent
			//   warmers; in the preVerified branch the bytes
			//   were buffered BEFORE the budget check (inside
			//   tryReadRepair), so the in-memory cost is
			//   already paid and serving from it does not
			//   violate the budget contract. signalPromotion
			//   above fires unconditionally so an async worker
			//   can still warm the cache off-path. This branch
			//   was previously missing — d94e15e fixed the
			//   data-corruption bug at the cost of breaking
			//   read-repair correctness under simultaneous
			//   budget exhaustion (rare-but-real: requires
			//   primary failure, repair success, budget
			//   exhaustion, range request on hashed piece
			//   within MaxInMemoryObjectBytes — all five
			//   conditions concurrent). The regression test
			//   TestCacheWarming_ReadRepairServedWhenBudgetExhausted
			//   pins this fix.
			if byteRange != nil {
				if preVerified {
					buf, rerr := io.ReadAll(body)
					_ = body.Close()
					if rerr != nil {
						return nil, false, rerr
					}
					end := byteRange.End
					if end < 0 || end >= int64(len(buf)) {
						end = int64(len(buf)) - 1
					}
					if byteRange.Start < 0 || byteRange.Start > end+1 {
						return nil, false, fmt.Errorf("s3compat: range %d-%d out of bounds for repaired piece %s (%d bytes)", byteRange.Start, byteRange.End, piece.PieceID, len(buf))
					}
					sliced := buf[byteRange.Start : end+1]
					return io.NopCloser(bytes.NewReader(sliced)), false, nil
				}
				_ = body.Close()
				ranged, rerr := pieceProvider.GetPiece(r.Context(), piece.PieceID, byteRange)
				if rerr != nil {
					return nil, false, rerr
				}
				return ranged, false, nil
			}
			return body, false, nil
		}
		// Defer the Release at the acquisition site so EVERY
		// exit path from here on (read error, integrity
		// failure, Cache.Put success, Cache == nil pass-through,
		// the range-slice return below) returns the budget
		// exactly once. Before this defer the function had
		// three ad-hoc Release sites that each guarded a
		// specific exit and missed the Cache == nil path,
		// which leaked pieceSize bytes from the semaphore on
		// every successful GET against a handler constructed
		// without a HotObjectCache (the leak only manifested
		// after enough requests exhausted the budget and
		// pushed subsequent GETs into the unverified streaming
		// fallback — a silent integrity-verification regression
		// that the existing tests did not catch because they
		// either ran with a Cache or used a negative budget to
		// disable the semaphore entirely). Defer also makes
		// every future exit path leak-safe by default.
		//
		// Ordering invariant: the comment block that the
		// previous post-Put Release lived under said "release
		// AFTER Cache.Put completes so the next warmer cannot
		// dogpile the same eviction window." That invariant
		// still holds: defer runs at function return, which
		// happens after the synchronous Cache.Put call below
		// completes (Go's defer is LIFO at return, not at
		// scope exit, and there are no inner Release calls
		// that could re-order it).
		defer h.cacheWarmSem.Release(pieceSize)
	}

	buf, rerr := io.ReadAll(body)
	_ = body.Close()
	if rerr != nil {
		return nil, false, rerr
	}

	if !preVerified {
		if verr := pieceintegrity.Verify(buf, piece); verr != nil {
			if errors.Is(verr, pieceintegrity.ErrIntegrityClaimUnrecognized) {
				// Legacy manifest with no recognised
				// integrity claim. We cannot prove the bytes
				// are wrong, so we serve them and surface the
				// count for operators via the dedicated
				// observability channel. A follow-up rewrite
				// migration is expected to fix the manifests
				// so this channel goes back to zero.
				h.recordIntegrityUnrecognized(piece, verr)
			} else {
				h.recordIntegrityFailure(piece, verr)
				return nil, false, verr
			}
		}
	}

	if h.cfg.Cache != nil {
		// Warm the cache inline regardless of whether this is a
		// full-piece or range request: when the integrity branch
		// fetched the full piece (wantFullPiece == true above),
		// the verified buffer is already in memory and the cache
		// is keyed by piece, not by byte range. Caching it costs
		// one memcpy and skips a backend round-trip on the next
		// request — full OR range — for the same piece. The
		// previous code gated this on byteRange == nil and then
		// published a promotion signal in the range branch,
		// which forced the async worker to re-fetch a piece we
		// already had verified bytes for. Oversize pieces (the
		// short-circuit above) and read-repair-supplied bodies
		// (preVerified == true, body served raw) still flow
		// through signalPromotion / the worker as before.
		_ = h.cfg.Cache.Put(r.Context(), piece.PieceID, bytes.NewReader(buf), hot_object_cache.PutOptions{
			SizeBytes: int64(len(buf)),
			Hash:      piece.Hash,
		})
	}

	if byteRange != nil {
		// We fetched the full piece for the integrity check;
		// slice it down to the requested range. The slice is
		// bounded by the buffered length, not ObjectSize,
		// because EC / multipart pieces may be smaller than the
		// object as a whole. No signalPromotion here: the cache
		// put above already warmed the full piece, so the async
		// worker would only cause a redundant origin fetch.
		end := byteRange.End
		if end < 0 || end >= int64(len(buf)) {
			end = int64(len(buf)) - 1
		}
		if byteRange.Start < 0 || byteRange.Start > end+1 {
			return nil, false, fmt.Errorf("s3compat: range %d-%d out of bounds for piece %s (%d bytes)", byteRange.Start, byteRange.End, piece.PieceID, len(buf))
		}
		sliced := buf[byteRange.Start : end+1]
		return io.NopCloser(bytes.NewReader(sliced)), false, nil
	}

	return io.NopCloser(bytes.NewReader(buf)), false, nil
}

// recordIntegrityFailure emits the structured error log and
// optional metric increment that an integrity-mismatched piece
// triggers. Centralised so every read path (fetchPiece today,
// the streaming-decrypt path in the follow-up PR) reports
// failures consistently.
func (h *Handler) recordIntegrityFailure(piece metadata.Piece, verr error) {
	log.Printf("s3compat: ERROR integrity_check_failed: piece=%s backend=%s expected_hash=%q err=%v",
		piece.PieceID, piece.Backend, piece.Hash, verr)
	if h.cfg.IntegrityFailures != nil {
		h.cfg.IntegrityFailures.Inc(piece.Backend)
	}
}

// recordIntegrityUnrecognized emits the WARN-level log and the
// dedicated observability counter for a manifest whose Hash is
// non-empty but not in any recognised format. The handler still
// serves the piece — there is no proof the bytes are wrong, only
// that we cannot check them — but operators get a count of
// unverifiable manifests so they can plan a one-shot rewrite
// that populates ProviderETag and clears the legacy Hash field.
func (h *Handler) recordIntegrityUnrecognized(piece metadata.Piece, verr error) {
	log.Printf("s3compat: WARN integrity_claim_unrecognized: piece=%s backend=%s recorded_hash=%q detail=%v",
		piece.PieceID, piece.Backend, piece.Hash, verr)
	if h.cfg.IntegrityFailures != nil {
		h.cfg.IntegrityFailures.IncUnrecognized(piece.Backend)
	}
}

// tryReadRepair invokes the configured ReadRepair when the primary
// backend fails to serve a piece and the manifest sits in a
// migration-in-progress state (Generation > 1). It returns:
//
//   - (body, true,  nil)   — repair succeeded; body has already
//     been integrity-verified by ReadRepair
//     and the caller MAY skip a second
//     pieceintegrity.Verify pass.
//   - (nil,  false, nil)   — repair is not applicable (no repair
//     wired, manifest not in migration, or
//     no pieces). Caller falls through to
//     the original backend error.
//   - (nil,  false, error) — repair attempt itself failed.
//
// The bool is part of the signature (not inferred at the call
// site) so the "already verified" contract is structural: any
// future change to ReadRepair.Repair that drops or weakens its
// internal Verify call MUST also flip the returned flag, which a
// reviewer will catch immediately.
func (h *Handler) tryReadRepair(
	r *http.Request,
	mkey manifest_store.ManifestKey,
	manifest *metadata.ObjectManifest,
	byteRange *providers.ByteRange,
) (io.ReadCloser, bool, error) {
	if h.cfg.ReadRepair == nil {
		return nil, false, nil
	}
	if manifest.MigrationState.Generation <= 1 {
		return nil, false, nil
	}
	if len(manifest.Pieces) == 0 {
		return nil, false, nil
	}
	res, err := h.cfg.ReadRepair.Repair(r.Context(), mkey, manifest, 0)
	if err != nil {
		return nil, false, err
	}
	data := res.Body
	if byteRange != nil {
		end := byteRange.End
		if end < 0 || end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		if byteRange.Start < 0 || byteRange.Start > end+1 {
			return nil, false, fmt.Errorf("s3compat: repaired body slice out of range")
		}
		data = data[byteRange.Start : end+1]
	}
	// ReadRepair.Repair calls pieceintegrity.Verify on the source
	// body before returning (lazy_read_repair/repair.go ~line 112).
	// Slicing a byte range below does not change those bytes' hash
	// relationship to the full-piece claim because we slice a
	// verified buffer in memory — the caller's downstream
	// fetchPiece guard already restricts byteRange-based reads to
	// pieces inside MaxInMemoryObjectBytes, and the full piece is
	// what got verified.
	return io.NopCloser(bytes.NewReader(data)), true, nil
}

func (h *Handler) signalPromotion(piece metadata.Piece, tenantID string, readBytes, pieceSize int64) {
	if h.cfg.CachePublisher == nil {
		return
	}
	h.cfg.CachePublisher.Publish(hot_object_cache.PromotionSignal{
		PieceID:        piece.PieceID,
		PieceSizeBytes: pieceSize,
		TenantID:       tenantID,
		ReadBytes:      readBytes,
		ReadCount:      1,
		ObservedAt:     h.cfg.Now(),
		OriginBackend:  piece.Backend,
	})
}

// notifyCacheWarmingExhausted calls the operator-supplied
// OnCacheWarmingBudgetExhausted hook when fetchPiece would have
// warmed the cache inline but the budget guard rejected the
// acquire. Production wires this into a Prometheus counter so a
// chronically-undersized budget shows up as a metric, not a
// silent degradation. The backend argument labels the counter so
// operators can tell which provider is generating the budget
// pressure (same convention as IncIntegrityFailure). Tests use
// the hook to assert the budget guard fired.
func (h *Handler) notifyCacheWarmingExhausted(backend string, pieceSize int64) {
	if h.cfg.OnCacheWarmingBudgetExhausted == nil {
		return
	}
	h.cfg.OnCacheWarmingBudgetExhausted(backend, pieceSize)
}

// Head handles S3 HEAD object.
//
// Per RFC 9110 §13.1, HEAD must return the same response metadata
// that a GET on the same target would, with the message body
// omitted. The Get handler dispatches on manifest type before
// processing Range; HEAD mirrors that dispatch so the headers
// HEAD emits exactly match what the matching Get would emit:
//
//   - Erasure-coded manifests: getErasureCoded serves Range as a
//     206 + Content-Range slice of the reconstructed object (416
//     on an invalid range). HEAD mirrors that — same status and
//     Content-Range with the body omitted. EC GET does NOT set
//     ETag (the manifest's piece hashes are per-shard, not
//     per-object), so HEAD must skip ETag too.
//
//   - Multipart manifests: getMultipart likewise serves Range as a
//     206 + Content-Range slice and does not set ETag. HEAD
//     mirrors both. Objects above maxMultipartInMemoryBytes still
//     return 507 on HEAD to match the GET ceiling.
//
//   - Gateway-encrypted manifests with a Range header:
//     bufferedGatewayDecryptedGet rejects pieces above
//     MaxInMemoryObjectBytes with 507 InsufficientStorage because
//     buffered decryption would defeat the OOM guard.
//     HEAD-with-Range on those objects mirrors that rejection
//     so clients learn up-front the GET will fail.
//
//   - Single-piece (and within-ceiling encrypted): Range parses
//     through parseHTTPRange, returning 416 on invalid /
//     out-of-bounds and 206 + Content-Range + slice-sized
//     Content-Length on success. ETag is set from
//     pieceETag(piece) (the per-piece BLAKE3 hash, which is the
//     same hash GET would set).
//
// HEAD also emits an AuditRecord (operation="HEAD") so the
// compliance trail covers object-existence probes — discovering
// that a key exists is a privacy-sensitive read in zero-knowledge
// deployments, even though no bytes are served.
func (h *Handler) Head(w http.ResponseWriter, r *http.Request) {
	manifest, pieceProvider, piece, tenantID, bucket, err := h.resolve(r)
	if err != nil {
		writeResolveError(w, r, err)
		return
	}

	if h.applyReadConditionals(w, r, manifest, piece) {
		return
	}

	// Mirror Get's metadata chokepoint so HEAD returns the same
	// Content-Type / system / x-amz-meta-* headers as GET would.
	setObjectMetadataHeaders(w, manifest)

	if isErasureCodedManifest(manifest) {
		h.headErasureCoded(w, r, manifest, piece, pieceProvider, tenantID, bucket)
		return
	}
	if isMultipartManifest(manifest) {
		h.headMultipart(w, r, manifest, piece, pieceProvider, tenantID, bucket)
		return
	}

	var byteRange *providers.ByteRange
	if hdr := r.Header.Get("Range"); hdr != "" {
		rng, perr := parseHTTPRange(hdr, manifest.ObjectSize)
		if perr != nil {
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", perr.Error(), r.URL.Path)
			return
		}
		byteRange = rng
	}

	// Gateway-encrypted oversize range: bufferedGatewayDecryptedGet
	// rejects pieces above MaxInMemoryObjectBytes outright because
	// it must buffer the full plaintext to slice an arbitrary
	// range (chunk-level range seek lands in v0.2.0). Mirror that
	// rejection on HEAD so pre-flight probes learn up-front rather
	// than discovering the failure on the GET.
	if IsGatewayEncrypted(manifest.Encryption.Mode) && byteRange != nil && manifest.ObjectSize > MaxInMemoryObjectBytes {
		writeError(w, http.StatusInsufficientStorage, "RangeRequestTooLargeForBufferedDecrypt",
			fmt.Sprintf("range GET on gateway-encrypted object of %d bytes exceeds in-memory decrypt ceiling of %d bytes; full-object GETs stream without this ceiling",
				manifest.ObjectSize, MaxInMemoryObjectBytes),
			r.URL.Path)
		return
	}

	if etag := pieceETag(piece); etag != "" {
		w.Header().Set("ETag", quote(etag))
	}
	w.Header().Set("x-amz-version-id", manifest.VersionID)
	status := http.StatusOK
	if byteRange != nil {
		end := byteRange.End
		if end < 0 {
			end = manifest.ObjectSize - 1
		}
		w.Header().Set("Content-Range", formatContentRange(byteRange, manifest.ObjectSize))
		w.Header().Set("Content-Length", strconv.FormatInt(end-byteRange.Start+1, 10))
		status = http.StatusPartialContent
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(manifest.ObjectSize, 10))
	}
	w.WriteHeader(status)

	h.emit(tenantID, bucket, billing.GetRequests, 1)
	// Audit HEAD with the same shape as GET. Country comes from
	// the resolved provider's placement labels; HEAD never reads
	// piece bytes so there is no integrity check to fail before
	// audit, mirroring the post-resolve / pre-body audit point in
	// Get for the simple single-piece path.
	h.audit(r, "HEAD", tenantID, bucket, manifest.ObjectKey, piece.PieceID, piece.Backend, pieceProvider.PlacementLabels().Country)
}

// headErasureCoded mirrors getErasureCoded's response metadata:
// 206 + Content-Range for a satisfiable Range, 416 for an invalid
// one, 200 + full ObjectSize otherwise, and no ETag in any case. EC
// pieces are per-shard, not per-object, so the gateway has no
// authoritative per-object hash to advertise as ETag (the
// Reed-Solomon decoder reconstructs the object on the fly; the
// matching whole-object hash would be a post-EC computation we
// don't currently persist).
//
// The ErasureCoding-nil guard matches getErasureCoded: an EC
// manifest with no registry configured is unservable, so HEAD
// must surface the misconfiguration as 500 rather than 200,
// otherwise pre-flight probes would pass and the follow-up GET
// would fail.
//
// Audit attribution uses manifest.MigrationState.PrimaryBackend
// — NOT piece.Backend from resolve() — because EC shards are
// scattered across multiple backends and PrimaryBackend is the
// canonical write-side attribution that getErasureCoded also
// uses. Routing through resolve()'s Pieces[0].Backend would
// produce HEAD audit rows that point at a shard host while the
// matching GET points at the primary, fragmenting the audit
// trail.
func (h *Handler) headErasureCoded(
	w http.ResponseWriter,
	r *http.Request,
	manifest *metadata.ObjectManifest,
	_ metadata.Piece,
	_ providers.StorageProvider,
	tenantID, bucket string,
) {
	// Mirror getErasureCoded's three-step configuration validation
	// (erasure_coding.go:258-273): registry present, profile name
	// set on the manifest, profile registered in the registry. Any
	// missing pre-condition that would 500 the GET must 500 the
	// HEAD too — a 200-HEAD-then-500-GET pre-flight gap is the
	// exact divergence this PR's manifest-type dispatch removes.
	if h.cfg.ErasureCoding == nil {
		writeError(w, http.StatusInternalServerError, "ErasureCodingNotConfigured",
			"object is erasure-coded but no registry is configured", r.URL.Path)
		return
	}
	profile := manifest.PlacementPolicy.ErasureProfile
	if profile == "" {
		writeError(w, http.StatusInternalServerError, "ErasureProfileMissing",
			"erasure-coded manifest is missing ErasureProfile", r.URL.Path)
		return
	}
	if _, err := h.cfg.ErasureCoding.Lookup(profile); err != nil {
		writeError(w, http.StatusInternalServerError, "ErasureProfileNotRegistered", err.Error(), r.URL.Path)
		return
	}
	var byteRange *providers.ByteRange
	if hdr := r.Header.Get("Range"); hdr != "" {
		rng, perr := parseHTTPRange(hdr, manifest.ObjectSize)
		if perr != nil {
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", perr.Error(), r.URL.Path)
			return
		}
		byteRange = rng
	}
	w.Header().Set("x-amz-version-id", manifest.VersionID)
	status := http.StatusOK
	if byteRange != nil {
		end := byteRange.End
		if end < 0 {
			end = manifest.ObjectSize - 1
		}
		w.Header().Set("Content-Range", formatContentRange(byteRange, manifest.ObjectSize))
		w.Header().Set("Content-Length", strconv.FormatInt(end-byteRange.Start+1, 10))
		status = http.StatusPartialContent
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(manifest.ObjectSize, 10))
	}
	w.WriteHeader(status)

	h.emit(tenantID, bucket, billing.GetRequests, 1)
	auditBackend, auditPieceID, auditCountry := h.ecAuditAttribution(manifest)
	h.audit(r, "HEAD", tenantID, bucket, manifest.ObjectKey, auditPieceID, auditBackend, auditCountry)
}

// headMultipart mirrors getMultipart's response metadata: 206 +
// Content-Range for a satisfiable Range, 507 above
// maxMultipartInMemoryBytes, otherwise 200 + full ObjectSize, with
// no ETag in any case. Multipart objects don't expose a
// per-object ETag on read (the multipart ETag returned at upload
// completion is a non-cryptographic concatenation hash that isn't
// recoverable from the manifest; matching GET we omit it rather
// than fabricate a Pieces[0]-based value that wouldn't agree with
// PUT's response).
//
// The maxMultipartInMemoryBytes guard exists because getMultipart
// must pre-fetch and concatenate all parts in memory. HEAD never
// touches part bytes, but if HEAD returned 200 on objects above
// the ceiling, the follow-up GET would 507 — exactly the
// pre-flight asymmetry this PR's manifest-type dispatch exists to
// eliminate. Both HEAD and GET reference the same constant in
// erasure_coding.go so the threshold can't drift.
func (h *Handler) headMultipart(
	w http.ResponseWriter,
	r *http.Request,
	manifest *metadata.ObjectManifest,
	_ metadata.Piece,
	_ providers.StorageProvider,
	tenantID, bucket string,
) {
	var byteRange *providers.ByteRange
	if hdr := r.Header.Get("Range"); hdr != "" {
		rng, perr := parseHTTPRange(hdr, manifest.ObjectSize)
		if perr != nil {
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", perr.Error(), r.URL.Path)
			return
		}
		byteRange = rng
	}
	if manifest.ObjectSize > maxMultipartInMemoryBytes {
		writeError(w, http.StatusInsufficientStorage, "MultipartTooLarge",
			fmt.Sprintf("multipart object of %d bytes exceeds in-memory pre-fetch ceiling of %d bytes",
				manifest.ObjectSize, maxMultipartInMemoryBytes),
			r.URL.Path)
		return
	}
	// Pre-flight every part's backend the same way getMultipart
	// does at erasure_coding.go:520-530. Multipart GET has no
	// read-repair / parity fallback — any deregistered backend
	// fails the whole read with 502 BackendNotRegistered. If HEAD
	// 200s on the same manifest, the AWS SDK and CDN pre-flight
	// will route traffic toward a GET that's guaranteed to fail.
	// EC HEAD intentionally skips this kind of check because EC
	// can tolerate up to ParityShards backends being deregistered
	// without DataLoss (the matching GET path triggers
	// read-repair to land surviving shards on a new primary), so
	// mirroring the GET-side feasibility check on HEAD would
	// produce false-fail pre-flights on read-repairable manifests.
	for _, p := range manifest.Pieces {
		if _, ok := h.cfg.Providers[p.Backend]; !ok {
			writeError(w, http.StatusBadGateway, "BackendNotRegistered",
				fmt.Sprintf("part %d references unregistered backend %q", p.PartNumber, p.Backend),
				r.URL.Path)
			return
		}
	}
	w.Header().Set("x-amz-version-id", manifest.VersionID)
	status := http.StatusOK
	if byteRange != nil {
		end := byteRange.End
		if end < 0 {
			end = manifest.ObjectSize - 1
		}
		w.Header().Set("Content-Range", formatContentRange(byteRange, manifest.ObjectSize))
		w.Header().Set("Content-Length", strconv.FormatInt(end-byteRange.Start+1, 10))
		status = http.StatusPartialContent
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(manifest.ObjectSize, 10))
	}
	w.WriteHeader(status)

	h.emit(tenantID, bucket, billing.GetRequests, 1)
	auditBackend, auditPieceID, auditCountry := h.multipartAuditAttribution(manifest)
	h.audit(r, "HEAD", tenantID, bucket, manifest.ObjectKey, auditPieceID, auditBackend, auditCountry)
}

// Delete handles S3 DELETE object.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	bucket, key := parseBucketKey(r.URL.Path)
	if bucket == "" || key == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "path must be /{bucket}/{key...}", r.URL.Path)
		return
	}
	if h.cfg.Manifests == nil {
		writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable", "manifest store not configured", r.URL.Path)
		return
	}

	// Versioning-enabled DELETE without an explicit ?versionId does
	// not remove data: it inserts a delete marker as a new latest
	// version (WS8.4), which hides older versions from
	// GET/HEAD/ListObjectsV2 while preserving them for
	// ListObjectVersions and versionId-addressed reads. This mirrors
	// AWS S3, where the marker is created even if the key has no
	// current version, so we skip the Get/legal-hold path entirely.
	// An explicit ?versionId still performs a permanent version
	// delete below (and is still subject to legal hold).
	versioning, verr := h.bucketVersioning(r.Context(), tenantID, bucket)
	if verr != nil {
		writeError(w, http.StatusInternalServerError, "VersioningLookupFailed", verr.Error(), r.URL.Path)
		return
	}
	if versioning == bucket_config.VersioningEnabled && r.URL.Query().Get("versionId") == "" {
		markerID := newPieceID(tenantID, bucket, key, h.cfg.Now())
		marker := &metadata.ObjectManifest{
			TenantID:      tenantID,
			Bucket:        bucket,
			ObjectKey:     key,
			ObjectKeyHash: hashObjectKey(key),
			VersionID:     markerID,
			DeleteMarker:  true,
			CreatedAt:     h.cfg.Now(),
		}
		mkey := manifest_store.ManifestKey{
			TenantID:      tenantID,
			Bucket:        bucket,
			ObjectKeyHash: hashObjectKey(key),
			VersionID:     markerID,
		}
		if err := h.cfg.Manifests.Put(r.Context(), mkey, marker); err != nil {
			writeError(w, http.StatusInternalServerError, "DeleteMarkerPutFailed", err.Error(), r.URL.Path)
			return
		}
		w.Header().Set("x-amz-delete-marker", "true")
		w.Header().Set("x-amz-version-id", markerID)
		h.emit(tenantID, bucket, billing.DeleteRequests, 1)
		h.audit(r, "DELETE", tenantID, bucket, key, "", "", "")
		h.notify(r, eventObjectRemovedDeleteMarker, tenantID, bucket, key, "", markerID, 0)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if h.cfg.Compliance.LegalHoldStore != nil {
		holds, herr := h.cfg.Compliance.LegalHoldStore.Active(r.Context(), tenantID, bucket, key)
		if herr != nil {
			writeError(w, http.StatusInternalServerError, "LegalHoldCheckFailed", herr.Error(), r.URL.Path)
			return
		}
		if len(holds) > 0 {
			writeError(w, http.StatusForbidden, "ObjectUnderLegalHold", "object is under an active legal hold", r.URL.Path)
			return
		}
	}

	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: hashObjectKey(key),
		VersionID:     r.URL.Query().Get("versionId"),
	}
	// Latest version: leave VersionID empty; concrete stores resolve
	// the current version by (tenant, bucket, object_key_hash). The
	// Postgres and in-memory implementations both honour that. When
	// the request includes ?versionId=, that specific version is
	// targeted instead.
	manifest, err := h.cfg.Manifests.Get(r.Context(), mkey)
	if err != nil {
		// S3 DeleteObject is idempotent: a missing object is a
		// success. Surface other errors.
		if errors.Is(err, manifest_store.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, "ManifestGetFailed", err.Error(), r.URL.Path)
		return
	}
	// Object Lock enforcement (WS8.3): a version under an active
	// retention or a legal hold cannot be permanently deleted.
	// GOVERNANCE retention can be bypassed with the bypass header;
	// COMPLIANCE and legal holds cannot. This applies to permanent
	// version deletes only — a versioning-enabled DELETE that inserts
	// a delete marker (handled above) preserves the locked version and
	// is always allowed.
	if msg, locked := objectLockBlocksDelete(manifest, h.cfg.Now(), governanceBypassRequested(r)); locked {
		writeError(w, http.StatusForbidden, "AccessDenied", msg, r.URL.Path)
		return
	}
	// Delete the manifest first so a mid-delete failure leaves orphan
	// backend pieces (recoverable by GC) rather than a zombie manifest
	// pointing at pieces that no longer exist.
	mkey.VersionID = manifest.VersionID
	if err := h.cfg.Manifests.Delete(r.Context(), mkey); err != nil && !errors.Is(err, manifest_store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "ManifestDeleteFailed", err.Error(), r.URL.Path)
		return
	}
	// Reference-counted DELETE: when the manifest carries a
	// ContentHash AND the gateway has a content_index store
	// wired, decrement the per-(tenant, content_hash) refcount.
	// The piece is removed from the backend only when the new
	// count reaches zero. Manifests that predate Phase 3.5 (no
	// ContentHash) take the original path and delete pieces
	// directly.
	// piecesDeleted records whether bytes were actually removed from
	// the backend. It gates the Wasabi early-delete warning below: a
	// refcount decrement that leaves the piece in place (still
	// referenced by another manifest) incurs no early-delete charge,
	// so emitting the warning then would be a false positive.
	piecesDeleted := false
	if manifest.ContentHash != "" && h.cfg.ContentIndex != nil {
		newCount, derr := h.cfg.ContentIndex.DecrementRef(r.Context(), tenantID, manifest.ContentHash)
		switch {
		case errors.Is(derr, content_index.ErrNotFound):
			// Index row is gone but the manifest still
			// pointed at it — fall through to a
			// best-effort piece delete to clean up.
			h.deletePiecesBestEffort(r, manifest)
			piecesDeleted = true
		case derr != nil:
			writeError(w, http.StatusInternalServerError, "ContentIndexDecrementFailed", derr.Error(), r.URL.Path)
			return
		case newCount == 0:
			// Last reference: drop the index row FIRST and
			// only delete the backend piece on a successful
			// conditional Delete. This closes the race where
			// a concurrent PUT does Lookup+IncrementRef
			// between our DecrementRef returning 0 and the
			// piece deletion: if the racer wins, our Delete
			// reports ErrRefCountNonZero and we leave the
			// piece in place for the new manifest.
			delErr := h.cfg.ContentIndex.Delete(r.Context(), tenantID, manifest.ContentHash)
			switch {
			case delErr == nil:
				h.deletePiecesBestEffort(r, manifest)
				piecesDeleted = true
			case errors.Is(delErr, content_index.ErrNotFound):
				// Row vanished out from under us (e.g.
				// background GC). The piece may still
				// be referenced elsewhere — best-effort
				// cleanup is unsafe, so leave it.
			case errors.Is(delErr, content_index.ErrRefCountNonZero):
				// A concurrent uploader bumped the
				// refcount; the piece is still needed.
				// Skip the backend delete entirely.
			default:
				writeError(w, http.StatusInternalServerError, "ContentIndexDeleteFailed", delErr.Error(), r.URL.Path)
				return
			}
		default:
			// newCount > 0: the piece is still referenced
			// by another manifest in this tenant. Leave
			// it on the backend.
			h.emit(tenantID, bucket, billing.DedupRefCount, uint64(newCount))
		}
	} else {
		h.deletePiecesBestEffort(r, manifest)
		piecesDeleted = true
	}

	h.emit(tenantID, bucket, billing.DeleteRequests, 1)
	if len(manifest.Pieces) > 0 {
		p := manifest.Pieces[0]
		var country string
		if prov, ok := h.cfg.Providers[p.Backend]; ok {
			country = prov.PlacementLabels().Country
		}
		h.audit(r, "DELETE", tenantID, bucket, key, p.PieceID, p.Backend, country)
	}
	// Warn-only: if bytes were actually removed from a Wasabi backend
	// and the object is still inside the 90-day minimum storage
	// window, surface the otherwise-hidden early-delete charge as
	// response headers. Skipped when content-dedup kept the pieces in
	// place (no charge applies). The delete already succeeded above;
	// this never blocks it.
	if piecesDeleted {
		h.setWasabiEarlyDeleteWarning(w, manifest)
	}
	h.notify(r, eventObjectRemovedDelete, tenantID, bucket, key, "", manifest.VersionID, 0)
	w.WriteHeader(http.StatusNoContent)
}

// setWasabiEarlyDeleteWarning sets informational response headers when
// a just-deleted object resided on a Wasabi backend and is still
// within Wasabi's 90-day minimum storage window. Operators billed for
// the residual window otherwise have no signal that the delete was
// "early". The headers are advisory only: callers must NOT treat them
// as an error, and the DELETE is never blocked on their account.
//
// Backend identity is resolved through the provider's PlacementLabels
// (Provider == "wasabi") rather than the registry key, so region-
// qualified keys such as "wasabi-ap-southeast-1" are matched too.
func (h *Handler) setWasabiEarlyDeleteWarning(w http.ResponseWriter, manifest *metadata.ObjectManifest) {
	if manifest == nil {
		return
	}
	onWasabi := false
	for _, piece := range manifest.Pieces {
		prov, ok := h.cfg.Providers[piece.Backend]
		if ok && prov.PlacementLabels().Provider == "wasabi" {
			onWasabi = true
			break
		}
	}
	if !onWasabi {
		return
	}
	warn := wasabi.MinStorageDurationWarning(manifest.CreatedAt, h.cfg.Now())
	if !warn.WithinMinStorageWindow {
		return
	}
	w.Header().Set("X-Zkof-Wasabi-Early-Delete-Warning", "true")
	w.Header().Set("X-Zkof-Wasabi-Min-Storage-Remaining-Days", strconv.Itoa(warn.RemainingDays))
}

// deletePiecesBestEffort removes every piece referenced by the
// manifest from its backend. Errors are swallowed: the manifest is
// already gone so any surviving pieces are user-invisible orphans
// that the GC sweep picks up.
func (h *Handler) deletePiecesBestEffort(r *http.Request, manifest *metadata.ObjectManifest) {
	for _, piece := range manifest.Pieces {
		provider, ok := h.cfg.Providers[piece.Backend]
		if !ok {
			continue
		}
		_ = provider.DeletePiece(r.Context(), piece.PieceID)
	}
}

// List handles S3 LIST bucket (ListObjectsV2). It is exported so
// tests can invoke it directly; normal traffic arrives through
// dispatch.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	bucket, _ := parseBucketKey(r.URL.Path)
	h.listBucket(w, r, bucket)
}

func (h *Handler) listBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	if bucket == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "LIST requires a bucket", r.URL.Path)
		return
	}
	if h.cfg.Manifests == nil {
		writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable", "manifest store not configured", r.URL.Path)
		return
	}

	cursor := r.URL.Query().Get("continuation-token")
	limit := 1000
	if s := r.URL.Query().Get("max-keys"); s != "" {
		if n, perr := strconv.Atoi(s); perr == nil && n > 0 {
			limit = n
		}
	}
	page, err := h.cfg.Manifests.List(r.Context(), tenantID, bucket, cursor, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ManifestListFailed", err.Error(), r.URL.Path)
		return
	}

	type content struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
		ETag string `xml:"ETag,omitempty"`
	}
	type response struct {
		XMLName               xml.Name  `xml:"ListBucketResult"`
		Name                  string    `xml:"Name"`
		Contents              []content `xml:"Contents"`
		IsTruncated           bool      `xml:"IsTruncated"`
		NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
	}

	resp := response{Name: bucket, IsTruncated: page.NextCursor != "", NextContinuationToken: page.NextCursor}
	for _, m := range page.Manifests {
		// Return the opaque object key (plaintext under managed
		// encryption, ciphertext under strict ZK) so a follow-up
		// GET /{bucket}/{key} hashes it back to the stored
		// ObjectKeyHash. Manifests written before ObjectKey existed
		// are unreachable via S3 LIST — clients would have to use
		// ObjectKeyHash directly, which does not round-trip through
		// the gateway's hashing step; skip them rather than return
		// a key that produces 404 on GET.
		if m.ObjectKey == "" {
			continue
		}
		// A delete marker as the latest version hides the key from
		// ListObjectsV2 (WS8.4); it remains visible via
		// ListObjectVersions. List returns latest-per-key, so a
		// marker here means the object is logically deleted.
		if m.DeleteMarker {
			continue
		}
		c := content{Key: m.ObjectKey, Size: m.ObjectSize}
		if len(m.Pieces) > 0 {
			c.ETag = quote(pieceETag(m.Pieces[0]))
		}
		resp.Contents = append(resp.Contents, c)
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(resp)

	h.emit(tenantID, bucket, billing.ListRequests, 1)
}

// resolve performs the common lookup path for GET/HEAD: authenticate,
// parse path, fetch the manifest, and pick the primary backend.
func (h *Handler) resolve(r *http.Request) (*metadata.ObjectManifest, providers.StorageProvider, metadata.Piece, string, string, error) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		// Map auth errors to typed *httpError at the boundary so
		// writeResolveError can dispatch uniformly. The wrapped
		// err field carries the original sentinel through the
		// error chain — errors.Is(returnedErr, ErrAuthMisconfigured)
		// still works for any downstream caller that wants to
		// special-case the misconfiguration signal (today only the
		// HTTP status code rendering needs that, but the chain is
		// preserved for forward-compat).
		if errors.Is(err, ErrAuthMisconfigured) {
			return nil, nil, metadata.Piece{}, "", "", &httpError{code: http.StatusInternalServerError, s3code: "InternalAuthMisconfigured", msg: err.Error(), err: err}
		}
		return nil, nil, metadata.Piece{}, "", "", &httpError{code: http.StatusForbidden, s3code: "AccessDenied", msg: err.Error(), err: err}
	}
	bucket, key := parseBucketKey(r.URL.Path)
	if bucket == "" || key == "" {
		return nil, nil, metadata.Piece{}, "", "", &httpError{code: http.StatusBadRequest, s3code: "InvalidArgument", msg: "path must be /{bucket}/{key...}"}
	}
	if h.cfg.Manifests == nil {
		return nil, nil, metadata.Piece{}, "", "", &httpError{code: http.StatusServiceUnavailable, s3code: "ServiceUnavailable", msg: "manifest store not configured"}
	}

	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: hashObjectKey(key),
		VersionID:     r.URL.Query().Get("versionId"),
	}
	manifest, err := h.cfg.Manifests.Get(r.Context(), mkey)
	if err != nil {
		if errors.Is(err, manifest_store.ErrNotFound) {
			return nil, nil, metadata.Piece{}, "", "", &httpError{code: http.StatusNotFound, s3code: "NoSuchKey", msg: "no such key"}
		}
		return nil, nil, metadata.Piece{}, "", "", &httpError{code: http.StatusInternalServerError, s3code: "ManifestGetFailed", msg: err.Error()}
	}
	// Delete markers (WS8.4) carry no payload. AWS S3 returns 404
	// NoSuchKey when the *latest* version is a delete marker (an
	// unversioned GET/HEAD), and 405 MethodNotAllowed when a delete
	// marker is addressed directly by ?versionId. Both responses set
	// x-amz-delete-marker: true and echo the marker's version id.
	if manifest.DeleteMarker {
		hdrs := map[string]string{"x-amz-delete-marker": "true"}
		if manifest.VersionID != "" {
			hdrs["x-amz-version-id"] = manifest.VersionID
		}
		if r.URL.Query().Get("versionId") != "" {
			return nil, nil, metadata.Piece{}, "", "", &httpError{code: http.StatusMethodNotAllowed, s3code: "MethodNotAllowed", msg: "the specified version is a delete marker and cannot be downloaded", headers: hdrs}
		}
		return nil, nil, metadata.Piece{}, "", "", &httpError{code: http.StatusNotFound, s3code: "NoSuchKey", msg: "no such key", headers: hdrs}
	}
	if len(manifest.Pieces) == 0 {
		return nil, nil, metadata.Piece{}, "", "", &httpError{code: http.StatusInternalServerError, s3code: "EmptyManifest", msg: "manifest has no pieces"}
	}
	piece := manifest.Pieces[0]
	provider, ok := h.cfg.Providers[piece.Backend]
	if !ok {
		return nil, nil, metadata.Piece{}, "", "", &httpError{code: http.StatusInternalServerError, s3code: "BackendNotRegistered", msg: "backend " + piece.Backend + " is not in the provider registry"}
	}
	return manifest, provider, piece, tenantID, bucket, nil
}

// authMemo caches the outcome of authenticate for a single request so
// repeated calls (applyCORS then the operation handler) do not redo
// the SigV4 HMAC. It is never shared across requests and is read by a
// single goroutine, so it needs no locking.
type authMemo struct {
	done   bool
	tenant string
	err    error
}

type authMemoKeyType struct{}

var authMemoKey authMemoKeyType

// withAuthMemo returns r carrying a fresh, request-scoped auth memo
// cell. dispatch installs it so authenticate can be called more than
// once per request without paying for the HMAC twice.
func withAuthMemo(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authMemoKey, &authMemo{}))
}

func (h *Handler) authenticate(r *http.Request) (string, error) {
	memo, _ := r.Context().Value(authMemoKey).(*authMemo)
	if memo == nil {
		return h.authenticateNow(r)
	}
	if !memo.done {
		memo.tenant, memo.err = h.authenticateNow(r)
		memo.done = true
	}
	return memo.tenant, memo.err
}

func (h *Handler) authenticateNow(r *http.Request) (string, error) {
	if h.cfg.Auth == nil {
		// Production-mode safety net: when an operator
		// configures RequireAuth=true (cmd/gateway does this
		// automatically when env="production") but forgets
		// to wire an Authenticator, refuse every request
		// instead of silently dropping into AnonymousTenant.
		if h.cfg.RequireAuth {
			return "", ErrAuthMisconfigured
		}
		return AnonymousTenant, nil
	}
	return h.cfg.Auth.Authenticate(r)
}

// writeAuthError translates an authenticate() error into the
// right HTTP response. Auth-misconfiguration (operator forgot to
// wire an Authenticator under RequireAuth=true) is a 500
// InternalAuthMisconfigured because the server is the broken
// party; everything else is a 403 AccessDenied (the request
// itself is malformed or signed with bad credentials).
func writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrAuthMisconfigured) {
		writeError(w, http.StatusInternalServerError, "InternalAuthMisconfigured", err.Error(), r.URL.Path)
		return
	}
	writeError(w, http.StatusForbidden, "AccessDenied", err.Error(), r.URL.Path)
}

func (h *Handler) emit(tenantID, bucket string, dim billing.Dimension, delta uint64) {
	if h.cfg.Billing == nil {
		return
	}
	h.cfg.Billing.Emit(billing.UsageEvent{
		TenantID:     tenantID,
		Bucket:       bucket,
		Dimension:    dim,
		Delta:        delta,
		ObservedAt:   h.cfg.Now(),
		SourceNodeID: h.cfg.NodeID,
	})
}

// notify hands one object-mutation event to the notification emitter,
// if any, for asynchronous fan-out to the bucket's configured webhook
// destinations (WS8.6). Like audit it is best-effort and called only on
// the success path; Emit is non-blocking, so this never adds latency to
// the request. eventName is the specific leaf S3 event name (e.g.
// "s3:ObjectCreated:Put").
func (h *Handler) notify(r *http.Request, eventName, tenantID, bucket, key, etag, versionID string, size int64) {
	if h.cfg.Notifications == nil {
		return
	}
	h.cfg.Notifications.Emit(ObjectEvent{
		TenantID:  tenantID,
		Bucket:    bucket,
		ObjectKey: key,
		EventName: eventName,
		SizeBytes: size,
		ETag:      etag,
		VersionID: versionID,
		RequestID: requestid.FromContext(r.Context()),
		SourceIP:  clientIP(r),
	})
}

// clientIP extracts the originating client address for an event
// record's sourceIPAddress field. It strips the port from RemoteAddr
// and is best-effort: an empty string is fine in the payload.
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// parseBucketKey splits /{bucket}/{key...}. Leading slashes are
// trimmed; missing components return empty strings.
func parseBucketKey(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", ""
	}
	i := strings.IndexByte(p, '/')
	if i < 0 {
		return p, ""
	}
	return p[:i], p[i+1:]
}

// hashObjectKey returns the SHA-256 hex digest of the plaintext key.
// The manifest stores only the hash (docs/PROPOSAL.md §3.3) so the
// control plane never sees the plaintext key. S3 keys are opaque
// byte strings, so the hash is computed over the raw key without
// any path normalization — distinct keys (e.g. "a//b" vs "a/b")
// must hash to distinct values.
func hashObjectKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// NewVersionID mints a version ID using the same scheme as the
// interactive write path, so a delete marker inserted by the
// background lifecycle evaluator is indistinguishable from one
// created by an interactive DELETE. cmd/gateway wires this into the
// evaluator's Config.NewVersionID.
func NewVersionID(tenantID, bucket, key string, now time.Time) string {
	return newPieceID(tenantID, bucket, key, now)
}

// newPieceID mints a deterministic-looking but unique piece ID for a
// new object. Phase 2's client SDK will later hand the gateway a
// BLAKE3 hash of the ciphertext; this helper unblocks the gateway
// until that lands.
func newPieceID(tenantID, bucket, key string, now time.Time) string {
	sum := sha256.Sum256([]byte(tenantID + "\x00" + bucket + "\x00" + key + "\x00" + strconv.FormatInt(now.UnixNano(), 10)))
	return hex.EncodeToString(sum[:])
}

// parseHTTPRange parses a single-range HTTP Range header
// ("bytes=start-end", "bytes=start-", or the suffix form "bytes=-N")
// into a providers.ByteRange. Multi-range requests are not supported.
//
// Suffix ranges request the final N bytes of the object and are
// resolved here against the known object size into an absolute
// [size-N, size-1] range, so every downstream consumer (Content-Range
// / Content-Length formatting, the backend ByteRange fetch) sees
// concrete endpoints and needs no suffix-awareness. Per RFC 7233
// §2.1 a suffix length at or beyond the object size returns the whole
// object, and a zero suffix length is unsatisfiable.
func parseHTTPRange(h string, size int64) (*providers.ByteRange, error) {
	if !strings.HasPrefix(h, "bytes=") {
		return nil, fmt.Errorf("invalid range header %q", h)
	}
	spec := strings.TrimPrefix(h, "bytes=")
	if strings.Contains(spec, ",") {
		return nil, fmt.Errorf("multi-range requests are not supported")
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return nil, fmt.Errorf("invalid range header %q", h)
	}
	startStr, endStr := spec[:dash], spec[dash+1:]
	if startStr == "" {
		// Suffix range ("bytes=-N"): the final N bytes of the object.
		if endStr == "" {
			return nil, fmt.Errorf("invalid range header %q", h)
		}
		suffix, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid suffix length: %w", err)
		}
		if suffix < 0 {
			return nil, fmt.Errorf("invalid range header %q", h)
		}
		// A suffix at or beyond the object size yields the whole
		// object; clamp before the zero check so a >size suffix on a
		// non-empty object stays satisfiable.
		if suffix > size {
			suffix = size
		}
		if suffix == 0 {
			return nil, fmt.Errorf("unsatisfiable suffix range %q for size %d", h, size)
		}
		return &providers.ByteRange{Start: size - suffix, End: size - 1}, nil
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid range start: %w", err)
	}
	end := int64(-1)
	if endStr != "" {
		end, err = strconv.ParseInt(endStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid range end: %w", err)
		}
		if end >= size {
			end = size - 1
		}
	}
	if start < 0 || start >= size || (end >= 0 && start > end) {
		return nil, fmt.Errorf("invalid range [%d,%d]", start, end)
	}
	return &providers.ByteRange{Start: start, End: end}, nil
}

func formatContentRange(r *providers.ByteRange, total int64) string {
	end := r.End
	if end < 0 {
		end = total - 1
	}
	return fmt.Sprintf("bytes %d-%d/%d", r.Start, end, total)
}

// etagListMatch reports whether etag (the gateway's advertised ETag in
// its quoted wire form, e.g. `"abc123"`) satisfies a comma-separated
// If-Match / If-None-Match list of entity-tags. The "*" wildcard is
// handled by the caller.
//
// When strong is true the comparison follows RFC 7232 §2.3.2 strong
// comparison: a weak validator ("W/"-prefixed) on either side never
// matches. If-Match (and copy-source-if-match) require strong
// comparison. When strong is false the weak indicator is ignored on
// both sides — the weak comparison RFC 7232 §3.2 prescribes for
// If-None-Match. In practice the gateway only ever mints strong tags,
// so the distinction only affects clients that send a weak validator.
func etagListMatch(headerValue, etag string, strong bool) bool {
	if etag == "" {
		return false
	}
	etagWeak := strings.HasPrefix(etag, "W/")
	want := strings.TrimPrefix(etag, "W/")
	for _, tok := range strings.Split(headerValue, ",") {
		tok = strings.TrimSpace(tok)
		tokWeak := strings.HasPrefix(tok, "W/")
		if strong && (tokWeak || etagWeak) {
			continue
		}
		if strings.TrimPrefix(tok, "W/") == want {
			return true
		}
	}
	return false
}

// honorIfRange implements the RFC 7233 §3.2 If-Range validator test: it
// reports whether the client's If-Range value still matches the
// selected representation, in which case the paired Range request is
// served as a 206. An entity-tag uses strong comparison (§3.2 forbids a
// weak validator in If-Range, so one never honors the range); a date
// matches when the representation has not been modified after it. etag
// is the object's advertised ETag in quoted wire form ("" when the
// object exposes none, e.g. erasure-coded / multipart objects).
func honorIfRange(ifRange, etag string, lastModified time.Time) bool {
	ir := strings.TrimSpace(ifRange)
	if ir == "" {
		return false
	}
	// A weak validator is invalid in If-Range; an entity-tag is
	// recognised by its leading DQUOTE, anything else is an HTTP-date.
	if strings.HasPrefix(ir, "W/") {
		return false
	}
	if strings.HasPrefix(ir, `"`) {
		return etag != "" && ir == etag
	}
	t, err := http.ParseTime(ir)
	if err != nil {
		return false
	}
	return !lastModified.Truncate(time.Second).After(t)
}

// applyIfRange implements RFC 7233 §3.2: when a request carries both
// Range and If-Range, the Range is honored only if the If-Range
// validator still matches the selected representation. When it does
// not, the Range header is dropped so the caller serves the full 200
// response instead of a 206. A request missing either header is left
// untouched.
func applyIfRange(r *http.Request, etag string, lastModified time.Time) {
	if r.Header.Get("Range") == "" || r.Header.Get("If-Range") == "" {
		return
	}
	if !honorIfRange(r.Header.Get("If-Range"), etag, lastModified) {
		r.Header.Del("Range")
	}
}

// evaluateConditionalRead applies the RFC 7232 / S3 GetObject
// conditional-request headers to a GET or HEAD whose target object has
// already been resolved. etag is the object ETag the gateway advertises
// in its quoted wire form ("" when the object exposes none, e.g.
// erasure-coded / multipart objects); lastModified is the stored
// CreatedAt.
//
// It returns (304, true) or (412, true) when a precondition diverts the
// response, and (0, false) when the read proceeds normally.
//
// Precedence follows RFC 7232 §6 (and matches AWS S3): the 412 group
// (If-Match, then If-Unmodified-Since) is evaluated before the 304
// group (If-None-Match, then If-Modified-Since); within each group the
// entity-tag header takes precedence and the date header is ignored
// when both are present. Last-modified comparisons are truncated to
// whole seconds so they agree with the second-resolution Last-Modified
// header the gateway emits.
func evaluateConditionalRead(hdr http.Header, etag string, lastModified time.Time) (int, bool) {
	modifiedAfter := func(raw string) (modified bool, ok bool) {
		t, err := http.ParseTime(raw)
		if err != nil {
			// An unparseable date makes the conditional inoperative
			// (RFC 7232 §3.3/§3.4): ignore the header.
			return false, false
		}
		return lastModified.Truncate(time.Second).After(t), true
	}

	// 412 group: If-Match wins over If-Unmodified-Since. If-Match uses
	// strong comparison (RFC 7232 §3.1).
	if im := hdr.Get("If-Match"); im != "" {
		if strings.TrimSpace(im) != "*" && !etagListMatch(im, etag, true) {
			return http.StatusPreconditionFailed, true
		}
	} else if ius := hdr.Get("If-Unmodified-Since"); ius != "" {
		if modified, ok := modifiedAfter(ius); ok && modified {
			return http.StatusPreconditionFailed, true
		}
	}

	// 304 group: If-None-Match wins over If-Modified-Since. If-None-Match
	// uses weak comparison (RFC 7232 §3.2).
	if inm := hdr.Get("If-None-Match"); inm != "" {
		if strings.TrimSpace(inm) == "*" || etagListMatch(inm, etag, false) {
			return http.StatusNotModified, true
		}
	} else if ims := hdr.Get("If-Modified-Since"); ims != "" {
		if modified, ok := modifiedAfter(ims); ok && !modified {
			return http.StatusNotModified, true
		}
	}

	return 0, false
}

// writeErrCapturingWriter wraps an io.Writer and remembers the first
// Write error it sees. streamGatewayDecryptedGet uses this to tell
// whether an io.Copy failure came from the downstream
// http.ResponseWriter (client disconnect / broken pipe — transport
// hiccup) or from the upstream decryptor (chunk-AEAD reject — real
// corruption). The two cases need different integrity-counter
// behaviour:
//
//   - Write failure ⇒ TeeReader hashed only a ciphertext prefix, so a
//     post-EOF BLAKE3 check would always mismatch. Suppress the
//     integrity counter to avoid false positives.
//   - Read failure with no write failure ⇒ the read-side error is the
//     integrity signal; record an integrity failure directly.
//
// We intentionally do not implement http.Flusher / http.Hijacker /
// http.Pusher here: streamGatewayDecryptedGet never relies on those
// optional interfaces, and forwarding them would only widen the
// blast radius if a future handler accidentally took a dependency on
// them through this wrapper.
type writeErrCapturingWriter struct {
	w        io.Writer
	writeErr error
}

func (e *writeErrCapturingWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil && e.writeErr == nil {
		e.writeErr = err
	}
	return n, err
}

func quote(s string) string {
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return s
	}
	return `"` + s + `"`
}

// httpError is the internal error type returned by resolve so the
// handler method can choose the right HTTP status code. The
// optional err field carries the underlying error chain so
// errors.Is / errors.As work through wrapped httpErrors —
// previously, resolve() and writeResolveError each had to
// errors.Is(err, ErrAuthMisconfigured) before falling through
// to the httpError dispatch. Wrapping the sentinel into the
// httpError at the boundary (resolve) means writeResolveError
// can dispatch uniformly via *httpError and the sentinel-check
// duplication goes away.
type httpError struct {
	code   int
	s3code string
	msg    string
	err    error
	// headers carries extra response headers that must accompany
	// the error (e.g. x-amz-delete-marker / x-amz-version-id on a
	// GET/HEAD that resolves to a delete marker). May be nil.
	headers map[string]string
}

func (e *httpError) Error() string { return e.msg }

// Unwrap exposes the underlying error so errors.Is and errors.As
// can match sentinels (e.g. ErrAuthMisconfigured) carried through
// a typed *httpError. Returns nil when no underlying error is
// attached, which is the common case for synthesised errors like
// "path must be /{bucket}/{key...}".
func (e *httpError) Unwrap() error { return e.err }

func writeResolveError(w http.ResponseWriter, r *http.Request, err error) {
	var he *httpError
	if errors.As(err, &he) {
		for k, v := range he.headers {
			w.Header().Set(k, v)
		}
		writeError(w, he.code, he.s3code, he.msg, r.URL.Path)
		return
	}
	writeError(w, http.StatusInternalServerError, "InternalError", err.Error(), r.URL.Path)
}

// s3ErrorResponse is the minimal S3 XML error shape. Phase 2+ widens
// it with RequestId and HostId.
type s3ErrorResponse struct {
	XMLName  xml.Name `xml:"Error"`
	Code     string   `xml:"Code"`
	Message  string   `xml:"Message"`
	Resource string   `xml:"Resource,omitempty"`
}

func writeError(w http.ResponseWriter, httpCode int, s3Code, message, resource string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(httpCode)
	_ = xml.NewEncoder(w).Encode(s3ErrorResponse{Code: s3Code, Message: message, Resource: resource})
}

// writeBodyReadError converts a read-body error into the right
// S3 error response. When the error is *http.MaxBytesError (the
// request body exceeded Config.MaxRequestBytes), it surfaces 413
// EntityTooLarge with the limit in the message so the caller can
// pick the right chunk size on retry; otherwise it surfaces 400
// InvalidArgument with the underlying error string. Callers
// should `return` immediately after invoking this helper.
// Callers that have already written a body must not call this —
// Go's net/http will refuse to set the status code twice and the
// client will see a truncated response with the wrong code.
func writeBodyReadError(w http.ResponseWriter, r *http.Request, err error) {
	if writeMaxBytesError(w, r, err) {
		return
	}
	writeError(w, http.StatusBadRequest, "InvalidArgument", "read body: "+err.Error(), r.URL.Path)
}

// writePutPieceError converts a PutPiece backend error into the
// right S3 response. Streaming PutPiece reads from the
// MaxBytesReader-wrapped body, so the client overflowing the
// per-request cap surfaces here as *http.MaxBytesError (not on
// the body read because the read happens inside the provider).
// We surface 413 in that case so the caller sees the same
// EntityTooLarge as the body-read path; everything else is a
// genuine backend failure and maps to 502 BackendPutFailed.
// Callers should `return` immediately after invoking this helper.
func writePutPieceError(w http.ResponseWriter, r *http.Request, err error) {
	if writeMaxBytesError(w, r, err) {
		return
	}
	writeError(w, http.StatusBadGateway, "BackendPutFailed", err.Error(), r.URL.Path)
}

// writeMaxBytesError writes a 413 EntityTooLarge response if err
// wraps *http.MaxBytesError and returns true. Returns false
// without writing anything when err is some other error type,
// letting the caller fall through to its protocol-specific
// default response. Sharing this between writeBodyReadError and
// writePutPieceError keeps the 413 limit-string format
// consistent across every PUT/POST surface.
func writeMaxBytesError(w http.ResponseWriter, r *http.Request, err error) bool {
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		writeError(w, http.StatusRequestEntityTooLarge,
			"EntityTooLarge",
			fmt.Sprintf("request body exceeds the configured MaxRequestBytes limit of %d bytes", mb.Limit),
			r.URL.Path)
		return true
	}
	return false
}

// pieceETag returns the S3-protocol ETag for a piece. It prefers
// ProviderETag (the opaque ETag the storage backend returned on PUT)
// so clients see the same value they received in the PUT response.
// Legacy manifests written before BLAKE3 hashing was added have
// ProviderETag="" and Hash set to the provider ETag; this falls
// through cleanly.
//
// Defence in depth: the fallback explicitly refuses to leak a
// "blake3:<hex>" hash to the client. Today no write-path produces
// a Piece with Hash set to a blake3-prefixed value and ProviderETag
// empty, but if a future migration regresses that invariant we want
// the client to see an empty ETag (which the S3 SDK treats as a
// missing header) instead of a non-standard blake3 value that
// would confuse strict ETag-matching clients.
func pieceETag(p metadata.Piece) string {
	if p.ProviderETag != "" {
		return p.ProviderETag
	}
	if strings.HasPrefix(p.Hash, "blake3:") {
		return ""
	}
	return p.Hash
}
