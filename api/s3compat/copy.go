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
	"time"

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

// evaluateCopySourceConditionals applies the CopyObject source
// preconditions against the resolved source object: the
// x-amz-copy-source-if-match / -if-none-match / -if-modified-since /
// -if-unmodified-since headers. It reports true when a precondition is
// NOT satisfied, in which case the caller must abort the copy with
// 412 PreconditionFailed. Unlike a conditional read there is no 304 —
// a copy is a write that either proceeds or fails — so an unsatisfied
// negative-group condition also yields 412.
//
// Precedence matches AWS S3 (CopyObject API reference) and the read
// path's evaluateConditionalRead: within the positive group
// copy-source-if-match wins over -if-unmodified-since; within the
// negative group copy-source-if-none-match wins over -if-modified-since;
// when an entity-tag header is present its sibling date header is
// ignored. copy-source-if-match uses RFC 7232 §3.1 strong comparison;
// copy-source-if-none-match uses §3.2 weak comparison. etag is the
// source object's advertised ETag in quoted wire form ("" when it
// exposes none, e.g. erasure-coded / multipart sources — which now
// reconstruct rather than 501, so an entity-tag conditional against
// them genuinely fails the precondition with 412 here); lastModified
// is the source CreatedAt, truncated to whole seconds to agree with
// the second resolution of the HTTP-date validators.
func evaluateCopySourceConditionals(hdr http.Header, etag string, lastModified time.Time) bool {
	modifiedAfter := func(raw string) (modified bool, ok bool) {
		t, err := http.ParseTime(raw)
		if err != nil {
			// An unparseable date makes the conditional inoperative
			// (RFC 7232 §3.3/§3.4): ignore the header.
			return false, false
		}
		return lastModified.Truncate(time.Second).After(t), true
	}

	// Positive group: if-match wins over if-unmodified-since.
	if im := hdr.Get("x-amz-copy-source-if-match"); im != "" {
		if strings.TrimSpace(im) != "*" && !etagListMatch(im, etag, true) {
			return true
		}
	} else if ius := hdr.Get("x-amz-copy-source-if-unmodified-since"); ius != "" {
		if modified, ok := modifiedAfter(ius); ok && modified {
			return true
		}
	}

	// Negative group: if-none-match wins over if-modified-since. A
	// satisfied negative condition fails the copy (412), not a 304.
	if inm := hdr.Get("x-amz-copy-source-if-none-match"); inm != "" {
		if strings.TrimSpace(inm) == "*" || etagListMatch(inm, etag, false) {
			return true
		}
	} else if ims := hdr.Get("x-amz-copy-source-if-modified-since"); ims != "" {
		if modified, ok := modifiedAfter(ims); ok && !modified {
			return true
		}
	}

	return false
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
	// Object Lock overwrite enforcement: a copy that would
	// replace a locked destination version in place is refused, just
	// like a regular PUT overwrite.
	if !h.allowObjectLockOverwrite(w, r, tenantID, dstBucket, dstKey) {
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

	srcPiece := srcManifest.Pieces[0]

	// CopyObject source preconditions (x-amz-copy-source-if-*). Evaluated
	// against the resolved source object's ETag / CreatedAt; an
	// unsatisfied precondition aborts the copy with 412 PreconditionFailed
	// (a write has no 304 fallback). For erasure-coded / multipart sources
	// condETagForRead returns "" so only the "*"/date validators apply.
	if evaluateCopySourceConditionals(r.Header, condETagForRead(srcManifest, srcPiece), srcManifest.CreatedAt) {
		writeError(w, http.StatusPreconditionFailed, "PreconditionFailed",
			"the copy source conditional request precondition failed", r.URL.Path)
		return
	}

	// Destination tagging (x-amz-tagging-directive): validate up front so a
	// bad directive or REPLACE x-amz-tagging header fails with 400 before
	// any bytes move. writeCopyManifest re-resolves the validated value.
	if _, verr := resolveCopyTags(r.Header, srcManifest.Tags); verr != nil {
		writeError(w, verr.code, verr.s3code, verr.msg, r.URL.Path)
		return
	}

	// Destination object metadata (x-amz-metadata-directive): validate up
	// front so a bad directive or oversized REPLACE x-amz-meta-* set fails
	// with 400 before any bytes move. writeCopyManifest re-applies the
	// validated value (COPY preserves the source metadata, REPLACE takes it
	// from this request's headers).
	if verr := validateCopyMetadataDirective(r.Header); verr != nil {
		writeError(w, verr.code, verr.s3code, verr.msg, r.URL.Path)
		return
	}

	// Erasure-coded and multipart sources span many pieces and cannot be
	// refcounted or server-side-copied as a single piece. Reconstruct the
	// object's logical bytes and re-store them as a fresh single-piece
	// destination. The conditional, tagging, and metadata-directive
	// validations above have already run, so they apply to these sources too.
	if isErasureCodedManifest(srcManifest) || isMultipartManifest(srcManifest) {
		h.copyReconstructedSource(w, r, tenantID, dstBucket, dstKey, srcManifest)
		return
	}

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

	// Destination bucket default encryption. A plaintext
	// source (no encryption config) copied into a bucket whose
	// effective encryption mode is gateway-managed must land
	// encrypted at the destination — matching AWS S3 (CopyObject
	// honors the destination bucket's default) and the operator's
	// fail-closed intent when they configured a bucket default.
	// Sources that already carry a mode (managed/convergent,
	// client_side, or v1 handled above) keep their own encryption
	// and are routed through the verbatim/dedup paths below; the
	// effective-mode resolver leaves a non-empty source mode
	// untouched, so only truly plaintext sources are promoted here.
	if srcManifest.Encryption.Mode == "" {
		dstMode, derr := h.effectiveEncryptionMode(r.Context(), tenantID, dstBucket, srcManifest.PlacementPolicy)
		if derr != nil {
			writeError(w, http.StatusInternalServerError, "EncryptionNotConfigured", derr.Error(), r.URL.Path)
			return
		}
		if IsGatewayEncrypted(dstMode) {
			h.copyEncryptForDefault(w, r, tenantID, dstBucket, dstKey, srcManifest, srcPiece, srcProvider, dstMode)
			return
		}
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
	// unaffected. Streaming v1 re-encryption is not yet
	// implemented, the same gap that gates streaming EC.
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

	// Re-encrypt the recovered plaintext under the destination identity and
	// store it, preserving the source's mode and ManifestEncrypted flag.
	h.storeReencryptedCopy(w, r, tenantID, dstBucket, dstKey, srcManifest,
		srcProvider, srcPiece.Backend, plaintext,
		srcManifest.Encryption.Mode, srcManifest.Encryption.ManifestEncrypted)
}

// copyEncryptForDefault performs a CopyObject when the destination
// bucket's default encryption promotes an otherwise-plaintext
// copy to a gateway-managed mode. The source object carries no
// encryption config, so its piece is read in the clear, encrypted
// under a fresh DEK bound to the destination identity, and stored as
// an AAD v1 object. It mirrors copyReencrypt's encrypt half without
// the decrypt step (there is no source ciphertext to unwrap), and
// records the promoted mode in the destination manifest so the GET
// path unseals it like any other gateway-encrypted object.
//
// Only single-piece, non-EC/non-multipart sources reach here (the
// same shapes copyReencrypt handles); larger-than-ceiling objects are
// rejected because the encrypt path buffers the whole object, exactly
// like copyReencrypt and the buffered PUT/EC paths.
func (h *Handler) copyEncryptForDefault(
	w http.ResponseWriter,
	r *http.Request,
	tenantID, dstBucket, dstKey string,
	srcManifest *metadata.ObjectManifest,
	srcPiece metadata.Piece,
	srcProvider providers.StorageProvider,
	dstMode string,
) {
	if h.cfg.Encryption == nil {
		// Should be unreachable: effectiveEncryptionMode only
		// returns a gateway-managed mode when a keyring is
		// configured (it fails closed otherwise). Guard anyway so
		// a future caller cannot silently store plaintext.
		writeError(w, http.StatusInternalServerError, "EncryptionNotConfigured",
			"destination bucket has a default-encryption configuration but no gateway encryption is configured", r.URL.Path)
		return
	}

	if srcManifest.ObjectSize > MaxInMemoryObjectBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "CopyEncryptObjectTooLarge",
			fmt.Sprintf("server-side copy that must apply the destination bucket default encryption requires buffering the %d-byte object, which exceeds the in-memory ceiling of %d bytes; streaming encryption on copy is not yet implemented",
				srcManifest.ObjectSize, MaxInMemoryObjectBytes),
			r.URL.Path)
		return
	}

	body, err := srcProvider.GetPiece(r.Context(), srcPiece.PieceID, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "BackendGetFailed", err.Error(), r.URL.Path)
		return
	}
	// The source is stored as plaintext, so the bytes read here are
	// the plaintext directly. Bound the read one byte over the
	// ceiling and reject overflow, exactly like copyReencrypt's
	// defence against a stale ObjectSize.
	plaintext, rerr := io.ReadAll(io.LimitReader(body, MaxInMemoryObjectBytes+1))
	_ = body.Close()
	if rerr != nil {
		writeError(w, http.StatusBadGateway, "BackendGetFailed", rerr.Error(), r.URL.Path)
		return
	}
	if int64(len(plaintext)) > MaxInMemoryObjectBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "CopyEncryptObjectTooLarge",
			fmt.Sprintf("server-side copy source exceeds in-memory encrypt ceiling of %d bytes", MaxInMemoryObjectBytes),
			r.URL.Path)
		return
	}

	// Encrypt the plaintext source under the destination identity, recording
	// the promoted mode. There is no source manifest encryption to preserve
	// (plaintext source), so ManifestEncrypted is false.
	h.storeReencryptedCopy(w, r, tenantID, dstBucket, dstKey, srcManifest,
		srcProvider, srcPiece.Backend, plaintext, dstMode, false)
}

