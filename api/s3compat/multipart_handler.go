// Multipart upload handler methods.
//
// The S3 multipart upload protocol is three-step: CreateMultipartUpload
// returns an UploadId, the client streams 1..N UploadPart requests
// with that UploadId, and finally issues CompleteMultipartUpload with
// an XML list of (PartNumber, ETag) tuples. AbortMultipartUpload cancels
// an in-flight upload.
//
// The gateway stores per-part ciphertext as individual pieces on the
// chosen backend keyed by a piece ID derived from the UploadId and
// PartNumber. The manifest written at Complete time lists the pieces
// in ascending PartNumber order; the GET path concatenates them. The
// placement engine is consulted once at Create time so every part
// of a single upload lands on the same backend.

package s3compat

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/zeebo/blake3"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// multipartDedupEligible reports whether the upload is in a state
// where CompleteMultipartUpload could run the content_index
// lookup/register flow. It pre-screens UploadPart so we only pay
// the BLAKE3 tee cost on uploads that have any chance of deduping.
//
// The checks mirror the ones CompleteMultipartUpload re-applies
// after assembling the part list:
//
//   - dedup must be enabled on the upload's policy AND the
//     gateway must have a content_index store wired
//   - the encryption mode must produce convergent ciphertext
//     (client_side or unencrypted); managed / public_distribution
//     multipart upload sessions use a fresh random DEK per
//     session, so two clients sending the same plaintext produce
//     different ciphertext and the lookup would always miss
func (h *Handler) multipartDedupEligible(upload *multipart.Upload) bool {
	if !h.dedupEnabled(upload.Policy) {
		return false
	}
	return upload.EncMode == string(encryption.StrictZK) || upload.EncMode == ""
}

// S3 initiate-multipart-upload response body (see
// https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateMultipartUpload.html).
type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// S3 complete-multipart-upload request/response bodies.
type completeMultipartUploadRequest struct {
	XMLName xml.Name              `xml:"CompleteMultipartUpload"`
	Parts   []completeUploadEntry `xml:"Part"`
}

type completeUploadEntry struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// S3 ListMultipartUploads body.
type listMultipartUploadsResult struct {
	XMLName xml.Name               `xml:"ListMultipartUploadsResult"`
	Bucket  string                 `xml:"Bucket"`
	Uploads []multipartUploadEntry `xml:"Upload"`
}

type multipartUploadEntry struct {
	Key       string `xml:"Key"`
	UploadID  string `xml:"UploadId"`
	Initiated string `xml:"Initiated"`
}

// CreateMultipartUpload handles POST /{bucket}/{key}?uploads.
func (h *Handler) CreateMultipartUpload(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Multipart == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "multipart uploads are not enabled", r.URL.Path)
		return
	}
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
	if h.cfg.Placement == nil {
		writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable", "placement engine not configured", r.URL.Path)
		return
	}
	backend, policy, err := h.cfg.Placement.ResolveBackend(tenantID, bucket, key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidPlacement", err.Error(), r.URL.Path)
		return
	}
	provider, ok := h.cfg.Providers[backend]
	if !ok {
		writeError(w, http.StatusInternalServerError, "BackendNotRegistered", "backend "+backend+" is not in the provider registry", r.URL.Path)
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

	uploadID, err := newUploadID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", err.Error(), r.URL.Path)
		return
	}

	// Lay down the multipart session's encryption state up front:
	// managed / public_distribution uploads generate one DEK here
	// that every UploadPart reuses, so the frames all decrypt under
	// the same key when the GET path concatenates them.
	upload := &multipart.Upload{
		ID:        uploadID,
		TenantID:  tenantID,
		Bucket:    bucket,
		ObjectKey: key,
		Backend:   backend,
		Policy:    policy,
		CreatedAt: h.cfg.Now(),
		EncMode:   policy.EncryptionMode,
	}
	if IsGatewayEncrypted(policy.EncryptionMode) {
		if h.cfg.Encryption == nil {
			writeError(w, http.StatusInternalServerError, "EncryptionNotConfigured",
				"tenant policy requires managed encryption but no gateway encryption is configured", r.URL.Path)
			return
		}
		dek, gerr := client_sdk.GenerateDEK()
		if gerr != nil {
			writeError(w, http.StatusInternalServerError, "DEKGenerationFailed", gerr.Error(), r.URL.Path)
			return
		}
		wrapped, werr := h.cfg.Encryption.Wrapper.WrapDEK(dek, h.cfg.Encryption.CMK)
		if werr != nil {
			writeError(w, http.StatusInternalServerError, "DEKWrapFailed", werr.Error(), r.URL.Path)
			return
		}
		upload.DEKMaterial = []byte(dek)
		upload.WrappedDEK = wrapped.WrappedKey
		upload.WrappedKeyID = wrapped.KeyID
		upload.WrapAlgorithm = wrapped.WrapAlgorithm
		upload.ContentAlgorithm = client_sdk.ContentAlgorithm
	}

	if err := h.cfg.Multipart.Create(upload); err != nil {
		writeError(w, http.StatusInternalServerError, "MultipartCreateFailed", err.Error(), r.URL.Path)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(initiateMultipartUploadResult{Bucket: bucket, Key: key, UploadID: uploadID})
}

