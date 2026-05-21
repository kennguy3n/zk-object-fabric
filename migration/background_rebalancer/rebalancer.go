// Package background_rebalancer implements the batch worker that
// sweeps the manifest store during a cloud→local (or any backend→
// backend) migration and copies pieces from the old backend to the
// new primary. See docs/PROPOSAL.md §4.3 and migration/state.go.
//
// The rebalancer is idempotent: replaying it after a crash re-scans
// the same manifests and copies only the pieces still missing on
// the destination. It respects a configurable bandwidth ceiling so
// background traffic does not starve interactive request paths.
package background_rebalancer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/pieceintegrity"
	"github.com/kennguy3n/zk-object-fabric/migration"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// IntegrityFailureSink is the optional metrics surface the
// rebalancer uses to report a streamed-piece content-hash
// mismatch. The contract mirrors api/s3compat's sink so the same
// internal/metrics adapter can drive both the cache-miss GET
// path and the rebalance path with one type. A nil sink in
// Config disables the counters but keeps the structured log
// line, which is enough for operators in dev / test contexts.
//
// Inc fires on a verified content mismatch (cf.
// pieceintegrity.ErrIntegrityCheckFailed) — i.e. the recorded
// hash and the streamed bytes disagree. Operators page on this:
// either the source backend's bytes have rotted / been tampered,
// or the manifest recorded the wrong hash on PUT. Either way the
// rebalancer refuses to leave the bad bytes on the destination
// (it deletes them) and aborts the manifest.
//
// IncUnrecognized fires when piece.Hash is non-empty but does
// not parse as blake3:<hex> or as a 64-char SHA-256 hex (cf.
// pieceintegrity.ErrIntegrityClaimUnrecognized). This is the
// legacy-manifest path: copy/dedup/multipart manifests written
// before the BLAKE3 cut-over stamped a backend ETag into Hash.
// The bytes are not known to be wrong; the rebalancer leaves
// them in place and surfaces the counter so operators can plan
// a one-shot rewrite migration that moves the legacy ETag into
// ProviderETag and clears Hash.
type IntegrityFailureSink interface {
	Inc(backend string)
	IncUnrecognized(backend string)
}

// TenantTarget names a single migration target inside the
// rebalancer's scan set. A manifest is eligible for rebalance when
// its (TenantID, Bucket) tuple matches and its MigrationState sits
// in one of the transient phases (DualWrite,
// LocalPrimaryWasabiBackup, LocalPrimaryWasabiDrain).
type TenantTarget struct {
	TenantID       string
	Bucket         string
	SourceBackend  string
	PrimaryBackend string
}

// Config captures the rebalancer's tuning knobs.
type Config struct {
	// Manifests is the control-plane manifest store. Required.
	Manifests manifest_store.ManifestStore

	// Providers is the backend registry. Required.
	Providers map[string]providers.StorageProvider

	// Targets lists the (tenant, bucket) pairs to rebalance. A
	// single pass of Run() iterates every target.
	Targets []TenantTarget

	// BytesPerSecond caps the steady-state copy bandwidth. Zero
	// means no cap. The cap is enforced inline per Read on the
	// streaming copy path (see throttledReader) so a slow rate
	// does not force the gateway to buffer the entire piece in
	// memory before dispatching the destination PUT.
	BytesPerSecond int64

	// PageSize is the ManifestStore list page size. Zero defaults
	// to 500.
	PageSize int

	// Clock, if set, returns the current time. Tests override it
	// to make the bandwidth throttle deterministic.
	Clock func() time.Time

	// Logger receives per-piece outcomes. Nil disables logging.
	Logger *log.Logger

	// IntegrityFailures, if set, receives metric increments when a
	// streamed piece's recomputed content hash does not match the
	// manifest's recorded Hash (or when the recorded hash format
	// is unrecognised). The rebalancer also logs the event
	// structurally either way; the sink is optional so tests that
	// do not wire a metrics registry still build.
	IntegrityFailures IntegrityFailureSink
}