// copyReconstructedSource performs a CopyObject whose source is an
// erasure-coded or multipart object. Such a source spans many pieces and
// cannot be refcounted or server-side-copied as a single piece, so the
// gateway reconstructs the object's logical bytes (EC decode / multipart
// concat, transparently decrypting gateway-encrypted sources under the
// source identity) and re-stores them as a fresh single-piece destination.
// The destination's encryption mirrors the single-piece copy paths:
//
//   - gateway-encrypted source (managed / public_distribution): the
//     reconstructed bytes are true plaintext, so re-encrypt under the
//     destination identity, preserving the source mode and ManifestEncrypted.
//   - plaintext source (no encryption config): honour the destination
//     bucket's effective encryption — re-encrypt when it resolves to a
//     gateway-managed mode, else store verbatim.
//   - client_side source: the reconstructed bytes are the opaque ZK payload;
//     store them verbatim and preserve the source envelope. The gateway
//     never re-encrypts client-side ciphertext.
//
// Reconstruction is bounded by the same in-memory ceilings the GET path
// enforces (reconstructErasureCoded / reconstructMultipart return 507/413
// for oversized objects), so a large source cannot OOM the gateway.
func (h *Handler) copyReconstructedSource(
	w http.ResponseWriter,
	r *http.Request,
	tenantID, dstBucket, dstKey string,
	srcManifest *metadata.ObjectManifest,
) {
	// A gateway-encrypted source must be unsealed during reconstruction
	// (decryptFromStorage) and re-sealed before storing, both of which
	// need the gateway CMK. Fail fast with the same explicit guard as
	// copyReencrypt / copyEncryptForDefault rather than relying on the
	// implicit nil check deeper in the encrypt/decrypt pipeline.
	if IsGatewayEncrypted(srcManifest.Encryption.Mode) && h.cfg.Encryption == nil {
		writeError(w, http.StatusInternalServerError, "EncryptionNotConfigured",
			"source object is gateway-encrypted but no gateway encryption is configured", r.URL.Path)
		return
	}

	// Reconstruct the source's logical bytes into one contiguous buffer.
	var content []byte
	if isErasureCodedManifest(srcManifest) {
		decoded, rerr := h.reconstructErasureCoded(r.Context(), srcManifest)
		if rerr != nil {
			writeError(w, rerr.status, rerr.s3code, rerr.msg, r.URL.Path)
			return
		}
		content = decoded
	} else {
		bodies, total, rerr := h.reconstructMultipart(r.Context(), srcManifest)
		if rerr != nil {
			writeError(w, rerr.status, rerr.s3code, rerr.msg, r.URL.Path)
			return
		}
		assembled := make([]byte, 0, total)
		for _, b := range bodies {
			assembled = append(assembled, b...)
		}
		content = assembled
	}

	dstProvider, dstBackend, ok := h.copyDestinationBackend(srcManifest)
	if !ok {
		writeError(w, http.StatusInternalServerError, "BackendNotRegistered",
			"no registered backend available for the copy destination", r.URL.Path)
		return
	}

	switch {
	case IsGatewayEncrypted(srcManifest.Encryption.Mode):
		h.storeReencryptedCopy(w, r, tenantID, dstBucket, dstKey, srcManifest,
			dstProvider, dstBackend, content,
			srcManifest.Encryption.Mode, srcManifest.Encryption.ManifestEncrypted)
	case srcManifest.Encryption.Mode == "":
		dstMode, derr := h.effectiveEncryptionMode(r.Context(), tenantID, dstBucket, srcManifest.PlacementPolicy)
		if derr != nil {
			writeError(w, http.StatusInternalServerError, "EncryptionNotConfigured", derr.Error(), r.URL.Path)
			return
		}
		if IsGatewayEncrypted(dstMode) {
			h.storeReencryptedCopy(w, r, tenantID, dstBucket, dstKey, srcManifest,
				dstProvider, dstBackend, content, dstMode, false)
			return
		}
		h.storeVerbatimCopy(w, r, tenantID, dstBucket, dstKey, srcManifest,
			dstProvider, dstBackend, content, metadata.EncryptionConfig{})
	default:
		h.storeVerbatimCopy(w, r, tenantID, dstBucket, dstKey, srcManifest,
			dstProvider, dstBackend, content, srcManifest.Encryption)
	}
}

