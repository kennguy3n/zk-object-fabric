// Package evaluator implements the daily background lifecycle worker
// (WS8.2). One Run pass enumerates every bucket that has a lifecycle
// configuration (across all tenants), then for each bucket:
//
//   - aborts incomplete multipart uploads older than a rule's
//     AbortIncompleteMultipartUpload.DaysAfterInitiation, reclaiming
//     their staged backend parts; and
//   - expires objects whose age/date satisfies an enabled Expiration
//     rule — inserting a delete marker when the bucket is
//     versioning-enabled (so the current version is preserved as a
//     noncurrent version, matching AWS), or permanently deleting the
//     version and its backend pieces otherwise.
//
// The pass is Object-Lock aware: it never permanently deletes a
// version under a legal hold or within its retention window. (Adding a
// delete marker on a versioned bucket is still allowed — an Object Lock
// blocks permanent deletion of a version, not the creation of a marker
// that hides it, matching AWS and the WS8.3 delete path.)
//
// Storage-class Transition rules are validated, persisted, and served
// at full S3 fidelity by the api/s3compat layer, but are NOT acted on
// here: moving object data between tiers reuses the migration tiering
// engine (docs/PROPOSAL.md §4) and lands in a follow-up slice.
//
// The worker is modelled on migration/background_rebalancer: a Config
// of dependencies, a New constructor, and a Run(ctx) method returning
// Stats plus the first fatal error. Per-object and per-upload failures
// are logged and counted but never abort the pass, so a single bad
// object cannot stall the whole sweep.
package evaluator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/lifecycle"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// ConfigSource enumerates the buckets to evaluate and resolves each
// one's versioning state. bucket_config.Store satisfies it.
type ConfigSource interface {
	ListLifecycle(ctx context.Context) ([]bucket_config.LifecycleEntry, error)
	GetVersioning(ctx context.Context, tenantID, bucket string) (bucket_config.VersioningState, error)
}

// Manifests is the subset of manifest_store.ManifestStore the
// evaluator needs. manifest_store.ManifestStore satisfies it.
type Manifests interface {
	List(ctx context.Context, tenantID, bucket, cursor string, limit int) (manifest_store.ListResult, error)
	ListVersions(ctx context.Context, tenantID, bucket, objectKeyHash string) ([]*metadata.ObjectManifest, error)
	Put(ctx context.Context, key manifest_store.ManifestKey, m *metadata.ObjectManifest) error
	Delete(ctx context.Context, key manifest_store.ManifestKey) error
}

// Uploads is the subset of the multipart session store the evaluator
// needs. multipart.Store satisfies it.
type Uploads interface {
	List(tenantID, bucket string) []*multipart.Upload
	Abort(uploadID, tenantID string) (*multipart.Upload, []multipart.Part, error)
}

// Config wires the evaluator's dependencies.
type Config struct {
	// Source enumerates configured buckets and resolves versioning.
	// Required.
	Source ConfigSource

	// Manifests is the control-plane manifest store. Required.
	Manifests Manifests

	// Uploads is the multipart-upload session store. Optional; when
	// nil the AbortIncompleteMultipartUpload action is skipped.
	Uploads Uploads

	// Providers is the backend registry, used to delete the pieces of
	// permanently-expired objects and the staged parts of aborted
	// uploads. Optional; when nil, backend cleanup is skipped (the
	// orphan GC reclaims the bytes eventually) but manifests/uploads
	// are still removed.
	Providers map[string]providers.StorageProvider

	// NewVersionID mints the version ID for a delete marker inserted
	// when expiring an object in a versioning-enabled bucket. The
	// gateway wires its own piece-ID minter so marker IDs are
	// indistinguishable from interactively-created ones. When nil a
	// built-in time-ordered fallback is used.
	NewVersionID func(tenantID, bucket, objectKey string, now time.Time) string

	// PageSize is the manifest List page size. Zero defaults to 500.
	PageSize int

	// Clock returns the current time. Nil defaults to time.Now. Tests
	// override it to make age comparisons deterministic.
	Clock func() time.Time

	// Logger receives per-object / per-upload outcomes. Nil disables
	// logging.
	Logger *log.Logger
}

