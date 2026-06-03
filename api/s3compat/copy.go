// CopyObject and ListObjectVersions handlers.
//
// CopyObject is a PUT with x-amz-copy-source set. The handler:
//   1. Resolves the source manifest.
//   2. If the source carries a ContentHash and the gateway has a
//      content_index store wired, increments the refcount on the
//      existing piece and reuses the original PieceID — the
//      backend never sees a copy operation.
//   3. Otherwise, when the provider supports server-side copy
//      (Capabilities().SupportsServerSideCopy), uses the
//      provider's native copy path via CopyPiece.
//   4. Otherwise, falls back to GetPiece + PutPiece.
//
// In all three cases a new manifest is written for the destination
// (tenant, bucket, key) and a CopyObjectResult XML body is
// returned per the S3 API spec.
package s3compat

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/zeebo/blake3"

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// CopyObjectResult is the S3 CopyObject response body.
type CopyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

// ListVersionsResult is the S3 ListObjectVersions response body.
// Delete markers are reported in their own <DeleteMarker> elements,
// distinct from the <Version> rows for real object versions, matching
// the AWS S3 wire format.
type ListVersionsResult struct {
	XMLName       xml.Name                `xml:"ListVersionsResult"`
	Name          string                  `xml:"Name"`
	Prefix        string                  `xml:"Prefix"`
	MaxKeys       int                     `xml:"MaxKeys"`
	IsTruncated   bool                    `xml:"IsTruncated"`
	Versions      []ListVersionEntry      `xml:"Version"`
	DeleteMarkers []ListDeleteMarkerEntry `xml:"DeleteMarker"`
}

// ListVersionEntry is a single row in ListObjectVersions output.
type ListVersionEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag,omitempty"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

// ListDeleteMarkerEntry is a <DeleteMarker> row in ListObjectVersions
// output. Delete markers carry no size, ETag, or storage class.
type ListDeleteMarkerEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
}