// Rebalancer owns a single migration workflow. Its Run method walks
// every configured target, copies outstanding pieces, and advances
// each manifest's migration phase when all pieces are on the new
// primary.
type Rebalancer struct {
	cfg Config
}

// New constructs a Rebalancer. It does not validate backend
// availability — that check fires on the first Run.
func New(cfg Config) *Rebalancer {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 500
	}
	return &Rebalancer{cfg: cfg}
}

// Stats summarises a single Run pass.
type Stats struct {
	ManifestsScanned int
	PiecesCopied     int
	BytesCopied      int64
	PhasesAdvanced   int
	Errors           int
}

// Run executes one full pass over every target. It returns an
// aggregate Stats and the first fatal error (if any). Per-manifest
// errors are logged and counted but do not abort the pass so the
// worker can make progress across the rest of the scan.
func (r *Rebalancer) Run(ctx context.Context) (Stats, error) {
	if r.cfg.Manifests == nil || r.cfg.Providers == nil {
		return Stats{}, errors.New("background_rebalancer: manifests and providers are required")
	}
	var stats Stats
	for _, target := range r.cfg.Targets {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		targetStats, err := r.runTarget(ctx, target)
		stats.ManifestsScanned += targetStats.ManifestsScanned
		stats.PiecesCopied += targetStats.PiecesCopied
		stats.BytesCopied += targetStats.BytesCopied
		stats.PhasesAdvanced += targetStats.PhasesAdvanced
		stats.Errors += targetStats.Errors
		if err != nil {
			return stats, err
		}
	}
	return stats, nil
}

func (r *Rebalancer) runTarget(ctx context.Context, target TenantTarget) (Stats, error) {
	var stats Stats
	source, ok := r.cfg.Providers[target.SourceBackend]
	if !ok {
		return stats, fmt.Errorf("background_rebalancer: source backend %q not registered", target.SourceBackend)
	}
	primary, ok := r.cfg.Providers[target.PrimaryBackend]
	if !ok {
		return stats, fmt.Errorf("background_rebalancer: primary backend %q not registered", target.PrimaryBackend)
	}

	cursor := ""
	for {
		page, err := r.cfg.Manifests.List(ctx, target.TenantID, target.Bucket, cursor, r.cfg.PageSize)
		if err != nil {
			return stats, fmt.Errorf("background_rebalancer: list manifests: %w", err)
		}
		for _, m := range page.Manifests {
			stats.ManifestsScanned++
			if !eligible(m) {
				continue
			}
			copied, bytes, err := r.rebalanceManifest(ctx, m, target, source, primary)
			stats.PiecesCopied += copied
			stats.BytesCopied += bytes
			if err != nil {
				stats.Errors++
				r.logf("background_rebalancer: rebalance %s/%s: %v", m.Bucket, m.ObjectKeyHash, err)
				continue
			}
			advanced, err := r.advancePhase(ctx, m, target)
			if err != nil {
				stats.Errors++
				r.logf("background_rebalancer: advance phase for %s/%s: %v", m.Bucket, m.ObjectKeyHash, err)
				continue
			}
			if advanced {
				stats.PhasesAdvanced++
			}
		}
		if page.NextCursor == "" {
			return stats, nil
		}
		cursor = page.NextCursor
	}
}

// eligible reports whether the manifest's MigrationState sits in a
// phase the rebalancer should touch.
func eligible(m *metadata.ObjectManifest) bool {
	switch migration.MigrationPhase(phaseOf(m)) {
	case migration.DualWrite, migration.LocalPrimaryWasabiBackup, migration.LocalPrimaryWasabiDrain:
		return true
	default:
		return false
	}
}