// Evaluator owns a single lifecycle-sweep workflow.
type Evaluator struct {
	cfg Config
}

// New constructs an Evaluator, filling in defaults. It does not touch
// any backend — that happens on the first Run.
func New(cfg Config) *Evaluator {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 500
	}
	if cfg.NewVersionID == nil {
		cfg.NewVersionID = func(_, _, _ string, now time.Time) string {
			return fmt.Sprintf("lc-%d", now.UTC().UnixNano())
		}
	}
	return &Evaluator{cfg: cfg}
}

// Stats summarises a single Run pass.
type Stats struct {
	BucketsScanned       int
	ObjectsScanned       int
	ObjectsExpired       int // permanently deleted (unversioned bucket)
	DeleteMarkersCreated int // versioning-enabled expirations
	DeleteMarkersRemoved int // ExpiredObjectDeleteMarker cleanups
	UploadsAborted       int
	Skipped              int // matched but protected by Object Lock
	Errors               int
}

// Run executes one full pass over every configured bucket. It returns
// aggregate Stats and the first fatal error (a failure to enumerate
// buckets). Per-bucket and per-object errors are logged and counted in
// Stats.Errors but do not abort the pass.
func (e *Evaluator) Run(ctx context.Context) (Stats, error) {
	var stats Stats
	if e.cfg.Source == nil || e.cfg.Manifests == nil {
		return stats, errors.New("lifecycle/evaluator: Source and Manifests are required")
	}
	entries, err := e.cfg.Source.ListLifecycle(ctx)
	if err != nil {
		return stats, fmt.Errorf("lifecycle/evaluator: list lifecycle configs: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		stats.BucketsScanned++
		e.evalBucket(ctx, entry, &stats)
	}
	return stats, nil
}

func (e *Evaluator) evalBucket(ctx context.Context, entry bucket_config.LifecycleEntry, stats *Stats) {
	now := e.cfg.Clock().UTC()
	e.abortStaleUploads(ctx, entry, now, stats)
	e.expireObjects(ctx, entry, now, stats)
}

// abortStaleUploads aborts incomplete multipart uploads older than the
// cutoff implied by a matching AbortIncompleteMultipartUpload rule.
func (e *Evaluator) abortStaleUploads(ctx context.Context, entry bucket_config.LifecycleEntry, now time.Time, stats *Stats) {
	if e.cfg.Uploads == nil {
		return
	}
	for _, u := range e.cfg.Uploads.List(entry.TenantID, entry.Bucket) {
		if err := ctx.Err(); err != nil {
			return
		}
		rule, ok := matchingAbortRule(entry.Config, u, now)
		if !ok {
			continue
		}
		cutoff, _ := rule.AbortStaleBefore(now)
		if u.CreatedAt.IsZero() || u.CreatedAt.After(cutoff) {
			continue
		}
		_, parts, err := e.cfg.Uploads.Abort(u.ID, u.TenantID)
		if err != nil {
			if errors.Is(err, multipart.ErrNotFound) {
				continue // already gone — concurrent completion/abort
			}
			stats.Errors++
			e.logf("lifecycle/evaluator: abort upload %s (%s/%s): %v", u.ID, entry.Bucket, u.ObjectKey, err)
			continue
		}
		e.deletePieces(ctx, partsBackends(parts))
		stats.UploadsAborted++
	}
}

// matchingAbortRule returns the first enabled rule whose
// AbortIncompleteMultipartUpload action applies to the upload. A
// multipart upload carries no object tags or final size, so only the
// rule's key prefix is considered (tag filters are rejected at
// PutBucketLifecycleConfiguration time; size filters cannot be
// evaluated against an in-flight upload and are ignored here).
func matchingAbortRule(cfg lifecycle.Config, u *multipart.Upload, now time.Time) (lifecycle.Rule, bool) {
	for _, r := range cfg.Rules {
		if !r.Enabled() || r.AbortIncompleteMultipartUpload == nil {
			continue
		}
		if _, ok := r.AbortStaleBefore(now); !ok {
			continue
		}
		if !prefixMatches(r.Filter.Prefix, u.ObjectKey) {
			continue
		}
		return r, true
	}
	return lifecycle.Rule{}, false
}

func prefixMatches(prefix, key string) bool {
	return prefix == "" || len(key) >= len(prefix) && key[:len(prefix)] == prefix
}

// expireObjects walks every latest-version manifest in the bucket and
// applies the first matching enabled Expiration rule.
func (e *Evaluator) expireObjects(ctx context.Context, entry bucket_config.LifecycleEntry, now time.Time, stats *Stats) {
	// Resolve versioning once per bucket; it determines whether an
	// expiration permanently deletes or inserts a delete marker.
	versioned := false
	if state, err := e.cfg.Source.GetVersioning(ctx, entry.TenantID, entry.Bucket); err != nil {
		stats.Errors++
		e.logf("lifecycle/evaluator: versioning lookup %s/%s: %v", entry.TenantID, entry.Bucket, err)
		return
	} else {
		versioned = state == bucket_config.VersioningEnabled
	}

	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		page, err := e.cfg.Manifests.List(ctx, entry.TenantID, entry.Bucket, cursor, e.cfg.PageSize)
		if err != nil {
			stats.Errors++
			e.logf("lifecycle/evaluator: list manifests %s/%s: %v", entry.TenantID, entry.Bucket, err)
			return
		}
		for _, m := range page.Manifests {
			stats.ObjectsScanned++
			e.expireOne(ctx, entry, m, versioned, now, stats)
		}
		if page.NextCursor == "" {
			return
		}
		cursor = page.NextCursor
	}
}