// storeReencryptedCopy encrypts plaintext under a fresh DEK bound to the
// destination identity, stores it as a single piece on dstProvider, and
// writes the destination manifest. It is the shared encrypt-and-store half
// of every gateway-encrypted copy path: copyReencrypt (v1 source decrypted
// first), copyEncryptForDefault (plaintext source promoted by the bucket
// default), and copyReconstructedSource (EC/multipart source reconstructed to
// plaintext). The caller supplies the EncryptionConfig.Mode to record and the
// source's ManifestEncrypted flag; the wrapped DEK, algorithm, and AAD
// version come from the encrypt step here. The plaintext is scrubbed once the
// SDK has consumed it (defence-in-depth against heap-dump exposure).
func (h *Handler) storeReencryptedCopy(
	w http.ResponseWriter,
	r *http.Request,
	tenantID, dstBucket, dstKey string,
	srcManifest *metadata.ObjectManifest,
	dstProvider providers.StorageProvider,
	dstBackend string,
	plaintext []byte,
	mode string,
	manifestEncrypted bool,
) {
	dstPieceID := newPieceID(tenantID, dstBucket, dstKey, h.cfg.Now())
	dstID := aadIdentity{
		TenantID:      tenantID,
		Bucket:        dstBucket,
		ObjectKeyHash: hashObjectKey(dstKey),
		VersionID:     dstPieceID,
	}
	newCiphertext, wrapped, eerr := h.encryptForStorage(plaintext, dstID)
	clear(plaintext)
	if eerr != nil {
		writeError(w, http.StatusInternalServerError, "EncryptionFailed", eerr.Error(), r.URL.Path)
		return
	}

	cipherHash := blake3.Sum256(newCiphertext)
	putRes, perr := dstProvider.PutPiece(r.Context(), dstPieceID, bytes.NewReader(newCiphertext), providers.PutOptions{
		ContentLength: int64(len(newCiphertext)),
		ContentType:   r.Header.Get("Content-Type"),
	})
	if perr != nil {
		writeError(w, http.StatusBadGateway, "BackendPutFailed", perr.Error(), r.URL.Path)
		return
	}

	// Record the deterministic dstPieceID we minted, not putRes.PieceID:
	// writeCopyManifest derives the manifest VersionID from Piece.PieceID and
	// the AAD above was bound to dstPieceID, so binding the recorded VersionID
	// to it keeps identity consistent on every provider.
	newPiece := metadata.Piece{
		PieceID:      dstPieceID,
		Hash:         "blake3:" + hex.EncodeToString(cipherHash[:]),
		ProviderETag: putRes.ETag,
		Backend:      dstBackend,
		Locator:      putRes.Locator,
		State:        "active",
		SizeBytes:    putRes.SizeBytes,
	}
	dstEnc := metadata.EncryptionConfig{
		Mode:              mode,
		Algorithm:         client_sdk.ContentAlgorithm,
		KeyID:             wrapped.KeyID,
		WrappedDEK:        wrapped.WrappedKey,
		WrapAlgorithm:     wrapped.WrapAlgorithm,
		ManifestEncrypted: manifestEncrypted,
		AADVersion:        AADVersionV1,
	}
	h.writeCopyManifest(w, r, tenantID, dstBucket, dstKey, srcManifest, newPiece, dstBackend, dstEnc, false)
}

