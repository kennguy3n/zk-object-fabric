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
	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
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

// ContentIndex is the subset of content_index.Store the evaluator
// needs to reclaim the bytes of a permanently-expired deduped object.
// content_index.Store satisfies it. The contract mirrors the
// interactive DELETE path: DecrementRef returns the post-decrement
// count, and the caller must attempt Delete (which only succeeds at
// RefCount==0) BEFORE removing the backend piece.
type ContentIndex interface {
	DecrementRef(ctx context.Context, tenantID, contentHash string) (newCount int, err error)
	Delete(ctx context.Context, tenantID, contentHash string) error
}

// AuditRecorder receives one entry per lifecycle action (expiration,
// delete marker, delete-marker cleanup, multipart abort) for the
// compliance audit trail. internal/compliance.AuditStore satisfies it
// via cmd/gateway's thin adapter — the same store the interactive
// PUT/GET/DELETE path writes to. Defined as a minimal local interface
// (with a shape-compatible AuditEntry) so the background worker does
// not import internal/compliance. Optional; nil disables auditing.
type AuditRecorder interface {
	Record(ctx context.Context, entry AuditEntry) error
}

// AuditEntry mirrors compliance.AuditEntry (and s3compat.AuditEntry)
// so cmd/gateway can forward evaluator entries to the same audit store
// with a trivial adapter. RequestID is always empty for lifecycle
// actions: the sweep runs on an internal goroutine with no inbound
// *http.Request, which the audit consumer already treats as "no id".
type AuditEntry struct {
	TenantID       string
	Operation      string
	Bucket         string
	ObjectKey      string
	PieceID        string
	PieceBackend   string
	BackendCountry string
	Timestamp      time.Time
	RequestID      string
}

// Lifecycle audit operation labels. They are intentionally distinct
// from the interactive path's HTTP-verb operations ("PUT"/"DELETE") so
// an auditor can tell a system-driven lifecycle action apart from a
// user-issued API call.
const (
	auditOpExpiration     = "LifecycleExpiration"          // permanent delete (unversioned bucket)
	auditOpDeleteMarker   = "LifecycleDeleteMarker"        // marker inserted (versioned bucket)
	auditOpDeleteMarkerGC = "LifecycleExpiredDeleteMarker" // ExpiredObjectDeleteMarker cleanup
	auditOpAbortMultipart = "LifecycleAbortMultipartUpload"
)

// BillingSink receives usage events for lifecycle actions. billing
// sinks (LoggerSink, ClickHouseSink, …) satisfy it. Optional; nil
// disables metering. Lifecycle actions are metered on dedicated
// dimensions (billing.Lifecycle*) rather than the Tier-1
// Put/Delete-request dimensions because AWS does not bill lifecycle
// expirations as API requests.
type BillingSink interface {
	Emit(event billing.UsageEvent)
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

	// ContentIndex is the intra-tenant dedup refcount store. Optional;
	// when nil the evaluator deletes a permanently-expired object's
	// backend pieces directly (correct for non-deduped manifests).
	// When wired, a manifest carrying a ContentHash decrements the
	// shared per-(tenant, content_hash) refcount and the canonical
	// piece is removed only on the final reference — mirroring the
	// interactive DELETE path so an expiration can never delete a
	// piece still shared by another object or orphan the index row.
	ContentIndex ContentIndex

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

	// Audit, when non-nil, records one entry per lifecycle action
	// (expiration, delete marker, delete-marker cleanup, multipart
	// abort) so lifecycle-driven mutations land in the same
	// compliance trail as interactive PUT/DELETE. Optional.
	Audit AuditRecorder

	// Billing, when non-nil, receives a usage event per lifecycle
	// action on a dedicated billing.Lifecycle* dimension (these are
	// deliberately not folded into the Tier-1 request dimensions —
	// AWS does not bill lifecycle expirations as API requests).
	// Optional.
	Billing BillingSink

	// NodeID identifies the worker node emitting billing events. It
	// is copied into each UsageEvent.SourceNodeID. Optional.
	NodeID string

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
		backends := partsBackends(parts)
		e.deletePieces(ctx, backends)
		stats.UploadsAborted++
		e.audit(ctx, auditOpAbortMultipart, entry.TenantID, entry.Bucket, u.ObjectKey, firstBackendPiece(backends), now)
		e.emit(entry.TenantID, entry.Bucket, billing.LifecycleAbortedUploads, 1, now)
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
			if e.removeExpiredDeleteMarker(ctx, entry, m, now, stats) {
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
		e.permanentlyDelete(ctx, entry, m, now, stats)
		return
	}
}

// removeExpiredDeleteMarker deletes m (a delete marker) when it is the
// sole remaining version of its key. Returns true when the marker was
// removed.
func (e *Evaluator) removeExpiredDeleteMarker(ctx context.Context, entry bucket_config.LifecycleEntry, m *metadata.ObjectManifest, now time.Time, stats *Stats) bool {
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
	e.audit(ctx, auditOpDeleteMarkerGC, entry.TenantID, entry.Bucket, m.ObjectKey, backendPiece{}, now)
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
	e.audit(ctx, auditOpDeleteMarker, entry.TenantID, entry.Bucket, m.ObjectKey, backendPiece{}, now)
	e.emit(entry.TenantID, entry.Bucket, billing.LifecycleDeleteMarkers, 1, now)
}