// phaseOf infers the manifest's MigrationPhase. Phase 2 stores only
// the PrimaryBackend and a Generation counter on the manifest, not
// the phase name directly; the helper defaults to WasabiPrimary for
// Generation 0/1 and uses the presence of the CloudCopy field to
// decide between DualWrite and LocalPrimaryWasabiBackup.
func phaseOf(m *metadata.ObjectManifest) string {
	if m.MigrationState.Generation <= 1 {
		return string(migration.WasabiPrimary)
	}
	if m.MigrationState.CloudCopy == "" {
		return string(migration.LocalOnly)
	}
	if m.MigrationState.Generation == 2 {
		return string(migration.DualWrite)
	}
	if m.MigrationState.Generation == 3 {
		return string(migration.LocalPrimaryWasabiBackup)
	}
	return string(migration.LocalPrimaryWasabiDrain)
}

// rebalanceManifest copies each piece that still lives on the source
// backend to the primary. Existing pieces on the primary are left
// alone (the rebalancer is a write-once batch). The manifest's
// piece locator is updated in-place and persisted.
func (r *Rebalancer) rebalanceManifest(
	ctx context.Context,
	m *metadata.ObjectManifest,
	target TenantTarget,
	source providers.StorageProvider,
	primary providers.StorageProvider,
) (copied int, bytesCopied int64, err error) {
	dirty := false
	for i, piece := range m.Pieces {
		if piece.Backend != target.SourceBackend {
			continue
		}
		copyBytes, newLocator, copyErr := r.copyPieceStreaming(ctx, piece, target, source, primary)
		bytesCopied += copyBytes
		if copyErr != nil {
			return copied, bytesCopied, copyErr
		}
		m.Pieces[i].Backend = target.PrimaryBackend
		if newLocator != "" {
			m.Pieces[i].Locator = newLocator
		}
		copied++
		dirty = true
	}
	if dirty {
		key := manifest_store.ManifestKey{
			TenantID:      m.TenantID,
			Bucket:        m.Bucket,
			ObjectKeyHash: m.ObjectKeyHash,
			VersionID:     m.VersionID,
		}
		if err := r.cfg.Manifests.Put(ctx, key, m); err != nil {
			return copied, bytesCopied, fmt.Errorf("persist manifest: %w", err)
		}
	}
	return copied, bytesCopied, nil
}

// advancePhase moves the manifest one step forward in the state
// machine when all pieces have landed on the primary. It returns
// whether a transition happened.
func (r *Rebalancer) advancePhase(ctx context.Context, m *metadata.ObjectManifest, target TenantTarget) (bool, error) {
	for _, p := range m.Pieces {
		if p.Backend != target.PrimaryBackend {
			return false, nil
		}
	}
	current := migration.MigrationPhase(phaseOf(m))
	next, ok := nextPhase(current)
	if !ok {
		return false, nil
	}
	if err := migration.ValidateTransition(current, next); err != nil {
		return false, err
	}
	applyPhase(m, next)
	key := manifest_store.ManifestKey{
		TenantID:      m.TenantID,
		Bucket:        m.Bucket,
		ObjectKeyHash: m.ObjectKeyHash,
		VersionID:     m.VersionID,
	}
	if err := r.cfg.Manifests.Put(ctx, key, m); err != nil {
		return false, err
	}
	return true, nil
}

// nextPhase is the forward edge of the state machine. It stops at
// LocalOnly.
func nextPhase(p migration.MigrationPhase) (migration.MigrationPhase, bool) {
	switch p {
	case migration.WasabiPrimary:
		return migration.DualWrite, true
	case migration.DualWrite:
		return migration.LocalPrimaryWasabiBackup, true
	case migration.LocalPrimaryWasabiBackup:
		return migration.LocalPrimaryWasabiDrain, true
	case migration.LocalPrimaryWasabiDrain:
		return migration.LocalOnly, true
	default:
		return "", false
	}
}

