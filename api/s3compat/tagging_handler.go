// WS8.1 — S3 object tagging (`?tagging`).
//
// Implements PutObjectTagging, GetObjectTagging, and
// DeleteObjectTagging. Tags live on the object manifest's Tags map
// (metadata.ObjectManifest.Tags) and are persisted as part of the
// opaque manifest JSONB body by every ManifestStore implementation —
// the control plane never inspects them. See docs/PROPOSAL.md §15.1.1.
//
// Only OBJECT tagging is implemented. Bucket-level tagging (`?tagging`
// on a bucket path with no key) is not supported and returns 501
// NotImplemented, matching the gateway's posture for other
// unimplemented bucket sub-resources.
package s3compat

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"sort"

	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
)

// S3 object-tagging limits (docs/PROPOSAL.md §15.1.1). These match the
// AWS S3 service limits so off-the-shelf tooling behaves identically.
const (
	maxObjectTags     = 10
	maxTagKeyLength   = 128
	maxTagValueLength = 256
)

// s3TaggingNamespace is the XML namespace AWS uses on the Tagging
// document. Clients tolerate its absence, but emitting it keeps the
// response byte-compatible with AWS for snapshot-based conformance.
const s3TaggingNamespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// tagEntry is a single <Tag> element.
type tagEntry struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// taggingDocument is the S3 Tagging request/response body:
//
//	<Tagging><TagSet><Tag><Key>..</Key><Value>..</Value></Tag>..</TagSet></Tagging>
type taggingDocument struct {
	XMLName xml.Name   `xml:"Tagging"`
	XMLNS   string     `xml:"xmlns,attr,omitempty"`
	TagSet  []tagEntry `xml:"TagSet>Tag"`
}

// PutObjectTagging handles PUT /{bucket}/{key}?tagging. It replaces
// the object's entire tag set with the supplied <Tagging> document.
func (h *Handler) PutObjectTagging(w http.ResponseWriter, r *http.Request) {
	tenantID, bucket, key, ok := h.authObjectTagging(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeBodyReadError(w, r, err)
		return
	}
	var doc taggingDocument
	if err := xml.Unmarshal(raw, &doc); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML",
			"the XML you provided was not well-formed or did not validate against our published schema", r.URL.Path)
		return
	}
	tags, verr := validateTagSet(doc.TagSet)
	if verr != nil {
		writeError(w, verr.code, verr.s3code, verr.msg, r.URL.Path)
		return
	}

	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: hashObjectKey(key),
		VersionID:     r.URL.Query().Get("versionId"),
	}
	manifest, err := h.cfg.Manifests.Get(r.Context(), mkey)
	if err != nil {
		writeManifestGetError(w, r, err)
		return
	}

	// Persist the tag set onto the resolved version. We re-Put using
	// the manifest's own VersionID (not the possibly-empty request
	// versionId) so the write targets exactly the version we read.
	// Both ManifestStore backends treat "latest" as the most-recently
	// written/updated version, so re-Putting the latest version (the
	// default, versionId omitted) keeps it latest; this mirrors the
	// existing overwrite-PUT semantics.
	if len(tags) == 0 {
		manifest.Tags = nil
	} else {
		manifest.Tags = tags
	}
	putKey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: hashObjectKey(key),
		VersionID:     manifest.VersionID,
	}
	if err := h.cfg.Manifests.Put(r.Context(), putKey, manifest); err != nil {
		writeError(w, http.StatusInternalServerError, "ManifestPutFailed", err.Error(), r.URL.Path)
		return
	}
	if manifest.VersionID != "" {
		w.Header().Set("x-amz-version-id", manifest.VersionID)
	}
	w.WriteHeader(http.StatusOK)
}

// GetObjectTagging handles GET /{bucket}/{key}?tagging. It returns the
// object's tag set (an empty TagSet when the object has no tags).
func (h *Handler) GetObjectTagging(w http.ResponseWriter, r *http.Request) {
	tenantID, bucket, key, ok := h.authObjectTagging(w, r)
	if !ok {
		return
	}
	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: hashObjectKey(key),
		VersionID:     r.URL.Query().Get("versionId"),
	}
	manifest, err := h.cfg.Manifests.Get(r.Context(), mkey)
	if err != nil {
		writeManifestGetError(w, r, err)
		return
	}

	doc := taggingDocument{XMLNS: s3TaggingNamespace, TagSet: tagsToEntries(manifest.Tags)}
	if manifest.VersionID != "" {
		w.Header().Set("x-amz-version-id", manifest.VersionID)
	}
	writeTaggingXML(w, doc)
}

