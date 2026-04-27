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
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/kennguy3n/zk-object-fabric/billing"
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
type ListVersionsResult struct {
	XMLName     xml.Name           `xml:"ListVersionsResult"`
	Name        string             `xml:"Name"`
	Prefix      string             `xml:"Prefix"`
	MaxKeys     int                `xml:"MaxKeys"`
	IsTruncated bool               `xml:"IsTruncated"`
	Versions    []ListVersionEntry `xml:"Version"`
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
		writeError(w, http.StatusForbidden, "AccessDenied", err.Error(), r.URL.Path)
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

	// Dedup-aware fast path: source has a ContentHash and the
	// gateway has the content index wired. Bump the refcount,
	// reuse the existing piece, write a new manifest pointing at
	// it. No backend data motion.
	if srcManifest.ContentHash != "" && h.cfg.ContentIndex != nil {
		if err := h.cfg.ContentIndex.IncrementRef(r.Context(), tenantID, srcManifest.ContentHash); err == nil {
			h.writeCopyManifest(w, r, tenantID, dstBucket, dstKey, srcManifest, srcPiece, srcPiece.Backend, true)
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
		} else {
			// Provider claims server-side copy capability
			// but does not implement PieceCopier — fall
			// back to GET+PUT.
			newSize, newETag, err = h.copyViaGetPut(r.Context(), srcProvider, srcPiece.PieceID, dstPieceID, r.Header.Get("Content-Type"))
			if err != nil {
				writeError(w, http.StatusBadGateway, "BackendCopyFailed", err.Error(), r.URL.Path)
				return
			}
		}
	} else {
		newSize, newETag, err = h.copyViaGetPut(r.Context(), srcProvider, srcPiece.PieceID, dstPieceID, r.Header.Get("Content-Type"))
		if err != nil {
			writeError(w, http.StatusBadGateway, "BackendCopyFailed", err.Error(), r.URL.Path)
			return
		}
	}

	// Build a fresh piece pointer for the destination manifest.
	newPiece := metadata.Piece{
		PieceID:   dstPieceID,
		Hash:      newETag,
		Backend:   srcPiece.Backend,
		Locator:   srcPiece.Locator, // approximated; provider rewrites on first GET
		State:     "active",
		SizeBytes: newSize,
	}
	h.writeCopyManifest(w, r, tenantID, dstBucket, dstKey, srcManifest, newPiece, srcPiece.Backend, false)
}

// copyViaGetPut streams the source piece through GetPiece and
// re-uploads it via PutPiece.
func (h *Handler) copyViaGetPut(
	ctx context.Context,
	srcProvider providers.StorageProvider,
	srcPieceID, dstPieceID, contentType string,
) (int64, string, error) {
	body, err := srcProvider.GetPiece(ctx, srcPieceID, nil)
	if err != nil {
		return 0, "", fmt.Errorf("get source piece: %w", err)
	}
	defer body.Close()
	res, err := srcProvider.PutPiece(ctx, dstPieceID, body, providers.PutOptions{
		ContentLength: -1,
		ContentType:   contentType,
	})
	if err != nil {
		return 0, "", fmt.Errorf("put dest piece: %w", err)
	}
	return res.SizeBytes, res.ETag, nil
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
	dedupCopy bool,
) {
	dstHash := hashObjectKey(dstKey)
	dstVersion := piece.PieceID
	objectSize := srcManifest.ObjectSize
	if !dedupCopy && piece.SizeBytes > 0 {
		// Non-dedup copy preserves the source's plaintext size
		// (gateway encryption is opaque to the copy path).
		objectSize = srcManifest.ObjectSize
	}
	manifest := &metadata.ObjectManifest{
		TenantID:        tenantID,
		Bucket:          dstBucket,
		ObjectKey:       dstKey,
		ObjectKeyHash:   dstHash,
		VersionID:       dstVersion,
		ObjectSize:      objectSize,
		ChunkSize:       srcManifest.ChunkSize,
		ContentHash:     srcManifest.ContentHash,
		Encryption:      srcManifest.Encryption,
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

	res := CopyObjectResult{
		ETag:         quote(piece.Hash),
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
		writeError(w, http.StatusForbidden, "AccessDenied", err.Error(), r.URL.Path)
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
			etag := ""
			if len(v.Pieces) > 0 {
				etag = quote(v.Pieces[0].Hash)
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


