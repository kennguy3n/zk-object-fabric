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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/migration"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

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
	// means no cap.
	BytesPerSecond int64

	// PageSize is the ManifestStore list page size. Zero defaults
	// to 500.
	PageSize int

	// Clock, if set, returns the current time. Tests override it
	// to make the bandwidth throttle deterministic.
	Clock func() time.Time

	// Logger receives per-piece outcomes. Nil disables logging.
	Logger *log.Logger
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
		rc, err := source.GetPiece(ctx, piece.PieceID, nil)
		if err != nil {
			return copied, bytesCopied, fmt.Errorf("get piece %s from %s: %w", piece.PieceID, target.SourceBackend, err)
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return copied, bytesCopied, fmt.Errorf("drain piece %s: %w", piece.PieceID, err)
		}
		r.throttle(int64(len(body)))

		put, err := primary.PutPiece(ctx, piece.PieceID, bytes.NewReader(body), providers.PutOptions{
			ContentLength: int64(len(body)),
		})
		if err != nil {
			return copied, bytesCopied, fmt.Errorf("put piece %s to %s: %w", piece.PieceID, target.PrimaryBackend, err)
		}
		m.Pieces[i].Backend = target.PrimaryBackend
		if put.Locator != "" {
			m.Pieces[i].Locator = put.Locator
		}
		copied++
		bytesCopied += int64(len(body))
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

// throttle pauses the worker to honour BytesPerSecond. It uses a
// simple token-bucket model: one call per copy reserves `bytes` of
// budget and sleeps the shortfall.
func (r *Rebalancer) throttle(bytes int64) {
	if r.cfg.BytesPerSecond <= 0 {
		return
	}
	d := time.Duration(float64(bytes) / float64(r.cfg.BytesPerSecond) * float64(time.Second))
	if d > 0 {
		time.Sleep(d)
	}
}

func (r *Rebalancer) logf(format string, args ...any) {
	if r.cfg.Logger == nil {
		return
	}
	r.cfg.Logger.Printf(format, args...)
}
