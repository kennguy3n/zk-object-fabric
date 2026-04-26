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
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/migration/lazy_read_repair"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// Authenticator verifies the identity claimed by an S3 request and
// returns the tenant ID bound to it. A nil Authenticator causes the
// handler to fall back to AnonymousTenant (used only in dev).
type Authenticator interface {
	Authenticate(r *http.Request) (tenantID string, err error)
}

// AnonymousTenant is the tenant ID the handler uses when no
// Authenticator is configured. Deployments MUST configure an
// Authenticator in production.
const AnonymousTenant = "anonymous"

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

	// Multipart is the server-side multipart-upload session store.
	// Required for CreateMultipartUpload / UploadPart /
	// CompleteMultipartUpload / AbortMultipartUpload. A nil store
	// causes those endpoints to return 501 NotImplemented.
	Multipart multipart.Store

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

	// Now, if set, returns the current time. Tests override it to
	// make manifests deterministic.
	Now func() time.Time
}

// Handler routes S3-compatible requests to the gateway's internal
// pipeline.
type Handler struct {
	cfg Config
}

// New returns a Handler ready to be wired into an HTTP mux.
func New(cfg Config) *Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Handler{cfg: cfg}
}

// Register attaches the S3-compatible routes to mux. Route parsing
// follows S3 path-style addressing (/{bucket}/{key...}).
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/", h.dispatch)
}

func (h *Handler) dispatch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodPut:
		if q.Get("uploadId") != "" && q.Get("partNumber") != "" {
			h.UploadPart(w, r)
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
		if key == "" && q.Has("uploads") {
			h.ListMultipartUploads(w, r, bucket)
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
		writeError(w, http.StatusForbidden, "AccessDenied", err.Error(), r.URL.Path)
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
		writeError(w, http.StatusForbidden, "AccessDenied", err.Error(), r.URL.Path)
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
		writeError(w, http.StatusForbidden, "AccessDenied", err.Error(), r.URL.Path)
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

	if policy.ErasureProfile != "" {
		h.putErasureCoded(w, r, tenantID, bucket, key, backendName, provider, policy)
		return
	}

	encMode := policy.EncryptionMode

	// Dedup path: when policy enables intra-tenant dedup AND the
	// gateway has a content_index store wired, route through the
	// pattern-specific lookup/register flow before touching the
	// backend. EC-coded objects are excluded above so the dedup
	// flow always runs against a single piece.
	if h.dedupEnabled(policy) && (encMode == "" || IsGatewayEncrypted(encMode) || encMode == "client_side") {
		h.putDeduped(w, r, tenantID, bucket, key, backendName, provider, policy)
		return
	}

	encCfg, body, contentLength, plaintextSize, ok := h.prepareSinglePieceEncryption(w, r, encMode)
	if !ok {
		return
	}

	pieceID := newPieceID(tenantID, bucket, key, h.cfg.Now())
	putRes, err := provider.PutPiece(r.Context(), pieceID, body, providers.PutOptions{
		ContentLength: contentLength,
		ContentType:   r.Header.Get("Content-Type"),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "BackendPutFailed", err.Error(), r.URL.Path)
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
			PieceID: putRes.PieceID,
			Backend: backendName,
			Locator: putRes.Locator,
			Hash:    putRes.ETag,
			State:   "active",
		}},
		MigrationState: metadata.MigrationState{
			Generation:     1,
			PrimaryBackend: backendName,
		},
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

	w.Header().Set("ETag", quote(putRes.ETag))
	w.Header().Set("x-amz-version-id", manifest.VersionID)
	w.WriteHeader(http.StatusOK)
}