// applyPhase writes the new phase onto the manifest's MigrationState.
// It increments Generation so Phase 2's numeric mapping in phaseOf
// stays internally consistent.
func applyPhase(m *metadata.ObjectManifest, next migration.MigrationPhase) {
	switch next {
	case migration.DualWrite:
		m.MigrationState.Generation = 2
		m.MigrationState.CloudCopy = "wasabi"
	case migration.LocalPrimaryWasabiBackup:
		m.MigrationState.Generation = 3
		m.MigrationState.CloudCopy = "wasabi"
	case migration.LocalPrimaryWasabiDrain:
		m.MigrationState.Generation = 4
		m.MigrationState.CloudCopy = "wasabi"
	case migration.LocalOnly:
		m.MigrationState.Generation = 5
		m.MigrationState.CloudCopy = ""
	}
}

// copyPieceStreaming pipes a single piece source→primary as a
// continuous io.Copy: GetPiece's body flows through a per-Read
// rate-limited throttledReader, then a TeeReader that feeds a
// pieceintegrity.Hasher tracking the on-wire bytes for content
// verification, and finally a countingReader that records the
// exact number of bytes that landed on the destination. The
// pipeline never materialises the full piece in the gateway's
// heap, which was the previous bottleneck — io.ReadAll on the
// source forced piece_size bytes of allocation and stalled the
// destination PUT until the source had fully drained.
//
// Integrity verification fires after the destination accepts the
// upload. A pieceintegrity.ErrIntegrityCheckFailed means the
// recomputed hash and the recorded Hash disagree (backend
// bit-rot, a tampered backend, or a manifest written with the
// wrong hash). The rebalancer treats that as fail-closed: the
// destination piece is deleted so a future pass can retry the
// copy (potentially from a different replica) and the caller's
// stats reflect the bytes that moved but not a successful
// copy. pieceintegrity.ErrIntegrityClaimUnrecognized means the
// manifest's Hash is non-empty but in a legacy format the
// verifier does not parse (e.g. a copy/dedup manifest that
// stamped a backend ETag into Hash); the bytes are not known to
// be wrong so the destination is left alone and the
// observability counter is incremented instead.
func (r *Rebalancer) copyPieceStreaming(
	ctx context.Context,
	piece metadata.Piece,
	target TenantTarget,
	source providers.StorageProvider,
	primary providers.StorageProvider,
) (bytesCopied int64, newLocator string, err error) {
	rc, err := source.GetPiece(ctx, piece.PieceID, nil)
	if err != nil {
		return 0, "", fmt.Errorf("get piece %s from %s: %w", piece.PieceID, target.SourceBackend, err)
	}
	defer rc.Close()

	contentLength, err := r.resolveContentLength(ctx, piece, target, source)
	if err != nil {
		return 0, "", err
	}

	hashWriter, checkHash := pieceintegrity.Hasher(piece)
	throttled := newThrottledReader(ctx, rc, r.cfg.BytesPerSecond)
	teed := io.TeeReader(throttled, hashWriter)
	counting := &countingReader{r: teed}

	put, err := primary.PutPiece(ctx, piece.PieceID, counting, providers.PutOptions{
		ContentLength: contentLength,
	})
	if err != nil {
		return counting.n, "", fmt.Errorf("put piece %s to %s: %w", piece.PieceID, target.PrimaryBackend, err)
	}

	if hashErr := checkHash(); hashErr != nil {
		switch {
		case errors.Is(hashErr, pieceintegrity.ErrIntegrityCheckFailed):
			r.recordIntegrityFailure(piece, target, hashErr)
			if delErr := primary.DeletePiece(ctx, piece.PieceID); delErr != nil {
				r.logf("background_rebalancer: failed to delete corrupted dest piece %s on %s: %v",
					piece.PieceID, target.PrimaryBackend, delErr)
			}
			return counting.n, "", fmt.Errorf("rebalance integrity check failed for piece %s: %w",
				piece.PieceID, hashErr)
		case errors.Is(hashErr, pieceintegrity.ErrIntegrityClaimUnrecognized):
			r.recordIntegrityClaimUnrecognized(piece, target, hashErr)
			// Fall through: keep the destination bytes because
			// we cannot prove they are wrong.
		default:
			// Defensive: an unexpected hash-check error should
			// still surface so the rebalancer does not silently
			// claim success.
			return counting.n, "", fmt.Errorf("rebalance hash check error for piece %s: %w",
				piece.PieceID, hashErr)
		}
	}
	return counting.n, put.Locator, nil
}

