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

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
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

	// Billing receives usage events. Optional.
	Billing BillingSink

	// NodeID identifies the gateway node emitting billing events.
	NodeID string

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
	switch r.Method {
	case http.MethodPut:
		h.Put(w, r)
	case http.MethodGet:
		// LIST is a bucket-level GET (no key, or ?list-type=2).
		bucket, key := parseBucketKey(r.URL.Path)
		if key == "" || r.URL.Query().Has("list-type") {
			h.listBucket(w, r, bucket)
			return
		}
		h.Get(w, r)
	case http.MethodHead:
		h.Head(w, r)
	case http.MethodDelete:
		h.Delete(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed", r.URL.Path)
	}
}

// Put handles S3 PUT object. It reads the request body, writes it to
// the storage backend chosen by the placement engine, constructs an
// ObjectManifest, and persists it to the manifest store.
func (h *Handler) Put(w http.ResponseWriter, r *http.Request) {
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

	pieceID := newPieceID(tenantID, bucket, key, h.cfg.Now())
	putRes, err := provider.PutPiece(r.Context(), pieceID, r.Body, providers.PutOptions{
		ContentLength: r.ContentLength,
		ContentType:   r.Header.Get("Content-Type"),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "BackendPutFailed", err.Error(), r.URL.Path)
		return
	}

	manifest := &metadata.ObjectManifest{
		TenantID:        tenantID,
		Bucket:          bucket,
		ObjectKeyHash:   hashObjectKey(key),
		VersionID:       pieceID,
		ObjectSize:      putRes.SizeBytes,
		ChunkSize:       putRes.SizeBytes,
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

	var byteRange *providers.ByteRange
	if hdr := r.Header.Get("Range"); hdr != "" {
		rng, perr := parseHTTPRange(hdr, manifest.ObjectSize)
		if perr != nil {
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", perr.Error(), r.URL.Path)
			return
		}
		byteRange = rng
	}

	body, err := pieceProvider.GetPiece(r.Context(), piece.PieceID, byteRange)
	if err != nil {
		writeError(w, http.StatusBadGateway, "BackendGetFailed", err.Error(), r.URL.Path)
		return
	}
	defer body.Close()

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
		// In Phase 2 every GET that misses the cache is also an
		// origin-egress event. The cache integration will later
		// subtract hits from this counter.
		h.emit(tenantID, bucket, billing.OriginEgressBytes, uint64(n))
	}
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
	for _, piece := range manifest.Pieces {
		provider, ok := h.cfg.Providers[piece.Backend]
		if !ok {
			continue
		}
		// Best-effort: the manifest is already gone so the object is
		// user-invisible. Any surviving pieces are orphans for GC.
		_ = provider.DeletePiece(r.Context(), piece.PieceID)
	}

	h.emit(tenantID, bucket, billing.DeleteRequests, 1)
	w.WriteHeader(http.StatusNoContent)
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
		c := content{Key: m.ObjectKeyHash, Size: m.ObjectSize}
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