// Copy handles S3 CopyObject (PUT with x-amz-copy-source).
//
// Dedup-aware copy: if the source manifest carries a
// ContentHash AND the gateway has a content_index store, the
// destination manifest reuses the source's piece via
// content_index.IncrementRef — no backend data movement.
//
// Otherwise, the copy is routed through the source provider's
// native CopyPiece (when Capabilities().SupportsServerSideCopy
// is true) or, as a fallback, GetPiece+PutPiece. Cross-provider
// copies always use the GetPiece+PutPiece fallback because
// there is no portable cross-provider copy primitive.
func (h *Handler) Copy(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	dstBucket, dstKey := parseBucketKey(r.URL.Path)
	if dstBucket == "" || dstKey == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "destination path must be /{bucket}/{key...}", r.URL.Path)
		return
	}
	if h.cfg.Manifests == nil {
		writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable", "manifest store not configured", r.URL.Path)
		return
	}
	srcBucket, srcKey, srcVersion, perr := parseCopySource(r.Header.Get("x-amz-copy-source"))
	if perr != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", perr.Error(), r.URL.Path)
		return
	}

	srcMKey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        srcBucket,
		ObjectKeyHash: hashObjectKey(srcKey),
		VersionID:     srcVersion,
	}
	srcManifest, err := h.cfg.Manifests.Get(r.Context(), srcMKey)
	if err != nil {
		if errors.Is(err, manifest_store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NoSuchKey", "copy source not found", r.URL.Path)
			return
		}
		writeError(w, http.StatusInternalServerError, "ManifestGetFailed", err.Error(), r.URL.Path)
		return
	}
	// A delete marker carries no payload, so it can never be a copy
	// source. Mirror GET/HEAD semantics (handler.go resolve): an
	// explicit ?versionId pointing at a marker is a 405, while a
	// marker resolved as the latest version reads as a missing key
	// (404). Both checks MUST precede the empty-Pieces guard below,
	// since a marker also has zero pieces and would otherwise surface
	// as a misleading 500 EmptyManifest.
	if srcManifest.DeleteMarker {
		if srcVersion != "" {
			writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed",
				"copy source version is a delete marker", r.URL.Path)
			return
		}
		writeError(w, http.StatusNotFound, "NoSuchKey", "copy source not found", r.URL.Path)
		return
	}
	if len(srcManifest.Pieces) == 0 {
		writeError(w, http.StatusInternalServerError, "EmptyManifest", "source manifest has no pieces", r.URL.Path)
		return
	}

	// Erasure-coded and multipart manifests are not supported by
	// CopyObject in this gateway. The source manifest's pieces
	// list cannot be safely refcounted as a single piece.
	if isErasureCodedManifest(srcManifest) || isMultipartManifest(srcManifest) {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "CopyObject does not support multipart or erasure-coded sources", r.URL.Path)
		return
	}

	srcPiece := srcManifest.Pieces[0]
	srcProvider, ok := h.cfg.Providers[srcPiece.Backend]
	if !ok {
		writeError(w, http.StatusInternalServerError, "BackendNotRegistered",
			"source backend "+srcPiece.Backend+" not registered", r.URL.Path)
		return
	}

	// AAD v1 re-encryption copy. A v1 object's ciphertext is
	// sealed with per-chunk AAD bound to the SOURCE identity
	// (tenant|bucket|object_key_hash|version_id). The destination
	// always has a different identity (a new version at minimum),
	// so reusing the ciphertext verbatim — refcount reuse, provider
	// CopyPiece, or GET+PUT — would make the destination GET rebuild
	// a different AAD and every chunk's Poly1305 tag would fail.
	// Decrypt under the source identity and re-encrypt under the
	// destination identity with a fresh DEK. Legacy (AADVersion "")
	// and convergent sources are not identity-bound and fall through
	// to the cheap verbatim paths below. (v1 objects carry no
	// ContentHash, so the dedup fast path would skip them anyway;
	// this guard makes the requirement explicit and also covers the
	// provider server-side-copy path.)
	if srcManifest.Encryption.AADVersion == AADVersionV1 {
		h.copyReencrypt(w, r, tenantID, dstBucket, dstKey, srcManifest, srcPiece, srcProvider)
		return
	}

	// Dedup-aware fast path: source has a ContentHash and the
	// gateway has the content index wired. Bump the refcount,
	// reuse the existing piece, write a new manifest pointing at
	// it. No backend data motion.
	if srcManifest.ContentHash != "" && h.cfg.ContentIndex != nil {
		if err := h.cfg.ContentIndex.IncrementRef(r.Context(), tenantID, srcManifest.ContentHash); err == nil {
			h.writeCopyManifest(w, r, tenantID, dstBucket, dstKey, srcManifest, srcPiece, srcPiece.Backend, srcManifest.Encryption, true)
			return
		} else if !errors.Is(err, content_index.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, "ContentIndexIncrementFailed", err.Error(), r.URL.Path)
			return
		}
		// Fall through: index row vanished. Treat as a fresh copy.
	}

	// Non-dedup copy. Mint a new piece ID; route the bytes
	// through the source provider's server-side copy if it
	// supports it, otherwise GetPiece + PutPiece.
	dstPieceID := newPieceID(tenantID, dstBucket, dstKey, h.cfg.Now())
	caps := srcProvider.Capabilities()
	var (
		newSize int64
		newHash string
		newETag string
	)
	if caps.SupportsServerSideCopy {
		if cp, ok := srcProvider.(PieceCopier); ok {
			res, cerr := cp.CopyPiece(r.Context(), srcPiece.PieceID, dstPieceID)
			if cerr != nil {
				writeError(w, http.StatusBadGateway, "BackendCopyFailed", cerr.Error(), r.URL.Path)
				return
			}
			newSize = res.SizeBytes
			newETag = res.ETag
			// Server-side copy means the backend duplicated
			// the bytes itself; the bytes are identical to the
			// source so the BLAKE3 hash carries over. We trust
			// the source manifest here because we already
			// verified the source piece (or refused to serve
			// it) on every GET path; a separate scrubber will
			// catch bit-rot in the destination later.
			newHash = srcPiece.Hash
		} else {
			// Provider claims server-side copy capability
			// but does not implement PieceCopier — fall
			// back to GET+PUT.
			newSize, newHash, newETag, err = h.copyViaGetPut(r.Context(), srcProvider, srcPiece.PieceID, dstPieceID, r.Header.Get("Content-Type"))
			if err != nil {
				writeError(w, http.StatusBadGateway, "BackendCopyFailed", err.Error(), r.URL.Path)
				return
			}
		}
	} else {
		newSize, newHash, newETag, err = h.copyViaGetPut(r.Context(), srcProvider, srcPiece.PieceID, dstPieceID, r.Header.Get("Content-Type"))
		if err != nil {
			writeError(w, http.StatusBadGateway, "BackendCopyFailed", err.Error(), r.URL.Path)
			return
		}
	}

	// Build a fresh piece pointer for the destination manifest.
	// Hash is the BLAKE3 content hash (used by the GET integrity
	// check); ProviderETag is the backend's opaque ETag. Before
	// this commit the two were conflated and the destination
	// manifest carried the backend's ETag in the Hash slot, which
	// broke every cache-miss GET on the copy once integrity
	// verification landed.
	newPiece := metadata.Piece{
		PieceID:      dstPieceID,
		Hash:         newHash,
		ProviderETag: newETag,
		Backend:      srcPiece.Backend,
		Locator:      srcPiece.Locator, // approximated; provider rewrites on first GET
		State:        "active",
		SizeBytes:    newSize,
	}
	h.writeCopyManifest(w, r, tenantID, dstBucket, dstKey, srcManifest, newPiece, srcPiece.Backend, srcManifest.Encryption, false)
}

