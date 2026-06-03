package s3compat

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"

	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
)

// s3XMLNamespace is the XML namespace AWS uses on S3 configuration
// documents. Clients tolerate its absence, but emitting it keeps the
// response byte-compatible with AWS.
const s3XMLNamespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// versioningConfiguration is the S3 PUT/GET ?versioning request and
// response body:
//
//	<VersioningConfiguration xmlns="..."><Status>Enabled</Status></VersioningConfiguration>
//
// Status is omitempty so a bucket that has never been configured
// serialises as <VersioningConfiguration></VersioningConfiguration>
// with no <Status>, matching the AWS GetBucketVersioning response for
// an unconfigured bucket. MFADelete is parsed for wire-compatibility
// but not honoured (the gateway has no MFA concept).
type versioningConfiguration struct {
	XMLName   xml.Name `xml:"VersioningConfiguration"`
	XMLNS     string   `xml:"xmlns,attr,omitempty"`
	Status    string   `xml:"Status,omitempty"`
	MFADelete string   `xml:"MfaDelete,omitempty"`
}

// bucketVersioning resolves the versioning state for (tenantID,
// bucket). A nil BucketConfig store (versioning feature unwired)
// reports VersioningUnset with no error, so callers fall back to the
// non-versioned path.
func (h *Handler) bucketVersioning(ctx context.Context, tenantID, bucket string) (bucket_config.VersioningState, error) {
	if h.cfg.BucketConfig == nil {
		return bucket_config.VersioningUnset, nil
	}
	return h.cfg.BucketConfig.GetVersioning(ctx, tenantID, bucket)
}

// PutBucketVersioning handles PUT /{bucket}?versioning. It sets the
// bucket's versioning state to Enabled or Suspended. S3 has no way to
// express "unset", so an empty/absent <Status> is rejected.
func (h *Handler) PutBucketVersioning(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	bucket, key := parseBucketKey(r.URL.Path)
	if bucket == "" || key != "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "versioning is a bucket-level sub-resource; path must be /{bucket}?versioning", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket versioning is not configured on this gateway", r.URL.Path)
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeBodyReadError(w, r, err)
		return
	}
	var doc versioningConfiguration
	if err := xml.Unmarshal(raw, &doc); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", "could not parse VersioningConfiguration: "+err.Error(), r.URL.Path)
		return
	}
	state := bucket_config.VersioningState(doc.Status)
	if !state.Valid() {
		writeError(w, http.StatusBadRequest, "IllegalVersioningConfigurationException", "Status must be Enabled or Suspended", r.URL.Path)
		return
	}
	// AWS forbids suspending versioning once Object Lock is enabled on a
	// bucket: Object Lock relies on versions being immutable, so allowing
	// Suspended would let a later overwrite/complete silently replace a
	// locked version. Reject the transition rather than enter that state.
	if state == bucket_config.VersioningSuspended {
		lockCfg, lerr := h.bucketObjectLock(r, tenantID, bucket)
		if lerr != nil {
			writeError(w, http.StatusInternalServerError, "ObjectLockGetFailed", lerr.Error(), r.URL.Path)
			return
		}
		if lockCfg.Enabled {
			writeError(w, http.StatusConflict, "InvalidBucketState", "cannot suspend versioning while Object Lock is enabled on the bucket", r.URL.Path)
			return
		}
	}
	if err := h.cfg.BucketConfig.SetVersioning(r.Context(), tenantID, bucket, state); err != nil {
		writeError(w, http.StatusInternalServerError, "VersioningPutFailed", err.Error(), r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetBucketVersioning handles GET /{bucket}?versioning. It returns the
// bucket's versioning state. An unconfigured bucket returns an empty
// <VersioningConfiguration/> (no <Status>), matching AWS.
func (h *Handler) GetBucketVersioning(w http.ResponseWriter, r *http.Request, bucket string) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	if bucket == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "path must be /{bucket}?versioning", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket versioning is not configured on this gateway", r.URL.Path)
		return
	}

	state, err := h.cfg.BucketConfig.GetVersioning(r.Context(), tenantID, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "VersioningGetFailed", err.Error(), r.URL.Path)
		return
	}
	doc := versioningConfiguration{XMLNS: s3XMLNamespace, Status: string(state)}
	w.Header().Set("Content-Type", "application/xml")
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return
	}
	_ = xml.NewEncoder(w).Encode(doc)
}
