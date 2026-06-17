// WS8.1 — S3 object metadata (system HTTP headers + x-amz-meta-*).
//
// S3 lets a client attach metadata to an object at write time and
// receive it back verbatim on every GET/HEAD: the system-defined HTTP
// headers (Content-Type, Content-Encoding, Content-Disposition,
// Content-Language, Cache-Control, Expires) and an arbitrary set of
// user-defined x-amz-meta-* pairs. Like Tags, this metadata lives on
// the object manifest (metadata.ObjectManifest) and rides the opaque
// manifest JSONB body — it is mode-independent (the same for managed,
// client-side, dedup, and erasure-coded objects, where the backend
// provider only ever sees the ciphertext shard's type) and is never
// used for placement. See docs/PROPOSAL.md §15.1.2.
package s3compat

import (
	"net/http"
	"strings"

	"github.com/kennguy3n/zk-object-fabric/metadata"
)

const (
	// userMetadataHeaderPrefix is the S3 prefix for user-defined object
	// metadata headers. Keys are stored on the manifest without it and
	// re-emitted with it on read.
	userMetadataHeaderPrefix = "x-amz-meta-"

	// defaultContentType is what S3 returns for an object stored without
	// an explicit Content-Type, so a browser still gets a MIME type.
	defaultContentType = "binary/octet-stream"

	// maxUserMetadataBytes is the S3 limit on the combined size of all
	// user-defined metadata keys and values (2 KB). Exceeding it fails
	// the PUT with 400 MetadataTooLarge, matching AWS.
	maxUserMetadataBytes = 2 * 1024
)

// systemMetadataHeaders are the system-defined HTTP headers S3 persists
// with an object and replays on read, paired with a pointer to the
// manifest field that stores each one. The slice drives both capture
// (request → manifest) and emission (manifest → response) so the two
// directions can never drift.
func systemMetadataHeaders(m *metadata.ObjectManifest) []struct {
	header string
	field  *string
} {
	return []struct {
		header string
		field  *string
	}{
		{"Content-Type", &m.ContentType},
		{"Content-Encoding", &m.ContentEncoding},
		{"Content-Disposition", &m.ContentDisposition},
		{"Content-Language", &m.ContentLanguage},
		{"Cache-Control", &m.CacheControl},
		{"Expires", &m.Expires},
	}
}

// metadataValidationError carries the S3 error triple for invalid object
// metadata so the caller can render it through writeError.
type metadataValidationError struct {
	code   int
	s3code string
	msg    string
}

func (e *metadataValidationError) Error() string { return e.msg }

// validateRequestObjectMetadata enforces the S3 user-metadata size limit
// up front so an oversized request fails with 400 before any backend
// write. It measures the combined byte length of every x-amz-meta-* key
// (without the prefix) and value, matching how AWS bounds metadata.
func validateRequestObjectMetadata(h http.Header) *metadataValidationError {
	total := 0
	for name, vals := range h {
		key, ok := strings.CutPrefix(strings.ToLower(name), userMetadataHeaderPrefix)
		if !ok || key == "" {
			continue
		}
		total += len(key)
		for _, v := range vals {
			total += len(v)
		}
	}
	if total > maxUserMetadataBytes {
		return &metadataValidationError{http.StatusBadRequest, "MetadataTooLarge",
			"Your metadata headers exceed the maximum allowed metadata size"}
	}
	return nil
}

// collectUserMetadata extracts the x-amz-meta-* headers from a write
// request into a map keyed by the lower-cased name with the prefix
// stripped (HTTP header names are case-insensitive). Repeated values for
// one key are joined with commas, matching AWS. Returns nil when the
// request carries no user metadata so the manifest field stays omitted.
func collectUserMetadata(h http.Header) map[string]string {
	var out map[string]string
	for name, vals := range h {
		if len(vals) == 0 {
			continue
		}
		key, ok := strings.CutPrefix(strings.ToLower(name), userMetadataHeaderPrefix)
		if !ok || key == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(vals))
		}
		out[key] = strings.Join(vals, ",")
	}
	return out
}

// applyRequestObjectMetadata copies the S3 system HTTP metadata headers
// and x-amz-meta-* user metadata from a write request onto the manifest
// being created. Callers MUST have already validated the request (the
// guard at the top of Put), so this never fails. Empty system headers
// leave the corresponding manifest field empty (omitted from the JSONB
// body), and the read path defaults a missing Content-Type.
func applyRequestObjectMetadata(m *metadata.ObjectManifest, h http.Header) {
	for _, hf := range systemMetadataHeaders(m) {
		*hf.field = h.Get(hf.header)
	}
	m.UserMetadata = collectUserMetadata(h)
}

// setObjectMetadataHeaders writes an object's stored metadata onto a
// GET/HEAD response: Content-Type (defaulting to binary/octet-stream),
// each non-empty system header, and every x-amz-meta-* user-metadata
// pair. It MUST run before WriteHeader. Called once at the read
// chokepoint (right after applyReadConditionals) so single-piece,
// erasure-coded, multipart, and gateway-encrypted paths all emit the
// same headers without each path repeating the logic.
func setObjectMetadataHeaders(w http.ResponseWriter, m *metadata.ObjectManifest) {
	contentType := m.ContentType
	if contentType == "" {
		contentType = defaultContentType
	}
	w.Header().Set("Content-Type", contentType)
	for _, hf := range systemMetadataHeaders(m) {
		if hf.header == "Content-Type" {
			continue
		}
		if v := *hf.field; v != "" {
			w.Header().Set(hf.header, v)
		}
	}
	for k, v := range m.UserMetadata {
		w.Header().Set(userMetadataHeaderPrefix+k, v)
	}
}