// copyReencrypt performs a CopyObject for an AAD v1 source by
// decrypting the source piece under the source identity and
// re-encrypting under the destination identity with a fresh DEK.
// This is mandatory for v1 objects (see the call site in Copy):
// their per-chunk AAD binds the ciphertext to the object identity,
// so a verbatim copy would be undecryptable at the destination.
//
// Only the single-piece, gateway-encrypted source shape reaches
// here — multipart / EC sources are already rejected upstream, and
// only v1 (not legacy / convergent) sources are routed in. The
// destination manifest records a v1 EncryptionConfig bound to the
// fresh DEK and destination identity.
func (h *Handler) copyReencrypt(
	w http.ResponseWriter,
	r *http.Request,
	tenantID, dstBucket, dstKey string,
	srcManifest *metadata.ObjectManifest,
	srcPiece metadata.Piece,
	srcProvider providers.StorageProvider,
) {
	if h.cfg.Encryption == nil {
		writeError(w, http.StatusInternalServerError, "EncryptionNotConfigured",
			"source object is gateway-encrypted but no gateway encryption is configured", r.URL.Path)
		return
	}

	// Re-encrypting a v1 source needs the whole object resident:
	// the ciphertext is read in full, decrypted to a full plaintext
	// buffer, and re-encrypted to a full ciphertext buffer (~3x the
	// object size live at once) because encryptForStorage consumes a
	// []byte. Cap this the same way every other buffered path does
	// (single-piece buffered GET, EC PUT, range GET) so a large v1
	// object cannot OOM the gateway during a copy; the verbatim
	// (legacy/convergent) copy path streams via copyViaGetPut and is
	// unaffected. Streaming v1 re-encryption is the same Phase-4
	// workstream that gates streaming EC.
	if srcManifest.ObjectSize > MaxInMemoryObjectBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "CopyReencryptObjectTooLarge",
			fmt.Sprintf("server-side copy of AAD v1 object of %d bytes exceeds in-memory re-encrypt ceiling of %d bytes; streaming re-encryption is not yet implemented",
				srcManifest.ObjectSize, MaxInMemoryObjectBytes),
			r.URL.Path)
		return
	}

	body, err := srcProvider.GetPiece(r.Context(), srcPiece.PieceID, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "BackendGetFailed", err.Error(), r.URL.Path)
		return
	}
	// Defence in depth against a backend (or a manifest with a stale
	// ObjectSize) returning more than the ceiling check admitted:
	// bound the read to one byte over the ceiling and reject if the
	// body actually exceeds it, exactly like the EC PUT path.
	ciphertext, rerr := io.ReadAll(io.LimitReader(body, MaxInMemoryObjectBytes+1))
	_ = body.Close()
	if rerr != nil {
		writeError(w, http.StatusBadGateway, "BackendGetFailed", rerr.Error(), r.URL.Path)
		return
	}
	if int64(len(ciphertext)) > MaxInMemoryObjectBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "CopyReencryptObjectTooLarge",
			fmt.Sprintf("server-side copy source ciphertext exceeds in-memory re-encrypt ceiling of %d bytes", MaxInMemoryObjectBytes),
			r.URL.Path)
		return
	}

	plaintext, derr := h.decryptFromStorage(ciphertext, srcManifest.Encryption, aadIdentityOf(srcManifest))
	if derr != nil {
		writeError(w, http.StatusInternalServerError, "DEKUnwrapFailed", derr.Error(), r.URL.Path)
		return
	}

	dstPieceID := newPieceID(tenantID, dstBucket, dstKey, h.cfg.Now())
	dstID := aadIdentity{
		TenantID:      tenantID,
		Bucket:        dstBucket,
		ObjectKeyHash: hashObjectKey(dstKey),
		VersionID:     dstPieceID,
	}
	newCiphertext, wrapped, eerr := h.encryptForStorage(plaintext, dstID)
	// Scrub the recovered plaintext now that the SDK has consumed
	// it; defence-in-depth against heap-dump exposure of cleartext
	// copied through the gateway.
	clear(plaintext)
	if eerr != nil {
		writeError(w, http.StatusInternalServerError, "EncryptionFailed", eerr.Error(), r.URL.Path)
		return
	}

	cipherHash := blake3.Sum256(newCiphertext)
	putRes, perr := srcProvider.PutPiece(r.Context(), dstPieceID, bytes.NewReader(newCiphertext), providers.PutOptions{
		ContentLength: int64(len(newCiphertext)),
		ContentType:   r.Header.Get("Content-Type"),
	})
	if perr != nil {
		writeError(w, http.StatusBadGateway, "BackendPutFailed", perr.Error(), r.URL.Path)
		return
	}

	newPiece := metadata.Piece{
		// Record the deterministic dstPieceID we minted, not
		// putRes.PieceID: writeCopyManifest derives the
		// manifest VersionID from Piece.PieceID, and the AAD
		// above was bound to dstPieceID. Binding to dstPieceID
		// (rather than whatever the backend echoes) keeps the
		// recorded VersionID identical to the bound identity on
		// every provider, mirroring the single-piece PUT path
		// (handler.go: VersionID = requested pieceID) and the
		// non-dedup copy path (which also records dstPieceID).
		PieceID:      dstPieceID,
		Hash:         "blake3:" + hex.EncodeToString(cipherHash[:]),
		ProviderETag: putRes.ETag,
		Backend:      srcPiece.Backend,
		Locator:      putRes.Locator,
		State:        "active",
		SizeBytes:    putRes.SizeBytes,
	}
	// Fresh v1 EncryptionConfig: new wrapped DEK, bound to the
	// destination identity. Preserve the source's Mode and
	// ManifestEncrypted; the DEK/algorithm fields all come from the
	// re-encrypt above.
	dstEnc := metadata.EncryptionConfig{
		Mode:              srcManifest.Encryption.Mode,
		Algorithm:         client_sdk.ContentAlgorithm,
		KeyID:             wrapped.KeyID,
		WrappedDEK:        wrapped.WrappedKey,
		WrapAlgorithm:     wrapped.WrapAlgorithm,
		ManifestEncrypted: srcManifest.Encryption.ManifestEncrypted,
		AADVersion:        AADVersionV1,
	}
	h.writeCopyManifest(w, r, tenantID, dstBucket, dstKey, srcManifest, newPiece, srcPiece.Backend, dstEnc, false)
}