// UploadPart handles PUT /{bucket}/{key}?partNumber=N&uploadId=X.
func (h *Handler) UploadPart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Multipart == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "multipart uploads are not enabled", r.URL.Path)
		return
	}
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeError(w, http.StatusForbidden, "AccessDenied", err.Error(), r.URL.Path)
		return
	}
	_ = tenantID
	q := r.URL.Query()
	uploadID := q.Get("uploadId")
	partStr := q.Get("partNumber")
	partNumber, err := strconv.Atoi(partStr)
	if err != nil || partNumber < 1 || partNumber > 10000 {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "partNumber must be between 1 and 10000", r.URL.Path)
		return
	}
	upload, err := h.cfg.Multipart.Get(uploadID)
	if err != nil {
		if errors.Is(err, multipart.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NoSuchUpload", "upload "+uploadID+" not found", r.URL.Path)
			return
		}
		writeError(w, http.StatusInternalServerError, "MultipartGetFailed", err.Error(), r.URL.Path)
		return
	}
	if upload.TenantID != tenantID {
		writeError(w, http.StatusForbidden, "AccessDenied", "tenant mismatch", r.URL.Path)
		return
	}
	provider, ok := h.cfg.Providers[upload.Backend]
	if !ok {
		writeError(w, http.StatusInternalServerError, "BackendNotRegistered", "backend "+upload.Backend+" is not in the provider registry", r.URL.Path)
		return
	}

	pieceID := partPieceID(uploadID, partNumber)

	// Apply the session's encryption mode. For managed /
	// public_distribution we encrypt each part with the DEK
	// captured at Create; for client_side we enforce the
	// per-request header so a Strict ZK tenant cannot accidentally
	// ship plaintext. SizeBytes recorded on the Part is the
	// plaintext size so Complete can sum them into ObjectSize.
	body := io.Reader(r.Body)
	contentLength := r.ContentLength
	plaintextSize := r.ContentLength
	switch upload.EncMode {
	case "managed", "public_distribution":
		plaintext, rerr := io.ReadAll(r.Body)
		if rerr != nil {
			writeError(w, http.StatusBadRequest, "InvalidArgument", "read part body: "+rerr.Error(), r.URL.Path)
			return
		}
		plaintextSize = int64(len(plaintext))
		ciphertext, eerr := h.encryptWithDEK(plaintext, upload.DEKMaterial)
		if eerr != nil {
			writeError(w, http.StatusInternalServerError, "EncryptionFailed", eerr.Error(), r.URL.Path)
			return
		}
		body = bytes.NewReader(ciphertext)
		contentLength = int64(len(ciphertext))
	case "client_side":
		if r.Header.Get("X-Amz-Meta-Zk-Encryption") == "" {
			writeError(w, http.StatusForbidden, "EncryptionRequired",
				"tenant policy requires client_side encryption; set X-Amz-Meta-Zk-Encryption header", r.URL.Path)
			return
		}
	}

	// Tee the body through a BLAKE3 hasher when this upload is
	// dedup-eligible so CompleteMultipartUpload can derive a
	// per-part hash without re-reading the piece from the
	// backend. We only do this for the encryption modes that
	// produce convergent ciphertext (client_side, ""): managed /
	// public_distribution multipart cannot dedup anyway, so the
	// hash would be wasted work.
	var partHasher *blake3.Hasher
	if h.multipartDedupEligible(upload) {
		partHasher = blake3.New()
		body = io.TeeReader(body, partHasher)
	}

	res, err := provider.PutPiece(r.Context(), pieceID, body, providers.PutOptions{
		ContentLength: contentLength,
		ContentType:   r.Header.Get("Content-Type"),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "BackendPutFailed", err.Error(), r.URL.Path)
		return
	}
	if partHasher != nil {
		upload.SetPartHash(partNumber, partHasher.Sum(nil))
	}

	// Record plaintext size on the Part so CompleteMultipartUpload
	// sums the logical (user-visible) object size rather than the
	// ciphertext-on-the-wire size. For non-encrypted uploads
	// plaintextSize equals res.SizeBytes.
	recordedSize := res.SizeBytes
	if IsGatewayEncrypted(upload.EncMode) {
		recordedSize = plaintextSize
	}
	if err := h.cfg.Multipart.PutPart(uploadID, multipart.Part{
		PartNumber: partNumber,
		PieceID:    res.PieceID,
		Backend:    upload.Backend,
		ETag:       res.ETag,
		SizeBytes:  recordedSize,
		UploadedAt: h.cfg.Now(),
	}); err != nil {
		// Best-effort rollback so we don't orphan the piece on the
		// backend when the control plane loses the upload record
		// mid-flight.
		_ = provider.DeletePiece(r.Context(), pieceID)
		writeError(w, http.StatusInternalServerError, "MultipartPutPartFailed", err.Error(), r.URL.Path)
		return
	}

	h.emit(tenantID, upload.Bucket, billing.PutRequests, 1)
	if res.SizeBytes > 0 {
		h.emit(tenantID, upload.Bucket, billing.StorageBytesSeconds, uint64(res.SizeBytes))
	}

	if res.ETag != "" {
		w.Header().Set("ETag", quote(res.ETag))
	}
	w.WriteHeader(http.StatusOK)
}