func (e *Evaluator) expireOne(ctx context.Context, entry bucket_config.LifecycleEntry, m *metadata.ObjectManifest, versioned bool, now time.Time, stats *Stats) {
	for _, r := range entry.Config.Rules {
		if !r.Enabled() || r.Expiration == nil {
			continue
		}
		if !r.Matches(m.ObjectKey, m.Tags, m.ObjectSize) {
			continue
		}

		// ExpiredObjectDeleteMarker: remove a delete marker that is the
		// only remaining version of its key (nothing behind it). It is
		// not driven by object age.
		if r.Expiration.ExpiredObjectDeleteMarker {
			if !m.DeleteMarker {
				continue
			}
			if e.removeExpiredDeleteMarker(ctx, entry, m, stats) {
				return
			}
			continue
		}

		expiresAt, ok := r.ExpiresAt(m.CreatedAt)
		if !ok || now.Before(expiresAt) {
			continue
		}
		// A delete marker is never expired by an age/date rule — only
		// by ExpiredObjectDeleteMarker, handled above.
		if m.DeleteMarker {
			continue
		}

		if versioned {
			e.insertDeleteMarker(ctx, entry, m, now, stats)
			return
		}
		// Unversioned: permanent delete, guarded by Object Lock.
		if objectLocked(m, now) {
			stats.Skipped++
			return
		}
		e.permanentlyDelete(ctx, entry, m, stats)
		return
	}
}

// removeExpiredDeleteMarker deletes m (a delete marker) when it is the
// sole remaining version of its key. Returns true when the marker was
// removed.
func (e *Evaluator) removeExpiredDeleteMarker(ctx context.Context, entry bucket_config.LifecycleEntry, m *metadata.ObjectManifest, stats *Stats) bool {
	versions, err := e.cfg.Manifests.ListVersions(ctx, entry.TenantID, entry.Bucket, m.ObjectKeyHash)
	if err != nil {
		stats.Errors++
		e.logf("lifecycle/evaluator: list versions %s/%s: %v", entry.Bucket, m.ObjectKeyHash, err)
		return false
	}
	if len(versions) != 1 {
		return false // non-current versions remain behind the marker
	}
	key := manifest_store.ManifestKey{
		TenantID:      entry.TenantID,
		Bucket:        entry.Bucket,
		ObjectKeyHash: m.ObjectKeyHash,
		VersionID:     m.VersionID,
	}
	if err := e.cfg.Manifests.Delete(ctx, key); err != nil && !errors.Is(err, manifest_store.ErrNotFound) {
		stats.Errors++
		e.logf("lifecycle/evaluator: delete marker %s/%s: %v", entry.Bucket, m.ObjectKeyHash, err)
		return false
	}
	stats.DeleteMarkersRemoved++
	return true
}