// storeVerbatimCopy stores content as a single destination piece with no
// gateway encryption transform and writes the destination manifest with the
// supplied EncryptionConfig. It backs the reconstructed-copy paths where the
// bytes are stored as-is: a plaintext source landing in a plaintext bucket
// (enc = zero EncryptionConfig) and a client_side source whose opaque ZK
// payload is preserved together with its source envelope
// (enc = srcManifest.Encryption).
func (h *Handler) storeVerbatimCopy(
	w http.ResponseWriter,
	r *http.Request,
	tenantID, dstBucket, dstKey string,
	srcManifest *metadata.ObjectManifest,
	dstProvider providers.StorageProvider,
	dstBackend string,
	content []byte,
	enc metadata.EncryptionConfig,
) {
	dstPieceID := newPieceID(tenantID, dstBucket, dstKey, h.cfg.Now())
	contentHash := blake3.Sum256(content)
	putRes, perr := dstProvider.PutPiece(r.Context(), dstPieceID, bytes.NewReader(content), providers.PutOptions{
		ContentLength: int64(len(content)),
		ContentType:   r.Header.Get("Content-Type"),
	})
	if perr != nil {
		writeError(w, http.StatusBadGateway, "BackendPutFailed", perr.Error(), r.URL.Path)
		return
	}
	newPiece := metadata.Piece{
		PieceID:      dstPieceID,
		Hash:         "blake3:" + hex.EncodeToString(contentHash[:]),
		ProviderETag: putRes.ETag,
		Backend:      dstBackend,
		Locator:      putRes.Locator,
		State:        "active",
		SizeBytes:    putRes.SizeBytes,
	}
	h.writeCopyManifest(w, r, tenantID, dstBucket, dstKey, srcManifest, newPiece, dstBackend, enc, false)
}