// copyViaGetPut streams the source piece through GetPiece and
// re-uploads it via PutPiece. It TeeReaders the bytes through a
// BLAKE3 hasher so the destination piece's content hash can be
// returned alongside the backend's ETag — the caller then stamps
// the BLAKE3 hash into Piece.Hash and the ETag into
// Piece.ProviderETag. The two are not interchangeable: Hash is
// what the GET integrity check (re-)computes from the bytes,
// ETag is the backend's opaque identifier for the upload.
func (h *Handler) copyViaGetPut(
	ctx context.Context,
	srcProvider providers.StorageProvider,
	srcPieceID, dstPieceID, contentType string,
) (size int64, blake3Hash, providerETag string, err error) {
	body, err := srcProvider.GetPiece(ctx, srcPieceID, nil)
	if err != nil {
		return 0, "", "", fmt.Errorf("get source piece: %w", err)
	}
	defer body.Close()

	hasher := blake3.New()
	tee := io.TeeReader(body, hasher)
	res, err := srcProvider.PutPiece(ctx, dstPieceID, tee, providers.PutOptions{
		ContentLength: -1,
		ContentType:   contentType,
	})
	if err != nil {
		return 0, "", "", fmt.Errorf("put dest piece: %w", err)
	}
	return res.SizeBytes, "blake3:" + hex.EncodeToString(hasher.Sum(nil)), res.ETag, nil
}