// CompleteMultipartUpload handles POST /{bucket}/{key}?uploadId=X.
func (h *Handler) CompleteMultipartUpload(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Multipart == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "multipart uploads are not enabled", r.URL.Path)
		return
	}
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
	uploadID := r.URL.Query().Get("uploadId")
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // cap at 1 MiB of XML
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "read body: "+err.Error(), r.URL.Path)
		return
	}
	var req completeMultipartUploadRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", "complete body is not valid XML: "+err.Error(), r.URL.Path)
		return
	}
	if len(req.Parts) == 0 {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "CompleteMultipartUpload must list at least one part", r.URL.Path)
		return
	}
	refs := make([]multipart.PartReference, len(req.Parts))
	for i, p := range req.Parts {
		refs[i] = multipart.PartReference{PartNumber: p.PartNumber, ETag: p.ETag}
	}
	parts, upload, err := h.cfg.Multipart.Complete(uploadID, tenantID, bucket, key, refs)
	if err != nil {
		switch {
		case errors.Is(err, multipart.ErrNotFound):
			writeError(w, http.StatusNotFound, "NoSuchUpload", "upload "+uploadID+" not found", r.URL.Path)
		case errors.Is(err, multipart.ErrTenantMismatch):
			writeError(w, http.StatusForbidden, "AccessDenied", "tenant mismatch", r.URL.Path)
		case errors.Is(err, multipart.ErrUploadMismatch):
			writeError(w, http.StatusForbidden, "AccessDenied", "upload does not match bucket/key", r.URL.Path)
		case errors.Is(err, multipart.ErrPartNotFound):
			writeError(w, http.StatusBadRequest, "InvalidPart", "one or more parts are missing", r.URL.Path)
		case errors.Is(err, multipart.ErrPartETagMismatch):
			writeError(w, http.StatusBadRequest, "InvalidPart", "part etag does not match uploaded part", r.URL.Path)
		default:
			writeError(w, http.StatusInternalServerError, "MultipartCompleteFailed", err.Error(), r.URL.Path)
		}
		return
	}

	// Assemble the manifest. Pieces are stored in ascending
	// PartNumber order so the GET path can concatenate them.
	pieces := make([]metadata.Piece, 0, len(parts))
	var totalSize int64
	for _, p := range parts {
		pieces = append(pieces, metadata.Piece{
			PieceID:    p.PieceID,
			Hash:       p.ETag,
			Backend:    p.Backend,
			State:      "active",
			PartNumber: p.PartNumber,
			SizeBytes:  p.SizeBytes,
		})
		totalSize += p.SizeBytes
	}
	versionID := newPieceID(tenantID, bucket, key, h.cfg.Now())
	aggregateETag := computeMultipartETag(parts)

	// Capture the session's encryption parameters on the manifest
	// so GET can unwrap the DEK and frame-decrypt the concatenated
	// parts. For client_side mode we only record what the tenant
	// declared; we never saw the plaintext DEK.
	encCfg := metadata.EncryptionConfig{Mode: upload.EncMode}
	switch upload.EncMode {
	case "managed", "public_distribution":
		encCfg.Algorithm = upload.ContentAlgorithm
		encCfg.KeyID = upload.WrappedKeyID
		encCfg.WrappedDEK = upload.WrappedDEK
		encCfg.WrapAlgorithm = upload.WrapAlgorithm
	case "client_side":
		// Pull the algorithm from the first part's declaration;
		// the store doesn't persist headers per part, so we
		// default to the canonical algorithm. Clients that wire
		// their own algorithm still see the recorded mode.
		encCfg.Algorithm = client_sdk.ContentAlgorithm
	}
	manifest := &metadata.ObjectManifest{
		TenantID:        tenantID,
		Bucket:          bucket,
		ObjectKey:       key,
		ObjectKeyHash:   hashObjectKey(key),
		VersionID:       versionID,
		ObjectSize:      totalSize,
		ChunkSize:       firstPartSize(parts),
		Encryption:      encCfg,
		PlacementPolicy: upload.Policy,
		Pieces:          pieces,
		MigrationState: metadata.MigrationState{
			Generation:     1,
			PrimaryBackend: upload.Backend,
		},
	}

	// Multipart dedup: when the upload's policy enables
	// intra-tenant dedup AND the gateway has a content_index store
	// wired AND the encryption mode can produce convergent
	// ciphertext, compute a content hash over the assembled bytes
	// and run the content_index lookup/register flow so duplicate
	// uploads dedup correctly.
	//
	// "managed" / "public_distribution" multipart uploads are
	// excluded: CreateMultipartUpload generates a fresh random DEK
	// per session (see the IsGatewayEncrypted branch above), so
	// two clients uploading identical plaintext produce different
	// ciphertext and the content_index Lookup would always miss.
	// Single-PUT covers these modes via the convergent-DEK path
	// in dedup.go (Pattern B).
	//
	// Multi-piece uploads (len(pieces) > 1) ARE supported via the
	// content_index PieceIDs JSONB extension. The hash for an
	// N-part upload is BLAKE3(hash1 || hash2 || ... || hashN) over
	// per-part BLAKE3 digests sorted by PartNumber. Per-part
	// digests are produced at UploadPart time via a TeeReader so
	// CompleteMultipartUpload doesn't have to re-read pieces from
	// the backend in the common case.
	dedupCandidate := h.multipartDedupEligible(upload)
	if dedupCandidate {
		contentHash, hashErr := h.computeMultipartContentHash(r.Context(), upload, pieces, parts)
		if hashErr != nil {
			h.deleteUploadedParts(r.Context(), parts)
			writeError(w, http.StatusInternalServerError, "DedupHashFailed", hashErr.Error(), r.URL.Path)
			return
		}
		manifest.ContentHash = contentHash
		existing, lerr := h.cfg.ContentIndex.Lookup(r.Context(), tenantID, contentHash)
		if lerr != nil && !errors.Is(lerr, content_index.ErrNotFound) {
			h.deleteUploadedParts(r.Context(), parts)
			writeError(w, http.StatusInternalServerError, "ContentIndexLookupFailed", lerr.Error(), r.URL.Path)
			return
		}
		if existing != nil {
			if err := h.cfg.ContentIndex.IncrementRef(r.Context(), tenantID, contentHash); err != nil {
				h.deleteUploadedParts(r.Context(), parts)
				writeError(w, http.StatusInternalServerError, "ContentIndexIncrementFailed", err.Error(), r.URL.Path)
				return
			}
			// Drop ALL just-uploaded duplicate pieces; the
			// manifest will reference the canonical piece(s)
			// instead.
			h.deleteUploadedParts(r.Context(), parts)
			if err := h.redirectManifestToCanonical(manifest, existing); err != nil {
				// IncrementRef has already fired — undo it
				// so we don't permanently inflate the
				// canonical refcount.
				_, _ = h.cfg.ContentIndex.DecrementRef(r.Context(), tenantID, contentHash)
				writeError(w, http.StatusInternalServerError, "DedupRedirectFailed", err.Error(), r.URL.Path)
				return
			}
			h.emit(tenantID, bucket, billing.DedupHits, 1)
			if existing.SizeBytes > 0 {
				h.emit(tenantID, bucket, billing.DedupBytesSaved, uint64(existing.SizeBytes))
			}
		} else {
			entry := content_index.ContentIndexEntry{
				TenantID:    tenantID,
				ContentHash: contentHash,
				// Anchor PieceID/Backend/SizeBytes/ETag on the
				// first part so single-piece reverse lookups
				// (orphan GC, the piece_id index) keep working
				// for both single- and multi-piece entries.
				PieceID:   parts[0].PieceID,
				Backend:   parts[0].Backend,
				SizeBytes: parts[0].SizeBytes,
				ETag:      parts[0].ETag,
			}
			if len(pieces) > 1 {
				entry.PieceIDs = pieceRefsFromParts(parts)
				// Record total assembled size on the
				// canonical entry so dedup-hit billing
				// reports the bytes-saved across the whole
				// object rather than only the first part.
				entry.SizeBytes = totalSize
			}
			raceLost, regErr := h.registerDedupedPiece(r.Context(), entry)
			if regErr != nil {
				// Best-effort cleanup of the orphaned pieces
				// so we don't leave billable storage behind.
				h.deleteUploadedParts(r.Context(), parts)
				writeError(w, http.StatusInternalServerError, "ContentIndexRegisterFailed", regErr.Error(), r.URL.Path)
				return
			}
			if raceLost {
				// A concurrent uploader registered first.
				// Drop the duplicate pieces and redirect the
				// manifest at the canonical copy — mirrors
				// the single-PUT race-recovery path in
				// dedup.go so the on-disk and refcount
				// views stay consistent.
				h.deleteUploadedParts(r.Context(), parts)
				canonical, lookupErr := h.cfg.ContentIndex.Lookup(r.Context(), tenantID, contentHash)
				if lookupErr != nil {
					// Roll back the IncrementRef that
					// registerDedupedPiece already performed:
					// no manifest will be written for this
					// upload, so leaving the bump in place
					// would permanently inflate the canonical
					// entry's refcount and prevent eventual
					// cleanup.
					_, _ = h.cfg.ContentIndex.DecrementRef(r.Context(), tenantID, contentHash)
					writeError(w, http.StatusInternalServerError, "ContentIndexLookupFailed", lookupErr.Error(), r.URL.Path)
					return
				}
				if err := h.redirectManifestToCanonical(manifest, canonical); err != nil {
					_, _ = h.cfg.ContentIndex.DecrementRef(r.Context(), tenantID, contentHash)
					writeError(w, http.StatusInternalServerError, "DedupRedirectFailed", err.Error(), r.URL.Path)
					return
				}
				h.emit(tenantID, bucket, billing.DedupHits, 1)
				if canonical.SizeBytes > 0 {
					h.emit(tenantID, bucket, billing.DedupBytesSaved, uint64(canonical.SizeBytes))
				}
			}
		}
	}

	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: manifest.ObjectKeyHash,
		VersionID:     manifest.VersionID,
	}
	if err := h.cfg.Manifests.Put(r.Context(), mkey, manifest); err != nil {
		// Roll back any dedup state we touched before deleting
		// pieces, mirroring the single-PUT putDeduped path. We
		// MUST drop the refcount before any piece-delete; we MUST
		// NOT delete the registered canonical piece because that
		// would leave the content_index pointing at a deleted
		// piece for any concurrent uploader who Lookup'd between
		// our Register and this rollback.
		if manifest.ContentHash != "" && h.cfg.ContentIndex != nil {
			_, _ = h.cfg.ContentIndex.DecrementRef(r.Context(), tenantID, manifest.ContentHash)
		}
		// Best-effort piece cleanup. Skip every piece the
		// content_index now references (the manifest's pieces
		// — that's either the just-registered canonical pieces
		// or the existing canonical pieces on a hit /
		// lost-race). In the dedup-disabled case the manifest
		// pieces line up with parts so we delete all of them.
		keep := make(map[string]struct{})
		if manifest.ContentHash != "" {
			for _, mp := range manifest.Pieces {
				keep[mp.PieceID] = struct{}{}
			}
		}
		for _, p := range parts {
			if _, kept := keep[p.PieceID]; kept {
				continue
			}
			if provider, ok := h.cfg.Providers[p.Backend]; ok {
				_ = provider.DeletePiece(r.Context(), p.PieceID)
			}
		}
		writeError(w, http.StatusInternalServerError, "ManifestPutFailed", err.Error(), r.URL.Path)
		return
	}

	auditBackend := upload.Backend
	var auditPieceID string
	if len(manifest.Pieces) > 0 {
		auditPieceID = manifest.Pieces[0].PieceID
		auditBackend = manifest.Pieces[0].Backend
	}
	var country string
	if prov, ok := h.cfg.Providers[auditBackend]; ok {
		country = prov.PlacementLabels().Country
	}
	h.audit(r, "PUT", tenantID, bucket, key, auditPieceID, auditBackend, country)

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-version-id", manifest.VersionID)
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(completeMultipartUploadResult{
		Bucket: bucket,
		Key:    key,
		ETag:   quote(aggregateETag),
	})
}