// Get handles S3 GET object.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	manifest, pieceProvider, piece, tenantID, bucket, err := h.resolve(r)
	if err != nil {
		writeResolveError(w, r, err)
		return
	}

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

	// For gateway-encrypted objects (managed / public_distribution)
	// the backend pieces are ciphertext with self-framing nonces, so
	// we must fetch the whole piece to decrypt and only then slice
	// to the requested byte range. Client_side objects are opaque
	// ciphertext from the gateway's view — byte ranges land on
	// ciphertext bytes and the client owns the framing.
	effectiveRange := byteRange
	if IsGatewayEncrypted(manifest.Encryption.Mode) {
		effectiveRange = nil
	}

	body, served, err := h.fetchPiece(r, mkey, manifest, piece, pieceProvider, effectiveRange, tenantID, bucket)
	if err != nil {
		writeError(w, http.StatusBadGateway, "BackendGetFailed", err.Error(), r.URL.Path)
		return
	}
	defer body.Close()
	_ = served

	if IsGatewayEncrypted(manifest.Encryption.Mode) {
		ciphertext, rerr := io.ReadAll(body)
		if rerr != nil {
			writeError(w, http.StatusBadGateway, "BackendGetFailed", rerr.Error(), r.URL.Path)
			return
		}
		plaintext, derr := h.decryptFromStorage(ciphertext, manifest.Encryption)
		if derr != nil {
			writeError(w, http.StatusInternalServerError, "DEKUnwrapFailed", derr.Error(), r.URL.Path)
			return
		}
		if byteRange != nil {
			end := byteRange.End
			if end < 0 || end >= int64(len(plaintext)) {
				end = int64(len(plaintext)) - 1
			}
			if byteRange.Start < 0 || byteRange.Start > end+1 {
				writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "range out of bounds", r.URL.Path)
				return
			}
			plaintext = plaintext[byteRange.Start : end+1]
		}
		body = io.NopCloser(bytes.NewReader(plaintext))
	}

	if piece.Hash != "" {
		w.Header().Set("ETag", quote(piece.Hash))
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
}

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
	if h.cfg.Cache != nil && byteRange == nil {
		cached, _, err := h.cfg.Cache.Get(r.Context(), piece.PieceID)
		if err == nil {
			h.emit(tenantID, bucket, billing.CacheHits, 1)
			return cached, true, nil
		}
	}
	body, err := pieceProvider.GetPiece(r.Context(), piece.PieceID, byteRange)
	if err != nil {
		repaired, repairErr := h.tryReadRepair(r, mkey, manifest, byteRange)
		if repairErr != nil || repaired == nil {
			return nil, false, err
		}
		body = repaired
	}
	if h.cfg.Cache != nil && byteRange == nil {
		h.emit(tenantID, bucket, billing.CacheMisses, 1)
		buf, rerr := io.ReadAll(body)
		_ = body.Close()
		if rerr != nil {
			return nil, false, rerr
		}
		// Warm the cache inline so a concurrent request doesn't
		// re-trigger the backend GET (or a redundant read-repair
		// round-trip during migration). The promotion worker
		// handles signals for pieces that were not cached here
		// (e.g. range reads) so we do not publish one from this
		// path — doing so would cause a redundant origin fetch
		// since the piece is already resident.
		_ = h.cfg.Cache.Put(r.Context(), piece.PieceID, bytes.NewReader(buf), hot_object_cache.PutOptions{
			SizeBytes: int64(len(buf)),
			Hash:      piece.Hash,
		})
		return io.NopCloser(bytes.NewReader(buf)), false, nil
	}
	if byteRange != nil {
		// Range reads skip the inline cache warm because the cache
		// is keyed by piece, not by byte range. Publish a signal so
		// the promotion worker can decide whether to fetch the
		// whole piece asynchronously. Open-ended ranges (End == -1)
		// resolve against the object size so the published
		// ReadBytes is never negative.
		end := byteRange.End
		if end < 0 {
			end = objectSize - 1
		}
		h.signalPromotion(piece, tenantID, end-byteRange.Start+1, objectSize)
	}
	return body, false, nil
}