// permanentlyDelete removes the manifest version and best-effort
// deletes its backend pieces.
func (e *Evaluator) permanentlyDelete(ctx context.Context, entry bucket_config.LifecycleEntry, m *metadata.ObjectManifest, now time.Time, stats *Stats) {
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
	e.reclaimPieces(ctx, entry.TenantID, entry.Bucket, m, now)
	stats.ObjectsExpired++
	// Audit only a piece-backed expiration, matching the interactive
	// DELETE path (api/s3compat/handler.go), whose audit entry is
	// anchored on manifest.Pieces[0]. A piece-less manifest (e.g. a
	// zero-byte object) is still metered below; the compliance trail
	// records piece-backed deletions, mirroring the user-issued path
	// exactly.
	if len(m.Pieces) > 0 {
		e.audit(ctx, auditOpExpiration, entry.TenantID, entry.Bucket, m.ObjectKey, firstBackendPiece(pieceBackends(m.Pieces)), now)
	}
	e.emit(entry.TenantID, entry.Bucket, billing.LifecycleExpirations, 1, now)
}

// reclaimPieces removes the backend bytes backing a just-deleted
// manifest. A deduped manifest (non-empty ContentHash) whose
// ContentIndex is wired decrements the shared per-(tenant,
// content_hash) refcount and removes the canonical piece only on the
// final reference, dropping the index row FIRST and deleting the
// piece only on a successful conditional Delete — closing the race
// with a concurrent dedup PUT that re-increments the count between
// our DecrementRef returning 0 and the piece deletion. This mirrors
// the interactive DELETE path (api/s3compat/handler.go) so a
// lifecycle expiration can never delete a piece another object still
// references or leave a dangling index row. Non-deduped manifests, or
// a nil ContentIndex, fall back to a direct best-effort piece delete.
func (e *Evaluator) reclaimPieces(ctx context.Context, tenantID, bucket string, m *metadata.ObjectManifest, now time.Time) {
	if m.ContentHash == "" || e.cfg.ContentIndex == nil {
		e.deletePieces(ctx, pieceBackends(m.Pieces))
		return
	}
	newCount, err := e.cfg.ContentIndex.DecrementRef(ctx, tenantID, m.ContentHash)
	switch {
	case errors.Is(err, content_index.ErrNotFound):
		// The index row is already gone but the manifest still
		// pointed at it — best-effort cleanup of the pieces.
		e.deletePieces(ctx, pieceBackends(m.Pieces))
	case err != nil:
		e.logf("lifecycle/evaluator: decrement refcount %s/%s: %v", tenantID, m.ContentHash, err)
	case newCount == 0:
		switch delErr := e.cfg.ContentIndex.Delete(ctx, tenantID, m.ContentHash); {
		case delErr == nil:
			e.deletePieces(ctx, pieceBackends(m.Pieces))
		case errors.Is(delErr, content_index.ErrNotFound),
			errors.Is(delErr, content_index.ErrRefCountNonZero):
			// Row vanished (e.g. orphan GC) or a concurrent uploader
			// re-bumped the count: the piece may still be referenced,
			// so leave it for that owner / the GC.
		default:
			e.logf("lifecycle/evaluator: delete index row %s/%s: %v", tenantID, m.ContentHash, delErr)
		}
	default:
		// newCount > 0: the piece is still referenced by another
		// manifest in this tenant. Leave it on the backend, and emit
		// the post-decrement refcount sample so the billing pipeline
		// can track hot content — exactly as the interactive DELETE
		// path does.
		e.emit(tenantID, bucket, billing.DedupRefCount, uint64(newCount), now)
	}
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

// firstBackendPiece returns the first piece in the slice (the
// representative recorded in the audit entry, matching the interactive
// DELETE path which audits manifest.Pieces[0]). The zero value carries
// empty backend/pieceID for actions with no backing piece (delete
// markers).
func firstBackendPiece(ps []backendPiece) backendPiece {
	if len(ps) > 0 {
		return ps[0]
	}
	return backendPiece{}
}

// audit records one lifecycle action to the compliance trail. It
// resolves the piece's backend placement country the same way the
// interactive DELETE path does. A nil Audit recorder is a no-op.
func (e *Evaluator) audit(ctx context.Context, op, tenantID, bucket, objectKey string, bp backendPiece, now time.Time) {
	if e.cfg.Audit == nil {
		return
	}
	var country string
	if bp.backend != "" {
		if prov, ok := e.cfg.Providers[bp.backend]; ok {
			country = prov.PlacementLabels().Country
		}
	}
	// A failed compliance recording is surfaced via the logger (like
	// every other non-fatal error in the sweep) rather than silently
	// dropped — an operator must be able to tell that a lifecycle
	// action ran but its audit entry was lost. The sweep still
	// proceeds; the action itself already succeeded.
	if err := e.cfg.Audit.Record(ctx, AuditEntry{
		TenantID:       tenantID,
		Operation:      op,
		Bucket:         bucket,
		ObjectKey:      objectKey,
		PieceID:        bp.pieceID,
		PieceBackend:   bp.backend,
		BackendCountry: country,
		Timestamp:      now,
	}); err != nil {
		e.logf("lifecycle/evaluator: audit %s %s/%s: %v", op, bucket, objectKey, err)
	}
}

// emit records one lifecycle usage event. A nil Billing sink is a
// no-op.
func (e *Evaluator) emit(tenantID, bucket string, dim billing.Dimension, delta uint64, now time.Time) {
	if e.cfg.Billing == nil {
		return
	}
	e.cfg.Billing.Emit(billing.UsageEvent{
		TenantID:     tenantID,
		Bucket:       bucket,
		Dimension:    dim,
		Delta:        delta,
		ObservedAt:   now,
		SourceNodeID: e.cfg.NodeID,
	})
}

func (e *Evaluator) logf(format string, args ...any) {
	if e.cfg.Logger != nil {
		e.cfg.Logger.Printf(format, args...)
	}
}
