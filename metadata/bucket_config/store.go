// Package bucket_config is the control-plane store for per-bucket S3
// configuration sub-resources. Today it persists bucket versioning
// state (WS8.4); future workstreams (CORS, lifecycle, notifications)
// can extend the same store rather than adding one table per
// sub-resource.
//
// The Store interface is the integration boundary the gateway
// consumes. Concrete implementations live in subpackages
// (postgres/, sqlite/) and an in-memory fake lives in this package
// for tests and the dev profile. Implementations are wired through
// dependency injection at gateway startup, mirroring
// metadata/content_index.
//
// Buckets in this gateway are implicit — they spring into existence
// the first time an object is written — so there is no bucket table
// to extend. Versioning state is therefore keyed by
// (tenant_id, bucket) directly. A bucket that was never configured
// reports VersioningUnset, which the S3 API surfaces as an empty
// <VersioningConfiguration/> document.
package bucket_config

import "context"

// VersioningState is the S3 bucket-versioning status. The zero value
// (VersioningUnset) means the bucket has never had versioning
// configured, which AWS reports as a versioning configuration with no
// <Status> element.
type VersioningState string

const (
	// VersioningUnset is the default for a bucket that has never
	// had PutBucketVersioning called on it.
	VersioningUnset VersioningState = ""
	// VersioningEnabled means new object writes create versions and
	// DeleteObject (without a versionId) inserts a delete marker
	// instead of removing data.
	VersioningEnabled VersioningState = "Enabled"
	// VersioningSuspended means versioning was enabled at some point
	// and then turned off. Pre-existing versions are retained, but
	// new writes and deletes behave like the unversioned case.
	VersioningSuspended VersioningState = "Suspended"
)

// Valid reports whether v is a state a client may set via
// PutBucketVersioning. VersioningUnset is the never-configured zero
// value and is intentionally NOT settable — S3 has no way to express
// "un-configure" a bucket, only Enabled/Suspended.
func (v VersioningState) Valid() bool {
	switch v {
	case VersioningEnabled, VersioningSuspended:
		return true
	default:
		return false
	}
}

// Store persists per-bucket configuration. All methods are scoped by
// tenant_id; cross-tenant reads are never possible through this
// interface.
type Store interface {
	// GetVersioning returns the versioning state for (tenantID,
	// bucket). A bucket that was never configured returns
	// VersioningUnset with a nil error — callers never have to
	// distinguish "missing" from "unset".
	GetVersioning(ctx context.Context, tenantID, bucket string) (VersioningState, error)

	// SetVersioning upserts the versioning state for (tenantID,
	// bucket). state must be Valid(); implementations reject
	// VersioningUnset.
	SetVersioning(ctx context.Context, tenantID, bucket string, state VersioningState) error
}