// tryReadRepair invokes the configured ReadRepair when the primary
// backend fails to serve a piece and the manifest sits in a
// migration-in-progress state (Generation > 1). It returns the
// repaired piece body (wrapped in an io.ReadCloser, sliced to
// byteRange when one was requested) or (nil, nil) when repair is
// not applicable. A non-nil error indicates the repair attempt
// itself failed; callers should fall through to the original
// backend error in that case.
func (h *Handler) tryReadRepair(
	r *http.Request,
	mkey manifest_store.ManifestKey,
	manifest *metadata.ObjectManifest,
	byteRange *providers.ByteRange,
) (io.ReadCloser, error) {
	if h.cfg.ReadRepair == nil {
		return nil, nil
	}
	if manifest.MigrationState.Generation <= 1 {
		return nil, nil
	}
	if len(manifest.Pieces) == 0 {
		return nil, nil
	}
	res, err := h.cfg.ReadRepair.Repair(r.Context(), mkey, manifest, 0)
	if err != nil {
		return nil, err
	}
	data := res.Body
	if byteRange != nil {
		end := byteRange.End
		if end < 0 || end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		if byteRange.Start < 0 || byteRange.Start > end+1 {
			return nil, fmt.Errorf("s3compat: repaired body slice out of range")
		}
		data = data[byteRange.Start : end+1]
	}
	return io.NopCloser(bytes.NewReader(data)), nil
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

// Head handles S3 HEAD object.
func (h *Handler) Head(w http.ResponseWriter, r *http.Request) {
	manifest, _, piece, tenantID, bucket, err := h.resolve(r)
	if err != nil {
		writeResolveError(w, r, err)
		return
	}
	if piece.Hash != "" {
		w.Header().Set("ETag", quote(piece.Hash))
	}
	w.Header().Set("x-amz-version-id", manifest.VersionID)
	w.Header().Set("Content-Length", strconv.FormatInt(manifest.ObjectSize, 10))
	w.WriteHeader(http.StatusOK)

	h.emit(tenantID, bucket, billing.GetRequests, 1)
}

// Delete handles S3 DELETE object.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeError(w, http.StatusForbidden, "AccessDenied", err.Error(), r.URL.Path)
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

	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: hashObjectKey(key),
	}
	// Latest version: leave VersionID empty; concrete stores resolve
	// the current version by (tenant, bucket, object_key_hash). The
	// Postgres and in-memory implementations both honour that.
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
	if manifest.ContentHash != "" && h.cfg.ContentIndex != nil {
		newCount, derr := h.cfg.ContentIndex.DecrementRef(r.Context(), tenantID, manifest.ContentHash)
		switch {
		case errors.Is(derr, content_index.ErrNotFound):
			// Index row is gone but the manifest still
			// pointed at it — fall through to a
			// best-effort piece delete to clean up.
			h.deletePiecesBestEffort(r, manifest)
		case derr != nil:
			writeError(w, http.StatusInternalServerError, "ContentIndexDecrementFailed", derr.Error(), r.URL.Path)
			return
		case newCount == 0:
			// Last reference: remove the piece from the
			// backend, then drop the index row.
			h.deletePiecesBestEffort(r, manifest)
			if err := h.cfg.ContentIndex.Delete(r.Context(), tenantID, manifest.ContentHash); err != nil && !errors.Is(err, content_index.ErrNotFound) {
				writeError(w, http.StatusInternalServerError, "ContentIndexDeleteFailed", err.Error(), r.URL.Path)
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
	}

	h.emit(tenantID, bucket, billing.DeleteRequests, 1)
	w.WriteHeader(http.StatusNoContent)
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
		writeError(w, http.StatusForbidden, "AccessDenied", err.Error(), r.URL.Path)
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
		c := content{Key: m.ObjectKey, Size: m.ObjectSize}
		if len(m.Pieces) > 0 {
			c.ETag = quote(m.Pieces[0].Hash)
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
		return nil, nil, metadata.Piece{}, "", "", &httpError{code: http.StatusForbidden, s3code: "AccessDenied", msg: err.Error()}
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
	}
	manifest, err := h.cfg.Manifests.Get(r.Context(), mkey)
	if err != nil {
		if errors.Is(err, manifest_store.ErrNotFound) {
			return nil, nil, metadata.Piece{}, "", "", &httpError{code: http.StatusNotFound, s3code: "NoSuchKey", msg: "no such key"}
		}
		return nil, nil, metadata.Piece{}, "", "", &httpError{code: http.StatusInternalServerError, s3code: "ManifestGetFailed", msg: err.Error()}
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

func (h *Handler) authenticate(r *http.Request) (string, error) {
	if h.cfg.Auth == nil {
		return AnonymousTenant, nil
	}
	return h.cfg.Auth.Authenticate(r)
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

// newPieceID mints a deterministic-looking but unique piece ID for a
// new object. Phase 2's client SDK will later hand the gateway a
// BLAKE3 hash of the ciphertext; this helper unblocks the gateway
// until that lands.
func newPieceID(tenantID, bucket, key string, now time.Time) string {
	sum := sha256.Sum256([]byte(tenantID + "\x00" + bucket + "\x00" + key + "\x00" + strconv.FormatInt(now.UnixNano(), 10)))
	return hex.EncodeToString(sum[:])
}

// parseHTTPRange parses a single-range HTTP Range header
// ("bytes=start-end" or "bytes=start-") into a providers.ByteRange.
// Suffix ranges ("bytes=-N") and multi-range requests are not yet
// supported.
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
		return nil, fmt.Errorf("suffix ranges are not supported")
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
// handler method can choose the right HTTP status code.
type httpError struct {
	code   int
	s3code string
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func writeResolveError(w http.ResponseWriter, r *http.Request, err error) {
	var he *httpError
	if errors.As(err, &he) {
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