// DeleteObjectTagging handles DELETE /{bucket}/{key}?tagging. It
// removes the object's entire tag set and returns 204.
func (h *Handler) DeleteObjectTagging(w http.ResponseWriter, r *http.Request) {
	tenantID, bucket, key, ok := h.authObjectTagging(w, r)
	if !ok {
		return
	}
	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: hashObjectKey(key),
		VersionID:     r.URL.Query().Get("versionId"),
	}
	manifest, err := h.cfg.Manifests.Get(r.Context(), mkey)
	if err != nil {
		writeManifestGetError(w, r, err)
		return
	}
	if manifest.Tags != nil {
		manifest.Tags = nil
		putKey := manifest_store.ManifestKey{
			TenantID:      tenantID,
			Bucket:        bucket,
			ObjectKeyHash: hashObjectKey(key),
			VersionID:     manifest.VersionID,
		}
		if err := h.cfg.Manifests.Put(r.Context(), putKey, manifest); err != nil {
			writeError(w, http.StatusInternalServerError, "ManifestPutFailed", err.Error(), r.URL.Path)
			return
		}
	}
	if manifest.VersionID != "" {
		w.Header().Set("x-amz-version-id", manifest.VersionID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// authObjectTagging performs the shared preamble for the three tagging
// handlers: authenticate, ensure a manifest store is configured, parse
// the path, and reject bucket-level tagging. On any failure it writes
// the response and returns ok=false.
func (h *Handler) authObjectTagging(w http.ResponseWriter, r *http.Request) (tenantID, bucket, key string, ok bool) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return "", "", "", false
	}
	if h.cfg.Manifests == nil {
		writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable", "manifest store not configured", r.URL.Path)
		return "", "", "", false
	}
	bucket, key = parseBucketKey(r.URL.Path)
	if bucket == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "path must be /{bucket}/{key...}", r.URL.Path)
		return "", "", "", false
	}
	if key == "" {
		// Bucket-level tagging is not implemented; only object
		// tagging is. Report it the same way the gateway reports any
		// other unimplemented sub-resource.
		writeError(w, http.StatusNotImplemented, "NotImplemented",
			"bucket-level tagging is not implemented by this gateway", r.URL.Path)
		return "", "", "", false
	}
	return tenantID, bucket, key, true
}

// writeManifestGetError maps a ManifestStore.Get error to the right S3
// response: ErrNotFound → 404 NoSuchKey, anything else → 500.
func writeManifestGetError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, manifest_store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "NoSuchKey", "the specified key does not exist", r.URL.Path)
		return
	}
	writeError(w, http.StatusInternalServerError, "ManifestGetFailed", err.Error(), r.URL.Path)
}

// writeTaggingXML serialises a Tagging document with the standard XML
// header and content type.
func writeTaggingXML(w http.ResponseWriter, doc taggingDocument) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return
	}
	_ = xml.NewEncoder(w).Encode(doc)
}

// tagsToEntries converts the manifest's tag map into a stable,
// deterministically-ordered slice of <Tag> entries. AWS does not
// guarantee tag ordering, but a stable order keeps responses
// reproducible for snapshot testing.
func tagsToEntries(tags map[string]string) []tagEntry {
	if len(tags) == 0 {
		return nil
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]tagEntry, 0, len(tags))
	for _, k := range keys {
		out = append(out, tagEntry{Key: k, Value: tags[k]})
	}
	return out
}

// tagValidationError carries the S3 error triple for an invalid tag
// set so the caller can render it through writeError.
type tagValidationError struct {
	code   int
	s3code string
	msg    string
}

func (e *tagValidationError) Error() string { return e.msg }

// validateTagSet enforces the S3 object-tagging limits and rejects
// duplicate keys, returning the validated key→value map. A nil error
// with an empty map means "no tags" (a valid request that clears the
// tag set).
func validateTagSet(entries []tagEntry) (map[string]string, *tagValidationError) {
	if len(entries) > maxObjectTags {
		return nil, &tagValidationError{http.StatusBadRequest, "BadRequest",
			"Object tags cannot be greater than 10"}
	}
	tags := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Key == "" || len(e.Key) > maxTagKeyLength {
			return nil, &tagValidationError{http.StatusBadRequest, "InvalidTag",
				"The TagKey you have provided is invalid"}
		}
		if len(e.Value) > maxTagValueLength {
			return nil, &tagValidationError{http.StatusBadRequest, "InvalidTag",
				"The TagValue you have provided is invalid"}
		}
		if _, dup := tags[e.Key]; dup {
			return nil, &tagValidationError{http.StatusBadRequest, "InvalidTag",
				"Cannot provide multiple Tags with the same key"}
		}
		tags[e.Key] = e.Value
	}
	return tags, nil
}
