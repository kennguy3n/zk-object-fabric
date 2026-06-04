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

// uploadAADIdentity builds the canonical AAD v1 identity that
// binds every managed multipart part's chunk tags. It uses the
// version fixed at CreateMultipartUpload (upload.VersionID) so the
// seal path (UploadPart), the consolidate path
// (assembleManagedMultipartPlaintext), and the GET path
// (getMultipart, via the manifest) all reproduce the identical AAD.
func uploadAADIdentity(u *multipart.Upload) aadIdentity {
	return aadIdentity{
		TenantID:      u.TenantID,
		Bucket:        u.Bucket,
		ObjectKeyHash: hashObjectKey(u.ObjectKey),
		VersionID:     u.VersionID,
	}
}

// partsEncryptionConfig returns the EncryptionConfig describing how
// the upload's individual parts were sealed at UploadPart time, for
// the consolidate decrypt path. Parts are AAD v1 only when the
// session carried a version up-front (post-deploy sessions); legacy
// session rows written before version_id existed sealed their parts
// with AAD = nil and must decrypt unbound. Only AADVersion is read
// by gatewayDecryptOptions, so that is the only field set here.
func partsEncryptionConfig(u *multipart.Upload) metadata.EncryptionConfig {
	if u.VersionID == "" {
		return metadata.EncryptionConfig{}
	}
	return metadata.EncryptionConfig{AADVersion: AADVersionV1}
}

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
		writeAuthError(w, r, err)
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

	// Layer the bucket default-encryption configuration (WS8.7) over
	// the placement policy before the session's encryption state is
	// fixed, so EncMode and the per-session DEK decision below reflect
	// the bucket default exactly as the single-piece PUT does.
	effMode, err := h.effectiveEncryptionMode(r.Context(), tenantID, bucket, policy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "EncryptionNotConfigured", err.Error(), r.URL.Path)
		return
	}
	policy.EncryptionMode = effMode

	uploadID, err := newUploadID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", err.Error(), r.URL.Path)
		return
	}

	// One clock read for both CreatedAt and the version's timestamp
	// component so the session's two timestamps agree exactly
	// (mirrors the single-piece PUT, which mints its pieceID from a
	// single Now()).
	now := h.cfg.Now()

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
		CreatedAt: now,
		EncMode:   policy.EncryptionMode,
		// Fix the object version now so the managed AAD v1 binding
		// can seal every part against the canonical identity at
		// UploadPart time; Complete records this same value on the
		// manifest so the GET path reproduces the identical AAD.
		VersionID: newPieceID(tenantID, bucket, key, now),
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
		writeAuthError(w, r, err)
		return
	}
	q := r.URL.Query()
	uploadID := q.Get("uploadId")
	partStr := q.Get("partNumber")
	partNumber, err := strconv.Atoi(partStr)
	if err != nil || partNumber < 1 || partNumber > 10000 {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "partNumber must be between 1 and 10000", r.URL.Path)
		return
	}
	// Get is scoped to tenantID: a cross-tenant upload_id is reported as
	// ErrNotFound (404 NoSuchUpload), not a 403, so an unauthorised
	// caller cannot probe for the existence of another tenant's upload.
	upload, err := h.cfg.Multipart.Get(tenantID, uploadID)
	if err != nil {
		if errors.Is(err, multipart.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NoSuchUpload", "upload "+uploadID+" not found", r.URL.Path)
			return
		}
		writeError(w, http.StatusInternalServerError, "MultipartGetFailed", err.Error(), r.URL.Path)
		return
	}
	if upload.TenantID != tenantID {
		// Defence-in-depth: Get already scoped the lookup to tenantID, so
		// this can only fire if a store implementation ignores the scope.
		writeError(w, http.StatusNotFound, "NoSuchUpload", "upload "+uploadID+" not found", r.URL.Path)
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
			writeBodyReadError(w, r, rerr)
			return
		}
		plaintextSize = int64(len(plaintext))

		// When dedup is enabled on the upload's policy, capture
		// BLAKE3(plaintext) for this part. CompleteMultipartUpload
		// combines the per-part digests in PartNumber order to
		// derive the canonical plaintext hash that drives the
		// deferred convergent consolidation path. The hash adds
		// negligible cost (the plaintext is already in memory)
		// and records nothing irreversible: non-dedup uploads
		// simply ignore the map.
		if h.dedupEnabled(upload.Policy) {
			ptDigest := blake3.Sum256(plaintext)
			upload.SetPlaintextPartHash(partNumber, ptDigest[:])
		}

		// partsEncryptionConfig gates the seal: AADVersion "v1"
		// (session carries a version) binds the part's chunks to
		// uploadAADIdentity; a legacy session (upload.VersionID
		// == "") seals with nil AAD, matching the AADVersion ""
		// that CompleteMultipartUpload records and the GET path
		// reads back. This is the exact signal partsEncryptionConfig
		// already feeds the consolidate decrypt path.
		ciphertext, eerr := h.encryptWithDEK(plaintext, upload.DEKMaterial, partsEncryptionConfig(upload), uploadAADIdentity(upload))
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

	// Tee the body through a BLAKE3 hasher so CompleteMultipartUpload
	// can stamp a real content hash on each Piece (and so the GET
	// integrity check has something to verify against). Before this
	// commit we only hashed when the upload was dedup-eligible, which
	// meant non-dedup multipart manifests left Piece.Hash as the
	// backend's opaque ETag — and the GET integrity check (PR-2) would
	// refuse to serve any of those pieces. The hash is also reused by
	// CompleteMultipartUpload's deferred convergent consolidation
	// path when dedup is enabled.
	partHasher := blake3.New()
	body = io.TeeReader(body, partHasher)

	res, err := provider.PutPiece(r.Context(), pieceID, body, providers.PutOptions{
		ContentLength: contentLength,
		ContentType:   r.Header.Get("Content-Type"),
	})
	if err != nil {
		// PutPiece reads from the MaxBytesReader-wrapped body
		// transitively (single-piece Put has the same shape) —
		// if the client overflows the request cap the error
		// wraps *http.MaxBytesError and writePutPieceError
		// surfaces 413 EntityTooLarge instead of a generic 502
		// so multipart clients see the same actionable error
		// they get on the single-piece Put path.
		writePutPieceError(w, r, err)
		return
	}
	upload.SetPartHash(partNumber, partHasher.Sum(nil))

	// Record plaintext size on the Part so CompleteMultipartUpload
	// sums the logical (user-visible) object size rather than the
	// ciphertext-on-the-wire size. For non-encrypted uploads
	// plaintextSize equals res.SizeBytes.
	recordedSize := res.SizeBytes
	if IsGatewayEncrypted(upload.EncMode) {
		recordedSize = plaintextSize
	}
	if err := h.cfg.Multipart.PutPart(tenantID, uploadID, multipart.Part{
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
	// Object Lock (WS8.3) pre-flight: completing a multipart upload
	// publishes a new manifest for the key, so on an Object-Lock bucket
	// that is not versioning-Enabled it would overwrite the current
	// (possibly locked) version. Mirror the Put/Copy guard and refuse
	// before the upload session is consumed by Complete().
	if !h.allowObjectLockOverwrite(w, r, tenantID, bucket, key) {
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // cap at 1 MiB of XML
	if err != nil {
		writeBodyReadError(w, r, err)
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
	// DEK scrubbing for multipart sessions: the session's raw DEK
	// (upload.DEKMaterial) is held in the multipart store across
	// every UploadPart for the lifetime of the upload, so the
	// PR-#74 "defer clear(dek) immediately after generation"
	// pattern used by single-piece encrypt cannot apply here — the
	// DEK must outlive the CreateMultipartUpload handler so
	// subsequent UploadPart calls can seal each part. Scrub
	// happens at session-end: Complete (here) and Abort (below)
	// own the only paths that retire the upload from the store.
	// Place the defer after Complete returns so it fires on every
	// subsequent return from this handler, including the
	// dedupManagedMultipartHit / dedupManagedMultipartMiss
	// branches that read upload.DEKMaterial via
	// assembleManagedMultipartPlaintext. Closes the gap noted in
	// PR #74 review (3299218688).
	defer scrubUploadDEK(upload)
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
	// Each piece's Hash is BLAKE3 of the ciphertext-on-the-wire
	// captured by the UploadPart hasher; the backend's ETag goes
	// into ProviderETag so we keep both — Hash is what the GET
	// integrity verifier re-computes from the bytes, ETag is the
	// backend's opaque identifier (some backends return a
	// multipart-style concatenated MD5 that has no relationship
	// to the bytes, so it cannot be used for verification).
	pieces := make([]metadata.Piece, 0, len(parts))
	var totalSize int64
	for _, p := range parts {
		hash := ""
		if raw, ok := upload.PartHash(p.PartNumber); ok && len(raw) > 0 {
			hash = "blake3:" + hex.EncodeToString(raw)
		}
		pieces = append(pieces, metadata.Piece{
			PieceID:      p.PieceID,
			Hash:         hash,
			ProviderETag: p.ETag,
			Backend:      p.Backend,
			State:        "active",
			PartNumber:   p.PartNumber,
			SizeBytes:    p.SizeBytes,
		})
		totalSize += p.SizeBytes
	}
	// Reuse the version fixed at CreateMultipartUpload. Managed
	// parts were sealed with AAD v1 bound to this exact version, so
	// minting a fresh one here would make the GET path rebuild a
	// different AAD and every part's tag would fail to open.
	//
	// aadBound also drives EncryptionConfig.AADVersion below:
	// upload.VersionID is empty only for a legacy session row
	// created before version_id was persisted — its parts were
	// sealed by the old code path with AAD = nil, so we must record
	// AADVersion = "" and mint a throwaway version (the empty AAD
	// doesn't bind to it anyway). Sessions created post-deploy
	// always carry a version and are bound.
	aadBound := upload.VersionID != ""
	versionID := upload.VersionID
	if !aadBound {
		versionID = newPieceID(tenantID, bucket, key, h.cfg.Now())
	}
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
		// Record AAD v1 only when the parts were actually sealed
		// with the bound identity (see aadBound above). The
		// deferred-convergent consolidation path overwrites this
		// EncryptionConfig wholesale with convergentEnc
		// (AADVersion ""), so a deduped object correctly stays
		// unbound.
		if aadBound {
			encCfg.AADVersion = AADVersionV1
		}
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
		CreatedAt: h.cfg.Now(),
	}

	// Multipart dedup. Two flows feed the content_index, selected
	// by the encryption mode of the upload:
	//
	//  1. client_side / unencrypted (multipartDedupEligible) —
	//     ciphertext is already convergent (the client either
	//     sent it that way or there is no encryption at all). The
	//     content hash is BLAKE3 over the assembled ciphertext
	//     bytes; per-part digests are produced at UploadPart time
	//     via a TeeReader so CompleteMultipartUpload doesn't
	//     re-read pieces from the backend. Multi-piece uploads
	//     (len(pieces) > 1) are supported via the content_index
	//     PieceIDs JSONB extension.
	//
	//  2. managed / public_distribution — CreateMultipartUpload
	//     generated a random per-session DEK so the parts on the
	//     backend are NOT convergent. The handler instead hashes
	//     plaintext per-part during UploadPart (see the managed/
	//     public_distribution case there) and runs a "deferred
	//     convergent consolidation" pass: it looks up the
	//     content_index by plaintext hash, and on miss decrypts
	//     every part with the random DEK and re-encrypts them
	//     under a convergent DEK derived from the same plaintext
	//     hash, producing a single consolidated piece that future
	//     uploads of the same content can dedup against.
	//
	// The two flows are mutually exclusive: the encryption mode
	// dictates which one fires.
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
	} else if h.dedupEnabled(upload.Policy) && IsGatewayEncrypted(upload.EncMode) && upload.PlaintextPartHashCount() == len(parts) {
		// Deferred convergent consolidation for managed /
		// public_distribution multipart uploads. The parts on the
		// backend were sealed with the session's random DEK, so
		// looking up by ciphertext hash would always miss. We
		// instead derive the canonical plaintext hash from the
		// per-part plaintext digests recorded at UploadPart time
		// and dispatch through dedupManagedMultipart, which either
		// redirects the manifest at an existing canonical piece
		// (HIT) or consolidates the parts into a single
		// convergent-encrypted piece and registers a fresh entry
		// (MISS).
		if !h.dedupManagedMultipart(w, r, tenantID, bucket, upload, parts, totalSize, manifest) {
			return
		}
	}

	if err := h.applyDefaultObjectLockRetention(r.Context(), tenantID, bucket, manifest); err != nil {
		// Mirror the manifest-Put refcount rollback; orphaned
		// pieces (if any) are reclaimed by the background GC.
		if manifest.ContentHash != "" && h.cfg.ContentIndex != nil {
			_, _ = h.cfg.ContentIndex.DecrementRef(r.Context(), tenantID, manifest.ContentHash)
		}
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
		writeAuthError(w, r, err)
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	upload, parts, err := h.cfg.Multipart.Abort(uploadID, tenantID)
	// DEK scrubbing for multipart sessions: see the matching
	// defer in CompleteMultipartUpload above. Abort is the second
	// terminal path that retires a multipart session from the
	// store; once we own the *Upload pointer no other handler
	// will read its DEKMaterial, so the raw key can be zeroed
	// before the handler returns. Closes the gap noted in PR #74
	// review (3299218688).
	defer scrubUploadDEK(upload)
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
		writeAuthError(w, r, err)
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

// scrubUploadDEK zeroes the raw DEK held on a multipart Upload
// once the session has reached a terminal state
// (CompleteMultipartUpload or AbortMultipartUpload). nil-safe so
// callers can `defer scrubUploadDEK(upload)` immediately after
// the store's Complete / Abort returns, before checking the
// error — if the store returned ErrNotFound / ErrTenantMismatch
// / ErrUploadMismatch the upload pointer is nil and this is a
// no-op.
//
// Unlike single-piece encrypt where the raw DEK lives only
// inside the function that generates it, multipart's
// DEKMaterial is held in the multipart store across every
// UploadPart call so each part can be sealed with the same key.
// The store is in-memory today (multipart.MemoryStore) but the
// schema (api/s3compat/multipart/schema.sql) documents
// DEKMaterial as "in-memory only, never persisted" — only the
// CMK-wrapped form lands in Postgres. That makes Complete and
// Abort the right terminal points to clear the raw bytes: at
// that moment no other handler will ever read DEKMaterial again
// (the upload row is deleted), and the goroutine that holds the
// last reference is about to return to the http.Server pool.
//
// Closes the gap noted in PR #74 review (3299218688).
func scrubUploadDEK(upload *multipart.Upload) {
	if upload == nil {
		return
	}
	if len(upload.DEKMaterial) > 0 {
		clear(upload.DEKMaterial)
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
		// The content_index does not yet carry a per-piece BLAKE3
		// hash, so we clear the manifest's Hash on dedup redirect.
		// The GET path treats empty Hash as "no integrity claim"
		// (legacy behaviour) rather than verifying against the
		// backend's ETag and returning 502. Adding a PieceHash
		// column to content_index is tracked as a follow-up so
		// dedup-redirected manifests also get integrity checks.
		manifest.Pieces[0].Hash = ""
		manifest.Pieces[0].ProviderETag = existing.ETag
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

// dedupManagedMultipart runs the deferred convergent consolidation
// dedup pass for a managed / public_distribution multipart upload.
// On success it returns true with manifest mutated to reference
// either an existing canonical piece (HIT) or a freshly written
// consolidated piece (MISS). On any error it writes a structured
// HTTP error response and returns false; the caller MUST return
// without writing the manifest in that case.
//
// Preconditions enforced by the caller: dedup is enabled on
// upload.Policy, upload.EncMode is gateway-encrypted, and every
// part in parts has a recorded plaintext digest.
func (h *Handler) dedupManagedMultipart(
	w http.ResponseWriter,
	r *http.Request,
	tenantID, bucket string,
	upload *multipart.Upload,
	parts []multipart.Part,
	totalSize int64,
	manifest *metadata.ObjectManifest,
) bool {
	ctx := r.Context()

	// Combine per-part plaintext hashes in PartNumber order
	// (parts is already sorted by the multipart store) to form
	// the canonical plaintext hash. If any part is missing a
	// digest the precondition was violated; bail out without
	// touching the parts so the non-dedup write path below can
	// still complete.
	combined := blake3.New()
	for _, p := range parts {
		d, ok := upload.PlaintextPartHash(p.PartNumber)
		if !ok {
			return true
		}
		combined.Write(d)
	}
	combinedDigest := combined.Sum(nil)
	plaintextHash := formatContentHash(fmt.Sprintf("%x", combinedDigest))

	existing, lerr := h.cfg.ContentIndex.LookupByPlaintextHash(ctx, tenantID, plaintextHash)
	if lerr != nil && !errors.Is(lerr, content_index.ErrNotFound) {
		h.deleteUploadedParts(ctx, parts)
		writeError(w, http.StatusInternalServerError, "ContentIndexLookupFailed", lerr.Error(), r.URL.Path)
		return false
	}

	if existing != nil {
		return h.dedupManagedMultipartHit(w, r, tenantID, bucket, upload, parts, combinedDigest, existing, manifest)
	}
	return h.dedupManagedMultipartMiss(w, r, tenantID, bucket, upload, parts, totalSize, combinedDigest, plaintextHash, manifest)
}

// dedupManagedMultipartHit is the HIT branch of
// dedupManagedMultipart: an existing canonical piece is referenced
// for the same plaintext, so we drop the just-uploaded random-DEK
// parts and redirect the manifest at the canonical piece. The
// convergent DEK is re-derived from the plaintext digest so this
// upload's manifest carries a correctly-wrapped DEK that decrypts
// the canonical piece.
func (h *Handler) dedupManagedMultipartHit(
	w http.ResponseWriter,
	r *http.Request,
	tenantID, bucket string,
	upload *multipart.Upload,
	parts []multipart.Part,
	combinedDigest []byte,
	existing *content_index.ContentIndexEntry,
	manifest *metadata.ObjectManifest,
) bool {
	ctx := r.Context()

	if err := h.cfg.ContentIndex.IncrementRef(ctx, tenantID, existing.ContentHash); err != nil {
		h.deleteUploadedParts(ctx, parts)
		writeError(w, http.StatusInternalServerError, "ContentIndexIncrementFailed", err.Error(), r.URL.Path)
		return false
	}

	convergentEnc, derr := h.deriveConvergentEncryptionConfig(combinedDigest, tenantID, upload.EncMode)
	if derr != nil {
		_, _ = h.cfg.ContentIndex.DecrementRef(ctx, tenantID, existing.ContentHash)
		h.deleteUploadedParts(ctx, parts)
		writeError(w, http.StatusInternalServerError, "DedupConvergentDEKFailed", derr.Error(), r.URL.Path)
		return false
	}

	// Drop ALL uploaded parts (encrypted with the session's random
	// DEK). The canonical piece referenced below is the one GET
	// will read.
	h.deleteUploadedParts(ctx, parts)

	manifest.Pieces = []metadata.Piece{{
		PieceID:      existing.PieceID,
		Backend:      existing.Backend,
		// See redirectManifestToCanonical: dedup redirect leaves
		// Hash empty until content_index grows a PieceHash field.
		Hash:         "",
		ProviderETag: existing.ETag,
		SizeBytes:    existing.SizeBytes,
		State:        "active",
	}}
	manifest.ContentHash = existing.ContentHash
	manifest.ChunkSize = existing.SizeBytes
	manifest.MigrationState.PrimaryBackend = existing.Backend
	manifest.Encryption = convergentEnc

	h.emit(tenantID, bucket, billing.DedupHits, 1)
	if existing.SizeBytes > 0 {
		h.emit(tenantID, bucket, billing.DedupBytesSaved, uint64(existing.SizeBytes))
	}
	return true
}

// dedupManagedMultipartMiss is the MISS branch: there is no
// existing canonical piece, so we read every part back, decrypt
// with the session's random DEK, re-encrypt the assembled
// plaintext under a convergent DEK, and write a single
// consolidated piece. The new piece is registered in the
// content_index with both its ciphertext content_hash and the
// plaintext_hash key future uploads will look up against. A
// concurrent uploader winning the Register race triggers the
// standard fall-back-to-canonical recovery.
func (h *Handler) dedupManagedMultipartMiss(
	w http.ResponseWriter,
	r *http.Request,
	tenantID, bucket string,
	upload *multipart.Upload,
	parts []multipart.Part,
	totalSize int64,
	combinedDigest []byte,
	plaintextHash string,
	manifest *metadata.ObjectManifest,
) bool {
	ctx := r.Context()

	plaintext, asmErr := h.assembleManagedMultipartPlaintext(ctx, upload, parts, totalSize)
	if asmErr != nil {
		h.deleteUploadedParts(ctx, parts)
		writeError(w, http.StatusInternalServerError, "DedupConsolidateFailed", asmErr.Error(), r.URL.Path)
		return false
	}

	convergentDEK, derr := client_sdk.DeriveConvergentDEK(combinedDigest, tenantID)
	if derr != nil {
		h.deleteUploadedParts(ctx, parts)
		writeError(w, http.StatusInternalServerError, "DedupConvergentDEKFailed", derr.Error(), r.URL.Path)
		return false
	}
	// DEK scrubbing via defer: zero the raw convergent DEK on
	// every return path of this function. The DEK is
	// deterministically re-derivable from (combinedDigest,
	// tenantID), so the scrub bounds the in-memory exposure
	// rather than providing forward secrecy — defence in depth
	// against heap-dump / paged-out memory between encrypt and
	// GC. Mirrors the pattern applied to encryptForStorage,
	// streamEncryptForStorage, and prepareDedupedPutPatternB
	// after PR #74 review (3299180089). EncryptObject copies the
	// DEK into the encryptReader for convergent-nonce derivation
	// before returning, so clearing the caller's backing array
	// does not corrupt the in-flight stream.
	defer clear(convergentDEK)
	encReader, eerr := client_sdk.EncryptObject(bytes.NewReader(plaintext), convergentDEK, client_sdk.Options{ConvergentNonce: true})
	if eerr != nil {
		h.deleteUploadedParts(ctx, parts)
		writeError(w, http.StatusInternalServerError, "DedupEncryptFailed", eerr.Error(), r.URL.Path)
		return false
	}
	consolidatedCiphertext, rerr := io.ReadAll(encReader)
	if rerr != nil {
		h.deleteUploadedParts(ctx, parts)
		writeError(w, http.StatusInternalServerError, "DedupReadCiphertextFailed", rerr.Error(), r.URL.Path)
		return false
	}

	provider, ok := h.cfg.Providers[upload.Backend]
	if !ok {
		h.deleteUploadedParts(ctx, parts)
		writeError(w, http.StatusInternalServerError, "BackendNotRegistered",
			"backend "+upload.Backend+" is not in the provider registry", r.URL.Path)
		return false
	}
	consolidatedPieceID := newPieceID(tenantID, upload.Bucket, upload.ObjectKey, h.cfg.Now())
	putRes, perr := provider.PutPiece(ctx, consolidatedPieceID, bytes.NewReader(consolidatedCiphertext), providers.PutOptions{
		ContentLength: int64(len(consolidatedCiphertext)),
	})
	if perr != nil {
		h.deleteUploadedParts(ctx, parts)
		writeError(w, http.StatusBadGateway, "BackendPutFailed", perr.Error(), r.URL.Path)
		return false
	}

	contentHash := formatContentHash(blake3Hex(consolidatedCiphertext))
	wrapped, werr := h.cfg.Encryption.Wrapper.WrapDEK(convergentDEK, h.cfg.Encryption.CMK)
	if werr != nil {
		_ = provider.DeletePiece(ctx, putRes.PieceID)
		h.deleteUploadedParts(ctx, parts)
		writeError(w, http.StatusInternalServerError, "DedupWrapDEKFailed", werr.Error(), r.URL.Path)
		return false
	}
	convergentEnc := metadata.EncryptionConfig{
		Mode:          upload.EncMode,
		Algorithm:     client_sdk.ContentAlgorithm,
		KeyID:         wrapped.KeyID,
		WrappedDEK:    wrapped.WrappedKey,
		WrapAlgorithm: wrapped.WrapAlgorithm,
	}

	raceLost, regErr := h.registerDedupedPiece(ctx, content_index.ContentIndexEntry{
		TenantID:      tenantID,
		ContentHash:   contentHash,
		PlaintextHash: plaintextHash,
		PieceID:       putRes.PieceID,
		Backend:       upload.Backend,
		SizeBytes:     putRes.SizeBytes,
		ETag:          putRes.ETag,
	})
	if regErr != nil {
		_ = provider.DeletePiece(ctx, putRes.PieceID)
		h.deleteUploadedParts(ctx, parts)
		writeError(w, http.StatusInternalServerError, "ContentIndexRegisterFailed", regErr.Error(), r.URL.Path)
		return false
	}

	// The original per-part pieces are now unreferenced regardless
	// of which branch we took. Delete them in a single pass.
	h.deleteUploadedParts(ctx, parts)

	if raceLost {
		// A concurrent uploader registered the canonical first.
		// Drop the just-written consolidated piece and redirect
		// the manifest at the canonical row — mirroring the
		// single-PUT race-recovery in dedup.go.
		_ = provider.DeletePiece(ctx, putRes.PieceID)
		canonical, lookupErr := h.cfg.ContentIndex.Lookup(ctx, tenantID, contentHash)
		if lookupErr != nil {
			_, _ = h.cfg.ContentIndex.DecrementRef(ctx, tenantID, contentHash)
			writeError(w, http.StatusInternalServerError, "ContentIndexLookupFailed", lookupErr.Error(), r.URL.Path)
			return false
		}
		manifest.Pieces = []metadata.Piece{{
			PieceID:      canonical.PieceID,
			Backend:      canonical.Backend,
			// See redirectManifestToCanonical.
			Hash:         "",
			ProviderETag: canonical.ETag,
			SizeBytes:    canonical.SizeBytes,
			State:        "active",
		}}
		manifest.ContentHash = canonical.ContentHash
		manifest.ChunkSize = canonical.SizeBytes
		manifest.MigrationState.PrimaryBackend = canonical.Backend
		manifest.Encryption = convergentEnc
		h.emit(tenantID, bucket, billing.DedupHits, 1)
		if canonical.SizeBytes > 0 {
			h.emit(tenantID, bucket, billing.DedupBytesSaved, uint64(canonical.SizeBytes))
		}
		return true
	}

	// contentHash (computed at line 1094) is
	// formatContentHash(blake3Hex(consolidatedCiphertext)) —
	// the BLAKE3 of the exact ciphertext bytes PUT to the backend
	// above. That is precisely what pieceintegrity.Verify
	// re-computes on the cache-miss GET path: hex of
	// blake3.Sum256(ciphertext_returned_by_backend). Stamping it
	// into Piece.Hash gives dedup-consolidated multipart
	// manifests full GET-path integrity coverage, closing the
	// observability gap the comment block above used to
	// describe. combinedDigest is a separate variable that
	// hashes plaintext digests — unsuitable for the verifier and
	// not used here.
	manifest.Pieces = []metadata.Piece{{
		PieceID:      putRes.PieceID,
		Backend:      upload.Backend,
		Hash:         contentHash,
		ProviderETag: putRes.ETag,
		SizeBytes:    putRes.SizeBytes,
		State:        "active",
	}}
	manifest.ContentHash = contentHash
	manifest.ChunkSize = putRes.SizeBytes
	manifest.MigrationState.PrimaryBackend = upload.Backend
	manifest.Encryption = convergentEnc
	return true
}

// assembleManagedMultipartPlaintext reads every part back from its
// backend, decrypts it with the session's random DEK, and returns
// the concatenated plaintext. The decrypted bytes are held in
// memory because the consolidation path needs to feed them into a
// single convergent EncryptObject call. Used only by the MISS
// branch of dedupManagedMultipart.
func (h *Handler) assembleManagedMultipartPlaintext(ctx context.Context, upload *multipart.Upload, parts []multipart.Part, totalSize int64) ([]byte, error) {
	out := make([]byte, 0, totalSize)
	for _, p := range parts {
		prov, ok := h.cfg.Providers[p.Backend]
		if !ok {
			return nil, fmt.Errorf("s3compat: consolidate: backend %q not registered", p.Backend)
		}
		rc, err := prov.GetPiece(ctx, p.PieceID, nil)
		if err != nil {
			return nil, fmt.Errorf("s3compat: consolidate get piece %s: %w", p.PieceID, err)
		}
		ciphertext, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			return nil, fmt.Errorf("s3compat: consolidate read piece %s: %w", p.PieceID, rerr)
		}
		pt, derr := h.decryptWithDEK(ciphertext, upload.DEKMaterial, partsEncryptionConfig(upload), uploadAADIdentity(upload))
		if derr != nil {
			return nil, fmt.Errorf("s3compat: consolidate decrypt piece %s: %w", p.PieceID, derr)
		}
		out = append(out, pt...)
	}
	return out, nil
}

// deriveConvergentEncryptionConfig builds an EncryptionConfig
// wrapping the convergent DEK derived from combinedDigest. Used by
// the HIT branch of the managed multipart consolidation flow,
// which does not need the unwrapped DEK itself (the canonical
// piece is read at GET time, not now) but does need the wrapped
// form on the manifest so future GETs can decrypt.
func (h *Handler) deriveConvergentEncryptionConfig(combinedDigest []byte, tenantID, encMode string) (metadata.EncryptionConfig, error) {
	if h.cfg.Encryption == nil {
		return metadata.EncryptionConfig{}, errors.New("s3compat: gateway encryption is not configured")
	}
	dek, err := client_sdk.DeriveConvergentDEK(combinedDigest, tenantID)
	if err != nil {
		return metadata.EncryptionConfig{}, fmt.Errorf("s3compat: derive convergent dek: %w", err)
	}
	// DEK scrubbing via defer: this function only needs the
	// convergent DEK long enough to ask the Wrapper to envelope
	// it; after WrapDEK returns the raw DEK is dead weight on the
	// goroutine heap. Mirrors the defer-scrub pattern PR #74
	// applied to encryptForStorage / streamEncryptForStorage /
	// prepareDedupedPutPatternB / dedupManagedMultipartMiss. The
	// convergent DEK is re-derivable from (combinedDigest,
	// tenantID) so the scrub bounds in-memory exposure rather
	// than providing forward secrecy.
	defer clear(dek)
	wrapped, err := h.cfg.Encryption.Wrapper.WrapDEK(dek, h.cfg.Encryption.CMK)
	if err != nil {
		return metadata.EncryptionConfig{}, fmt.Errorf("s3compat: wrap convergent dek: %w", err)
	}
	return metadata.EncryptionConfig{
		Mode:          encMode,
		Algorithm:     client_sdk.ContentAlgorithm,
		KeyID:         wrapped.KeyID,
		WrappedDEK:    wrapped.WrappedKey,
		WrapAlgorithm: wrapped.WrapAlgorithm,
	}, nil
}