// writeCopyManifest assembles a destination manifest, persists it,
// and emits the CopyObjectResult.
//
// dedupCopy=true means the destination piece is the exact same
// PieceID as the source (refcount-only copy); the manifest carries
// the source's ContentHash so a subsequent DELETE will use the
// refcount-aware DELETE path.
func (h *Handler) writeCopyManifest(
	w http.ResponseWriter,
	r *http.Request,
	tenantID, dstBucket, dstKey string,
	srcManifest *metadata.ObjectManifest,
	piece metadata.Piece,
	backend string,
	enc metadata.EncryptionConfig,
	dedupCopy bool,
) {
	dstHash := hashObjectKey(dstKey)
	dstVersion := piece.PieceID
	// Both copy paths preserve the source's plaintext object
	// size; gateway encryption is opaque to the copy path.
	objectSize := srcManifest.ObjectSize
	// Only propagate ContentHash on the dedup-aware fast path.
	// On the non-dedup fall-through we minted a fresh piece that
	// is NOT tracked in content_index; carrying the source's
	// hash would cause a subsequent DELETE on the destination to
	// decrement the source's refcount via the refcount-aware
	// path and corrupt accounting.
	contentHash := ""
	if dedupCopy {
		contentHash = srcManifest.ContentHash
	}
	manifest := &metadata.ObjectManifest{
		TenantID:        tenantID,
		Bucket:          dstBucket,
		ObjectKey:       dstKey,
		ObjectKeyHash:   dstHash,
		VersionID:       dstVersion,
		ObjectSize:      objectSize,
		ChunkSize:       srcManifest.ChunkSize,
		ContentHash:     contentHash,
		Encryption:      enc,
		PlacementPolicy: srcManifest.PlacementPolicy,
		Pieces:          []metadata.Piece{piece},
		MigrationState: metadata.MigrationState{
			Generation:     1,
			PrimaryBackend: backend,
		},
	}
	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        dstBucket,
		ObjectKeyHash: dstHash,
		VersionID:     dstVersion,
	}
	if err := h.cfg.Manifests.Put(r.Context(), mkey, manifest); err != nil {
		// Best-effort rollback for non-dedup copy: drop the
		// freshly-uploaded destination piece. For dedup copy
		// we'd also want to decrement the refcount, but the
		// rollback path is best-effort and the orphan GC
		// handles eventual cleanup.
		if !dedupCopy {
			if provider, ok := h.cfg.Providers[backend]; ok {
				_ = provider.DeletePiece(r.Context(), piece.PieceID)
			}
		} else if h.cfg.ContentIndex != nil && srcManifest.ContentHash != "" {
			_, _ = h.cfg.ContentIndex.DecrementRef(r.Context(), tenantID, srcManifest.ContentHash)
		}
		writeError(w, http.StatusInternalServerError, "ManifestPutFailed", err.Error(), r.URL.Path)
		return
	}

	h.emit(tenantID, dstBucket, billing.PutRequests, 1)
	var copyCountry string
	if prov, ok := h.cfg.Providers[backend]; ok {
		copyCountry = prov.PlacementLabels().Country
	}
	h.audit(r, "COPY", tenantID, dstBucket, dstKey, piece.PieceID, backend, copyCountry)

	res := CopyObjectResult{
		ETag:         quote(pieceETag(piece)),
		LastModified: h.cfg.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-version-id", manifest.VersionID)
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(res)
}

