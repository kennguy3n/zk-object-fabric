package s3compat

import (
	"net/http"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/metadata"
)

// captureMultipartObjectMetadata builds the multipart session's stored
// tag set and S3 object metadata from the CreateMultipartUpload request
// headers, reusing the exact normalisation of the single-piece PUT path
// (applyRequestObjectMetadata + requestObjectTags). AWS attaches
// x-amz-tagging and the system / user-metadata headers at create time
// and stamps them on the object CompleteMultipartUpload produces, so the
// gateway captures them here and persists them on the upload session.
//
// The caller MUST have validated the headers up front (parseObjectTaggingHeader
// + validateRequestObjectMetadata) so this never yields a partial set: a
// header that somehow fails re-validation is dropped rather than applied
// half-formed, matching requestObjectTags.
func captureMultipartObjectMetadata(h http.Header) multipart.ObjectMetadata {
	var tmp metadata.ObjectManifest
	applyRequestObjectMetadata(&tmp, h)
	return multipart.ObjectMetadata{
		Tags:               requestObjectTags(h.Get("x-amz-tagging")),
		ContentType:        tmp.ContentType,
		ContentEncoding:    tmp.ContentEncoding,
		ContentDisposition: tmp.ContentDisposition,
		ContentLanguage:    tmp.ContentLanguage,
		CacheControl:       tmp.CacheControl,
		Expires:            tmp.Expires,
		UserMetadata:       tmp.UserMetadata,
	}
}

// applyMultipartObjectMetadata stamps the tags + object metadata captured
// at CreateMultipartUpload onto the completed object's manifest. It runs
// before the dedup branches so a dedup-redirected manifest keeps them
// too — tags and metadata are object-level, independent of where the
// bytes physically land.
func applyMultipartObjectMetadata(m *metadata.ObjectManifest, md multipart.ObjectMetadata) {
	m.Tags = md.Tags
	m.ContentType = md.ContentType
	m.ContentEncoding = md.ContentEncoding
	m.ContentDisposition = md.ContentDisposition
	m.ContentLanguage = md.ContentLanguage
	m.CacheControl = md.CacheControl
	m.Expires = md.Expires
	m.UserMetadata = md.UserMetadata
}