// insertDeleteMarker adds a delete marker as the new latest version,
// preserving the expired version as a noncurrent version (AWS
// versioning-enabled expiration semantics).
func (e *Evaluator) insertDeleteMarker(ctx context.Context, entry bucket_config.LifecycleEntry, m *metadata.ObjectManifest, now time.Time, stats *Stats) {
	markerID := e.cfg.NewVersionID(entry.TenantID, entry.Bucket, m.ObjectKey, now)
	marker := &metadata.ObjectManifest{
		TenantID:      entry.TenantID,
		Bucket:        entry.Bucket,
		ObjectKey:     m.ObjectKey,
		ObjectKeyHash: m.ObjectKeyHash,
		VersionID:     markerID,
		DeleteMarker:  true,
		CreatedAt:     now,
	}
	key := manifest_store.ManifestKey{
		TenantID:      entry.TenantID,
		Bucket:        entry.Bucket,
		ObjectKeyHash: m.ObjectKeyHash,
		VersionID:     markerID,
	}
	if err := e.cfg.Manifests.Put(ctx, key, marker); err != nil {
		stats.Errors++
		e.logf("lifecycle/evaluator: insert delete marker %s/%s: %v", entry.Bucket, m.ObjectKey, err)
		return
	}
	stats.DeleteMarkersCreated++
}

// permanentlyDelete removes the manifest version and best-effort
// deletes its backend pieces.
func (e *Evaluator) permanentlyDelete(ctx context.Context, entry bucket_config.LifecycleEntry, m *metadata.ObjectManifest, stats *Stats) {
	key := manifest_store.ManifestKey{
		TenantID:      entry.TenantID,
		Bucket:        entry.Bucket,
		ObjectKeyHash: m.ObjectKeyHash,
		VersionID:     m.VersionID,
	}
	if err := e.cfg.Manifests.Delete(ctx, key); err != nil {
		if errors.Is(err, manifest_store.ErrNotFound) {
			return // already gone
		}
		stats.Errors++
		e.logf("lifecycle/evaluator: delete object %s/%s: %v", entry.Bucket, m.ObjectKey, err)
		return
	}
	e.deletePieces(ctx, pieceBackends(m.Pieces))
	stats.ObjectsExpired++
}

// objectLocked reports whether the version is protected from permanent
// deletion by an Object Lock legal hold or an unexpired retention.
func objectLocked(m *metadata.ObjectManifest, now time.Time) bool {
	if m.LegalHold {
		return true
	}
	return !m.RetainUntil.IsZero() && m.RetainUntil.After(now)
}

// backendPiece pairs a backend name with the piece ID stored there.
type backendPiece struct {
	backend string
	pieceID string
}

func pieceBackends(pieces []metadata.Piece) []backendPiece {
	out := make([]backendPiece, 0, len(pieces))
	for _, p := range pieces {
		if p.PieceID == "" {
			continue
		}
		out = append(out, backendPiece{backend: p.Backend, pieceID: p.PieceID})
	}
	return out
}

func partsBackends(parts []multipart.Part) []backendPiece {
	out := make([]backendPiece, 0, len(parts))
	for _, p := range parts {
		if p.PieceID == "" {
			continue
		}
		out = append(out, backendPiece{backend: p.Backend, pieceID: p.PieceID})
	}
	return out
}

// deletePieces best-effort removes backend pieces. Failures are
// logged, not counted as pass errors: the orphan GC reclaims any piece
// left behind, exactly as the interactive Abort/overwrite paths rely
// on.
func (e *Evaluator) deletePieces(ctx context.Context, pieces []backendPiece) {
	if e.cfg.Providers == nil {
		return
	}
	for _, p := range pieces {
		provider, ok := e.cfg.Providers[p.backend]
		if !ok {
			continue
		}
		if err := provider.DeletePiece(ctx, p.pieceID); err != nil {
			e.logf("lifecycle/evaluator: delete piece %s on %s: %v", p.pieceID, p.backend, err)
		}
	}
}

func (e *Evaluator) logf(format string, args ...any) {
	if e.cfg.Logger != nil {
		e.cfg.Logger.Printf(format, args...)
	}
}
