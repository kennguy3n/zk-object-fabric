// Package bucket_config is the control-plane store for per-bucket S3
// configuration sub-resources: bucket versioning state, Object Lock,
// CORS, lifecycle, event notifications, and default encryption (SSE)
// all persist through the same store rather than one table per
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

import (
	"context"

	"github.com/kennguy3n/zk-object-fabric/metadata/cors"
	"github.com/kennguy3n/zk-object-fabric/metadata/lifecycle"
	"github.com/kennguy3n/zk-object-fabric/metadata/notification"
	"github.com/kennguy3n/zk-object-fabric/metadata/object_lock"
	"github.com/kennguy3n/zk-object-fabric/metadata/sse"
)

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

	// GetObjectLock returns the bucket-level S3 Object Lock
	// configuration for (tenantID, bucket). A bucket that was
	// never configured returns the zero object_lock.Config (Enabled
	// false) with a nil error, so callers never have to distinguish
	// "missing" from "no Object Lock".
	GetObjectLock(ctx context.Context, tenantID, bucket string) (object_lock.Config, error)

	// SetObjectLock upserts the bucket-level Object Lock configuration
	// for (tenantID, bucket). cfg must pass cfg.Valid(); enabling
	// Object Lock requires bucket versioning, which the API layer
	// enforces before calling this.
	SetObjectLock(ctx context.Context, tenantID, bucket string, cfg object_lock.Config) error

	// GetCORS returns the bucket CORS configuration for (tenantID,
	// bucket). A bucket with no CORS configuration returns the
	// zero cors.Config (no rules) with a nil error; callers use
	// Config.Empty to distinguish "not configured" (which the S3 API
	// surfaces as 404 NoSuchCORSConfiguration) from a configured rule
	// set.
	GetCORS(ctx context.Context, tenantID, bucket string) (cors.Config, error)

	// SetCORS upserts the bucket CORS configuration for (tenantID,
	// bucket). cfg must pass cfg.Valid().
	SetCORS(ctx context.Context, tenantID, bucket string, cfg cors.Config) error

	// DeleteCORS removes any CORS configuration for (tenantID,
	// bucket). Deleting a bucket that has no CORS configuration is a
	// no-op and returns a nil error, matching S3's idempotent
	// DeleteBucketCors.
	DeleteCORS(ctx context.Context, tenantID, bucket string) error

	// GetLifecycle returns the bucket lifecycle configuration for
	// (tenantID, bucket). A bucket with no lifecycle
	// configuration returns the zero lifecycle.Config (no rules) with
	// a nil error; callers use Config.Empty to distinguish "not
	// configured" (which the S3 API surfaces as 404
	// NoSuchLifecycleConfiguration) from a configured rule set.
	GetLifecycle(ctx context.Context, tenantID, bucket string) (lifecycle.Config, error)

	// SetLifecycle upserts the bucket lifecycle configuration for
	// (tenantID, bucket). cfg must pass cfg.Valid().
	SetLifecycle(ctx context.Context, tenantID, bucket string, cfg lifecycle.Config) error

	// DeleteLifecycle removes any lifecycle configuration for
	// (tenantID, bucket). Deleting a bucket that has none is a no-op
	// and returns a nil error, matching S3's idempotent
	// DeleteBucketLifecycle.
	DeleteLifecycle(ctx context.Context, tenantID, bucket string) error

	// ListLifecycle returns every (tenantID, bucket) that currently
	// has a lifecycle configuration, together with its rules. Unlike
	// every other method on this interface it is NOT tenant-scoped: it
	// exists solely for the background lifecycle evaluator, which has
	// no tenant list to iterate over and must visit every configured
	// bucket across all tenants once per pass. It is never reachable
	// from a tenant-facing request path.
	ListLifecycle(ctx context.Context) ([]LifecycleEntry, error)

	// GetNotification returns the bucket event-notification
	// configuration for (tenantID, bucket). A bucket with no
	// notification configuration returns the zero notification.Config
	// (no rules) with a nil error; callers use Config.Empty to
	// distinguish "not configured" from a configured rule set. S3 has
	// no error for an unconfigured bucket — GetBucketNotification
	// returns an empty document — so there is no separate DeleteCORS
	// equivalent here.
	GetNotification(ctx context.Context, tenantID, bucket string) (notification.Config, error)

	// SetNotification upserts the bucket notification configuration for
	// (tenantID, bucket). cfg must pass cfg.Valid(). An empty cfg is
	// valid and clears any existing configuration, matching S3's
	// PutBucketNotificationConfiguration with an empty body.
	SetNotification(ctx context.Context, tenantID, bucket string, cfg notification.Config) error

	// GetEncryption returns the bucket default SSE configuration for
	// (tenantID, bucket). A bucket with no default-encryption
	// configuration returns the zero sse.Config with a nil error;
	// callers use Config.Empty to distinguish "not configured" (which
	// the S3 API surfaces as 404
	// ServerSideEncryptionConfigurationNotFoundError) from a configured
	// default.
	GetEncryption(ctx context.Context, tenantID, bucket string) (sse.Config, error)

	// SetEncryption upserts the bucket default SSE configuration for
	// (tenantID, bucket). cfg must pass cfg.Valid().
	SetEncryption(ctx context.Context, tenantID, bucket string, cfg sse.Config) error

	// DeleteEncryption removes any default SSE configuration for
	// (tenantID, bucket). Deleting a bucket that has none is a no-op and
	// returns a nil error, matching S3's idempotent
	// DeleteBucketEncryption.
	DeleteEncryption(ctx context.Context, tenantID, bucket string) error
}

// LifecycleEntry is one (tenant, bucket) bucket lifecycle
// configuration returned by Store.ListLifecycle, for the background
// evaluator to act on.
type LifecycleEntry struct {
	TenantID string
	Bucket   string
	Config   lifecycle.Config
}