// AbortMultipartUpload handles DELETE /{bucket}/{key}?uploadId=X.
func (h *Handler) AbortMultipartUpload(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Multipart == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "multipart uploads are not enabled", r.URL.Path)
		return
	}
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeError(w, http.StatusForbidden, "AccessDenied", err.Error(), r.URL.Path)
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	upload, parts, err := h.cfg.Multipart.Abort(uploadID, tenantID)
	if err != nil {
		switch {
		case errors.Is(err, multipart.ErrNotFound):
			// S3 Abort is idempotent.
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, multipart.ErrTenantMismatch):
			writeError(w, http.StatusForbidden, "AccessDenied", "tenant mismatch", r.URL.Path)
		default:
			writeError(w, http.StatusInternalServerError, "MultipartAbortFailed", err.Error(), r.URL.Path)
		}
		return
	}
	_ = upload
	for _, p := range parts {
		if provider, ok := h.cfg.Providers[p.Backend]; ok {
			_ = provider.DeletePiece(r.Context(), p.PieceID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListMultipartUploads handles GET /{bucket}?uploads. It exists so
// administrative tools can inspect in-flight uploads; the S3 SDKs
// also probe it on failure recovery.
func (h *Handler) ListMultipartUploads(w http.ResponseWriter, r *http.Request, bucket string) {
	if h.cfg.Multipart == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "multipart uploads are not enabled", r.URL.Path)
		return
	}
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeError(w, http.StatusForbidden, "AccessDenied", err.Error(), r.URL.Path)
		return
	}
	uploads := h.cfg.Multipart.List(tenantID, bucket)
	resp := listMultipartUploadsResult{Bucket: bucket}
	for _, u := range uploads {
		resp.Uploads = append(resp.Uploads, multipartUploadEntry{
			Key:       u.ObjectKey,
			UploadID:  u.ID,
			Initiated: u.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(resp)
}

// partPieceID is the deterministic piece ID the handler uses when
// storing a part's ciphertext. It encodes the upload ID and part
// number so Complete can address the parts without a side-table
// lookup.
func partPieceID(uploadID string, partNumber int) string {
	return fmt.Sprintf("%s-p%05d", uploadID, partNumber)
}

// newUploadID returns a fresh URL-safe multipart upload identifier.
func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// computeMultipartETag returns the standard S3 multipart ETag, which
// is the MD5 of the concatenated part MD5s followed by "-N". Each
// part ETag is expected to already be an MD5 hex digest (which
// s3_generic providers produce).
func computeMultipartETag(parts []multipart.Part) string {
	// Concatenate the raw MD5 bytes of each part ETag, then MD5 that
	// and append -N. If a part ETag isn't a 32-char hex string we
	// fall back to MD5-ing the ETag bytes directly; the suffix still
	// matches so S3 SDKs treat the object as multipart.
	hasher := md5.New()
	for _, p := range parts {
		etag := strings.Trim(p.ETag, `"`)
		if b, err := hex.DecodeString(etag); err == nil && len(b) == 16 {
			hasher.Write(b)
			continue
		}
		hasher.Write([]byte(etag))
	}
	sum := hasher.Sum(nil)
	return fmt.Sprintf("%x-%d", sum, len(parts))
}

func firstPartSize(parts []multipart.Part) int64 {
	if len(parts) == 0 {
		return 0
	}
	return parts[0].SizeBytes
}

// computeMultipartContentHash returns the canonical content hash
// for the multipart upload's pieces. For len(pieces) == 1 the
// hash is the BLAKE3 of the piece bytes (matching
// hashAssembledPieces and the single-piece dedup hash format that
// shipped before multi-piece). For len(pieces) > 1 the hash is
// BLAKE3(hash1 || hash2 || ... || hashN) over per-part BLAKE3
// digests in ascending PartNumber order, so two uploads of the
// same content with the same chunking land on the same canonical
// row.
//
// When per-part hashes are recorded on the upload session (the
// common case for dedup-eligible uploads), the function builds
// the digest entirely in memory. Otherwise it falls back to
// hashAssembledPieces, which streams every piece back from the
// backend.
func (h *Handler) computeMultipartContentHash(ctx context.Context, upload *multipart.Upload, pieces []metadata.Piece, parts []multipart.Part) (string, error) {
	if len(pieces) == 0 {
		return "", errors.New("s3compat: multipart content hash: no pieces")
	}
	// Build the per-part digest list. When any part is missing
	// a recorded hash (e.g. UploadPart wasn't dedup-eligible at
	// the time, or the in-memory store was restarted) we fall
	// back to hashAssembledPieces for correctness.
	partDigests := make([][]byte, 0, len(parts))
	for _, p := range parts {
		d, ok := upload.PartHash(p.PartNumber)
		if !ok {
			return h.hashAssembledPieces(ctx, pieces)
		}
		partDigests = append(partDigests, d)
	}
	if len(pieces) == 1 {
		// Single-piece: contentHash = BLAKE3(piece bytes) ==
		// the per-part digest. Return it verbatim so the
		// hash format matches every other single-piece dedup
		// caller.
		return formatContentHash(fmt.Sprintf("%x", partDigests[0])), nil
	}
	// Multi-piece: contentHash = BLAKE3(hash1 || ... || hashN)
	// in ascending PartNumber order (parts is already sorted by
	// PartNumber when the multipart store returns it).
	hasher := blake3.New()
	for _, d := range partDigests {
		hasher.Write(d)
	}
	return formatContentHash(fmt.Sprintf("%x", hasher.Sum(nil))), nil
}

// pieceRefsFromParts builds a content_index.PieceRef list from the
// multipart parts in ascending PartNumber order. parts is assumed
// sorted (the multipart store guarantees it).
func pieceRefsFromParts(parts []multipart.Part) []content_index.PieceRef {
	out := make([]content_index.PieceRef, 0, len(parts))
	for _, p := range parts {
		out = append(out, content_index.PieceRef{
			PieceID:    p.PieceID,
			Backend:    p.Backend,
			PartNumber: p.PartNumber,
			SizeBytes:  p.SizeBytes,
		})
	}
	return out
}

// deleteUploadedParts is the cleanup the multipart Complete
// error paths run before the manifest is written. The multipart
// session has already been consumed by Complete and the client
// cannot retry; without this the just-uploaded parts would orphan
// on the backend until orphan GC reclaims them. Best-effort:
// errors are swallowed so a transient backend hiccup does not
// shadow the underlying dedup / manifest error.
func (h *Handler) deleteUploadedParts(ctx context.Context, parts []multipart.Part) {
	for _, p := range parts {
		if prov, ok := h.cfg.Providers[p.Backend]; ok {
			_ = prov.DeletePiece(ctx, p.PieceID)
		}
	}
}

// redirectManifestToCanonical rewrites every piece on manifest to
// point at the canonical piece list recorded on existing. For a
// single-piece canonical (existing.PieceIDs == nil) the manifest
// must already have exactly one piece and the redirect simply
// copies the canonical PieceID/Backend/SizeBytes/ETag onto it. For
// a multi-piece canonical, the manifest's Pieces are matched
// against existing.PieceIDs by PartNumber so both uploads end up
// with byte-identical Pieces[i] entries.
//
// Returns an error when the canonical piece set does not cover
// every PartNumber on the manifest — this can only happen if a
// caller registered a malformed canonical entry.
func (h *Handler) redirectManifestToCanonical(manifest *metadata.ObjectManifest, existing *content_index.ContentIndexEntry) error {
	if len(existing.PieceIDs) == 0 {
		// Single-piece canonical. The manifest must also be
		// single-piece for the hash to have matched.
		if len(manifest.Pieces) != 1 {
			return fmt.Errorf("s3compat: dedup redirect: manifest has %d pieces, canonical is single-piece", len(manifest.Pieces))
		}
		manifest.Pieces[0].PieceID = existing.PieceID
		manifest.Pieces[0].Backend = existing.Backend
		manifest.Pieces[0].Hash = existing.ETag
		manifest.Pieces[0].SizeBytes = existing.SizeBytes
		manifest.MigrationState.PrimaryBackend = existing.Backend
		return nil
	}
	// Multi-piece canonical: match manifest pieces to canonical
	// pieces by PartNumber.
	byPart := make(map[int]content_index.PieceRef, len(existing.PieceIDs))
	for _, ref := range existing.PieceIDs {
		byPart[ref.PartNumber] = ref
	}
	for i := range manifest.Pieces {
		ref, ok := byPart[manifest.Pieces[i].PartNumber]
		if !ok {
			return fmt.Errorf("s3compat: dedup redirect: canonical missing part %d", manifest.Pieces[i].PartNumber)
		}
		manifest.Pieces[i].PieceID = ref.PieceID
		manifest.Pieces[i].Backend = ref.Backend
		manifest.Pieces[i].SizeBytes = ref.SizeBytes
		// Per-part canonical ETag is not recorded in the
		// content_index PieceRef (the existing.ETag covers
		// the assembled object). Leave the part Hash alone:
		// the GET path does not surface per-part ETags, only
		// the manifest's aggregate ETag.
	}
	if len(manifest.Pieces) > 0 {
		manifest.MigrationState.PrimaryBackend = manifest.Pieces[0].Backend
	}
	return nil
}