// PieceCopier is an optional interface that providers MAY
// implement when their backend exposes a server-side copy
// primitive (e.g. S3 CopyObject). When absent, the handler falls
// back to GetPiece + PutPiece.
type PieceCopier interface {
	CopyPiece(ctx context.Context, srcPieceID, dstPieceID string) (providers.PutResult, error)
}

// parseCopySource decodes the x-amz-copy-source header value.
// Accepted forms: "bucket/key", "/bucket/key", "bucket/key?versionId=v".
func parseCopySource(raw string) (bucket, key, version string, err error) {
	if raw == "" {
		return "", "", "", errors.New("x-amz-copy-source is empty")
	}
	// versionId may be appended as a query string.
	src := raw
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		src = raw[:i]
		q, qerr := url.ParseQuery(raw[i+1:])
		if qerr != nil {
			return "", "", "", fmt.Errorf("invalid copy-source query: %w", qerr)
		}
		version = q.Get("versionId")
	}
	src = strings.TrimPrefix(src, "/")
	src, err = url.QueryUnescape(src)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid copy-source: %w", err)
	}
	idx := strings.IndexByte(src, '/')
	if idx <= 0 || idx == len(src)-1 {
		return "", "", "", fmt.Errorf("copy-source must be /{bucket}/{key}; got %q", raw)
	}
	return src[:idx], src[idx+1:], version, nil
}

// ListObjectVersions handles bucket-level GET ?versions.
func (h *Handler) ListObjectVersions(w http.ResponseWriter, r *http.Request, bucket string) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	if h.cfg.Manifests == nil {
		writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable", "manifest store not configured", r.URL.Path)
		return
	}
	if bucket == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "path must be /{bucket}", r.URL.Path)
		return
	}

	// Strategy: walk the bucket's latest-version List, then for
	// each unique object call ListVersions to enumerate every
	// version. The gateway's List endpoint already collapses to
	// latest-only per S3 ListObjectsV2 semantics; ListVersions
	// must surface the full history.
	//
	// For deployments that have a small number of objects this
	// is fine; for large buckets the operator should rely on
	// pagination (TODO: implement key-marker / version-id-marker
	// pagination).
	prefix := r.URL.Query().Get("prefix")
	maxKeys := 1000
	listing, err := h.cfg.Manifests.List(r.Context(), tenantID, bucket, "", maxKeys)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ManifestListFailed", err.Error(), r.URL.Path)
		return
	}
	out := ListVersionsResult{Name: bucket, Prefix: prefix, MaxKeys: maxKeys}
	for _, latest := range listing.Manifests {
		if prefix != "" && !strings.HasPrefix(latest.ObjectKey, prefix) {
			continue
		}
		versions, verr := h.cfg.Manifests.ListVersions(r.Context(), tenantID, bucket, latest.ObjectKeyHash)
		if verr != nil {
			writeError(w, http.StatusInternalServerError, "ManifestListVersionsFailed", verr.Error(), r.URL.Path)
			return
		}
		for i, v := range versions {
			// Delete markers are reported in their own element
			// without size/ETag, matching AWS S3. ListVersions
			// returns newest-first, so index 0 is the latest.
			if v.DeleteMarker {
				out.DeleteMarkers = append(out.DeleteMarkers, ListDeleteMarkerEntry{
					Key:          v.ObjectKey,
					VersionID:    v.VersionID,
					IsLatest:     i == 0,
					LastModified: h.cfg.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
				})
				continue
			}
			etag := ""
			if len(v.Pieces) > 0 {
				etag = quote(pieceETag(v.Pieces[0]))
			}
			out.Versions = append(out.Versions, ListVersionEntry{
				Key:          v.ObjectKey,
				VersionID:    v.VersionID,
				IsLatest:     i == 0,
				LastModified: h.cfg.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
				ETag:         etag,
				Size:         v.ObjectSize,
				StorageClass: "STANDARD",
			})
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return
	}
	_ = xml.NewEncoder(w).Encode(out)
}