// resolveContentLength returns the byte count to advertise on the
// destination PutPiece. Manifests written by the Phase 4+ PUT
// path carry per-piece SizeBytes, so the common case is a
// constant-time lookup. Legacy manifests (multipart/copy/dedup
// flows that pre-date the SizeBytes stamp) may carry a zero,
// in which case we HEAD the source to learn the true size — S3
// backends require a known Content-Length on the upload and we
// would rather pay a single round trip than fall back to a
// chunked upload that some providers reject.
func (r *Rebalancer) resolveContentLength(
	ctx context.Context,
	piece metadata.Piece,
	target TenantTarget,
	source providers.StorageProvider,
) (int64, error) {
	if piece.SizeBytes > 0 {
		return piece.SizeBytes, nil
	}
	head, headErr := source.HeadPiece(ctx, piece.PieceID)
	if headErr != nil {
		return 0, fmt.Errorf("head piece %s on %s: %w", piece.PieceID, target.SourceBackend, headErr)
	}
	if head.SizeBytes <= 0 {
		// HEAD returned 0 — the source does not know either.
		// Defer to the destination's streaming-length contract
		// (some providers, like local_fs_dev, accept -1).
		return -1, nil
	}
	return head.SizeBytes, nil
}

// countingReader is a thin io.Reader middlebox that records the
// total bytes observed. It exists because the streaming-copy
// path's piece body never sits in a single buffer the caller can
// len() — the source's Read calls drive the count instead, and
// the destination's PutPiece result does not surface the byte
// count.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// recordIntegrityFailure logs a structured ERROR line and
// increments the optional metric sink for a verified content
// mismatch on a streamed piece. The log line carries every label
// an on-call needs to scope the investigation (which backend
// served the bad bytes, which destination the rebalancer was
// moving them to, what hash was recorded) without forcing the
// gateway to depend on internal/metrics from this package.
func (r *Rebalancer) recordIntegrityFailure(piece metadata.Piece, target TenantTarget, hashErr error) {
	r.logf("background_rebalancer: ERROR integrity_check_failed piece=%s source=%s primary=%s recorded_hash=%q err=%v",
		piece.PieceID, target.SourceBackend, target.PrimaryBackend, piece.Hash, hashErr)
	if r.cfg.IntegrityFailures != nil {
		r.cfg.IntegrityFailures.Inc(target.SourceBackend)
	}
}

// recordIntegrityClaimUnrecognized logs a structured WARN line
// and increments the observability sink for a streamed piece
// whose recorded Hash is non-empty but not in any recognised
// format. The rebalancer keeps the destination bytes (there is
// no proof they are wrong) and the counter lets operators see
// the population of legacy manifests that still need a rewrite.
func (r *Rebalancer) recordIntegrityClaimUnrecognized(piece metadata.Piece, target TenantTarget, hashErr error) {
	r.logf("background_rebalancer: WARN integrity_claim_unrecognized piece=%s source=%s primary=%s recorded_hash=%q detail=%v",
		piece.PieceID, target.SourceBackend, target.PrimaryBackend, piece.Hash, hashErr)
	if r.cfg.IntegrityFailures != nil {
		r.cfg.IntegrityFailures.IncUnrecognized(target.SourceBackend)
	}
}

func (r *Rebalancer) logf(format string, args ...any) {
	if r.cfg.Logger == nil {
		return
	}
	r.cfg.Logger.Printf(format, args...)
}
