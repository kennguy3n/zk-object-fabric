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

	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

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
	if _, ok := h.cfg.Providers[backend]; !ok {
		writeError(w, http.StatusInternalServerError, "BackendNotRegistered", "backend "+backend+" is not in the provider registry", r.URL.Path)
		return
	}
	uploadID, err := newUploadID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", err.Error(), r.URL.Path)
		return
	}
	if err := h.cfg.Multipart.Create(&multipart.Upload{
		ID:        uploadID,
		TenantID:  tenantID,
		Bucket:    bucket,
		ObjectKey: key,
		Backend:   backend,
		Policy:    policy,
		CreatedAt: h.cfg.Now(),
	}); err != nil {
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
	res, err := provider.PutPiece(r.Context(), pieceID, r.Body, providers.PutOptions{
		ContentLength: r.ContentLength,
		ContentType:   r.Header.Get("Content-Type"),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "BackendPutFailed", err.Error(), r.URL.Path)
		return
	}
	if err := h.cfg.Multipart.PutPart(uploadID, multipart.Part{
		PartNumber: partNumber,
		PieceID:    res.PieceID,
		Backend:    upload.Backend,
		ETag:       res.ETag,
		SizeBytes:  res.SizeBytes,
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
	parts, upload, err := h.cfg.Multipart.Complete(uploadID, refs)
	if err != nil {
		switch {
		case errors.Is(err, multipart.ErrNotFound):
			writeError(w, http.StatusNotFound, "NoSuchUpload", "upload "+uploadID+" not found", r.URL.Path)
		case errors.Is(err, multipart.ErrPartNotFound):
			writeError(w, http.StatusBadRequest, "InvalidPart", "one or more parts are missing", r.URL.Path)
		case errors.Is(err, multipart.ErrPartETagMismatch):
			writeError(w, http.StatusBadRequest, "InvalidPart", "part etag does not match uploaded part", r.URL.Path)
		default:
			writeError(w, http.StatusInternalServerError, "MultipartCompleteFailed", err.Error(), r.URL.Path)
		}
		return
	}
	if upload.TenantID != tenantID || upload.Bucket != bucket || upload.ObjectKey != key {
		writeError(w, http.StatusForbidden, "AccessDenied", "upload does not match bucket/key", r.URL.Path)
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
	manifest := &metadata.ObjectManifest{
		TenantID:        tenantID,
		Bucket:          bucket,
		ObjectKey:       key,
		ObjectKeyHash:   hashObjectKey(key),
		VersionID:       versionID,
		ObjectSize:      totalSize,
		ChunkSize:       firstPartSize(parts),
		PlacementPolicy: upload.Policy,
		Pieces:          pieces,
		MigrationState: metadata.MigrationState{
			Generation:     1,
			PrimaryBackend: upload.Backend,
		},
	}
	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: manifest.ObjectKeyHash,
		VersionID:     manifest.VersionID,
	}
	if err := h.cfg.Manifests.Put(r.Context(), mkey, manifest); err != nil {
		// Don't orphan pieces on a manifest failure; best-effort
		// cleanup like the single-piece Put path.
		for _, p := range parts {
			if provider, ok := h.cfg.Providers[p.Backend]; ok {
				_ = provider.DeletePiece(r.Context(), p.PieceID)
			}
		}
		writeError(w, http.StatusInternalServerError, "ManifestPutFailed", err.Error(), r.URL.Path)
		return
	}

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
	upload, parts, err := h.cfg.Multipart.Abort(uploadID)
	if err != nil {
		if errors.Is(err, multipart.ErrNotFound) {
			// S3 Abort is idempotent.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, "MultipartAbortFailed", err.Error(), r.URL.Path)
		return
	}
	if upload.TenantID != tenantID {
		writeError(w, http.StatusForbidden, "AccessDenied", "tenant mismatch", r.URL.Path)
		return
	}
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
