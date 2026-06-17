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

// clearObjectMetadataHeaders strips the object-metadata response headers
// that setObjectMetadataHeaders writes at the GET/HEAD chokepoint, so
// they never linger on an error response emitted by a read sub-handler
// (e.g. a 416 InvalidRange or a 502 BackendGetFailed raised after the
// chokepoint). writeError calls this before rendering the XML error
// body: a stored Content-Encoding (gzip) would make a client try to
// gunzip the plaintext error XML, and a Content-Disposition: attachment
// would make a browser download the error as a file — both hide the
// real error. Content-Type is excluded here because writeError resets
// it to application/xml itself. The header names come from the same
// systemMetadataHeaders table that drives capture/emission (a throwaway
// manifest is used purely to read the names, never field values) so the
// cleared set can never drift from the emitted set.
func clearObjectMetadataHeaders(w http.ResponseWriter) {
	hdr := w.Header()
	for _, hf := range systemMetadataHeaders(&metadata.ObjectManifest{}) {
		if hf.header == "Content-Type" {
			continue
		}
		hdr.Del(hf.header)
	}
	for name := range hdr {
		if strings.HasPrefix(strings.ToLower(name), userMetadataHeaderPrefix) {
			hdr.Del(name)
		}
	}
}

const (
	// metadataDirectiveHeader selects how CopyObject derives the
	// destination's object metadata, mirroring x-amz-tagging-directive.
	metadataDirectiveHeader = "x-amz-metadata-directive"
	// metadataDirectiveCopy preserves the source object's stored metadata
	// (the default when the header is absent), matching AWS.
	metadataDirectiveCopy = "COPY"
	// metadataDirectiveReplace takes the destination metadata from the
	// copy request's own system/x-amz-meta-* headers instead.
	metadataDirectiveReplace = "REPLACE"
)

// validateCopyMetadataDirective checks x-amz-metadata-directive up front in
// CopyObject so a malformed request fails before any bytes move: an unknown
// directive is 400 InvalidArgument, and a REPLACE whose x-amz-meta-* set
// exceeds the size limit is 400 MetadataTooLarge. COPY (the default) needs
// no validation — it carries the already-stored source metadata. The value
// is case-sensitive, matching resolveCopyTags and AWS.
func validateCopyMetadataDirective(hdr http.Header) *metadataValidationError {
	switch hdr.Get(metadataDirectiveHeader) {
	case "", metadataDirectiveCopy:
		return nil
	case metadataDirectiveReplace:
		return validateRequestObjectMetadata(hdr)
	default:
		return &metadataValidationError{http.StatusBadRequest, "InvalidArgument",
			"The x-amz-metadata-directive you provided is invalid"}
	}
}

// applyCopyObjectMetadata sets a CopyObject destination manifest's object
// metadata per x-amz-metadata-directive. The directive MUST already have
// been validated by validateCopyMetadataDirective (Copy guards it up
// front), so this never fails. REPLACE takes the system headers and
// x-amz-meta-* set from the copy request via the same applyRequestObjectMetadata
// the live PUT path uses; COPY (default) preserves the source object's
// stored metadata. The source values are deep-copied (a fresh UserMetadata
// map) so the destination manifest never aliases the source's.
func applyCopyObjectMetadata(m *metadata.ObjectManifest, hdr http.Header, src *metadata.ObjectManifest) {
	if hdr.Get(metadataDirectiveHeader) == metadataDirectiveReplace {
		applyRequestObjectMetadata(m, hdr)
		return
	}
	srcFields := systemMetadataHeaders(src)
	for i, hf := range systemMetadataHeaders(m) {
		*hf.field = *srcFields[i].field
	}
	m.UserMetadata = cloneUserMetadata(src.UserMetadata)
}

// cloneUserMetadata returns a fresh copy of a user-metadata map (nil for an
// empty input) so a copied manifest never shares the source's map.
func cloneUserMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