// copyDestinationBackend picks a registered storage provider for the
// reconstructed copy destination. It prefers the source's anchor backend (the
// EC primary backend, else the first piece's backend for multipart) so the
// copy lands on the same provider as the source when possible, and falls back
// to the first piece backend that is actually registered. Returns ok=false
// when none of the candidate backends are registered.
func (h *Handler) copyDestinationBackend(m *metadata.ObjectManifest) (providers.StorageProvider, string, bool) {
	anchor := m.MigrationState.PrimaryBackend
	if anchor == "" && len(m.Pieces) > 0 {
		anchor = m.Pieces[0].Backend
	}
	if anchor != "" {
		if prov, ok := h.cfg.Providers[anchor]; ok {
			return prov, anchor, true
		}
	}
	for _, p := range m.Pieces {
		if prov, ok := h.cfg.Providers[p.Backend]; ok {
			return prov, p.Backend, true
		}
	}
	return nil, "", false
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
	// ChunkSize tracks the actual stored destination piece, never the
	// inherited source value, exactly as the live PUT path
	// (handler.go records putRes.SizeBytes) and the AAD migrator do.
	// For the verbatim/dedup/re-encrypt paths the destination piece is
	// the same length as the source, so this equals srcManifest.ChunkSize;
	// for the encrypt-on-copy path (copyEncryptForDefault) the stored
	// piece is ciphertext, larger than the plaintext source, so pinning
	// to piece.SizeBytes keeps the ChunkSize == Pieces[0].SizeBytes
	// invariant that cache-warming budgets and the migrator rely on.
	//
	// Fall back to the source ChunkSize when the stored piece size is
	// unknown: a server-side CopyPiece can succeed yet report SizeBytes==0
	// if its post-copy HEAD fails (s3_generic.CopyPiece). In that degraded
	// case the verbatim destination is byte-identical to the source, so the
	// source ChunkSize is the correct estimate — better than recording 0.
	chunkSize := piece.SizeBytes
	if chunkSize == 0 {
		chunkSize = srcManifest.ChunkSize
	}
	// The destination inherits the source's placement policy (residency,
	// backends, hot-cache) but is always written as a single piece (see
	// Pieces below) — never erasure-coded. Clear the source's
	// ErasureProfile so a reconstructed EC→single-piece copy does not
	// advertise a stale profile name that would mislead tools inspecting
	// placement (the struct documents empty ErasureProfile == single-piece).
	dstPolicy := srcManifest.PlacementPolicy
	dstPolicy.ErasureProfile = ""
	manifest := &metadata.ObjectManifest{
		TenantID:        tenantID,
		Bucket:          dstBucket,
		ObjectKey:       dstKey,
		ObjectKeyHash:   dstHash,
		VersionID:       dstVersion,
		ObjectSize:      objectSize,
		ChunkSize:       chunkSize,
		ContentHash:     contentHash,
		Encryption:      enc,
		PlacementPolicy: dstPolicy,
		Pieces:          []metadata.Piece{piece},
		Tags:            copyDestinationTags(r.Header, srcManifest.Tags),
		MigrationState: metadata.MigrationState{
			Generation:     1,
			PrimaryBackend: backend,
		},
		CreatedAt: h.cfg.Now(),
	}
	// Object metadata (Content-Type, system headers, x-amz-meta-*) per
	// x-amz-metadata-directive — COPY preserves the source's, REPLACE takes
	// this request's. Validated up front in Copy; applied here so every copy
	// path (verbatim, dedup, re-encrypt) funnels through one site, exactly
	// like Tags above.
	applyCopyObjectMetadata(manifest, r.Header, srcManifest)
	rollbackCopyPiece := func() {
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
	}
	// The copy destination is a new object version, so it inherits the
	// bucket's default Object Lock retention like any PUT.
	if err := h.applyDefaultObjectLockRetention(r.Context(), tenantID, dstBucket, manifest); err != nil {
		rollbackCopyPiece()
		writeError(w, http.StatusInternalServerError, "ObjectLockGetFailed", err.Error(), r.URL.Path)
		return
	}
	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        dstBucket,
		ObjectKeyHash: dstHash,
		VersionID:     dstVersion,
	}
	if err := h.cfg.Manifests.Put(r.Context(), mkey, manifest); err != nil {
		rollbackCopyPiece()
		writeError(w, http.StatusInternalServerError, "ManifestPutFailed", err.Error(), r.URL.Path)
		return
	}

	h.emit(tenantID, dstBucket, billing.PutRequests, 1)
	var copyCountry string
	if prov, ok := h.cfg.Providers[backend]; ok {
		copyCountry = prov.PlacementLabels().Country
	}
	h.audit(r, "COPY", tenantID, dstBucket, dstKey, piece.PieceID, backend, copyCountry)
	h.notify(r, eventObjectCreatedCopy, tenantID, dstBucket, dstKey, pieceETag(piece), manifest.VersionID, manifest.ObjectSize)

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


