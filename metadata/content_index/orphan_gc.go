// Package content_index orphan_gc.go: background sweep that
// removes content_index rows whose piece is no longer referenced
// by any live manifest.
//
// Phase 3.5 wired DELETE-time refcounting (see handler.go), but
// non-deduped paths and crash recoveries can leave content_index
// rows whose ref_count never reached zero on the DELETE path.
// The sweep walks every (tenant, content_hash) row periodically,
// asks the manifest store whether any manifest in the tenant
// still references the entry's piece_id, and — if not — deletes
// the backend piece and the index row.
//
// The sweep is intentionally conservative: it only acts when
// HasManifestWithPieceID returns false, never on a transient
// store error. It also respects context cancellation between
// rows so a long sweep is interruptible.
package content_index

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
)

// PieceDeleter is the subset of providers.StorageProvider the
// orphan GC needs. The handler's full provider registry satisfies
// this interface via the shared map[string]providers.StorageProvider.
type PieceDeleter interface {
	DeletePiece(ctx context.Context, pieceID string) error
}

// ProviderResolver returns the PieceDeleter for the named backend
// or false if no such backend is registered. The gateway's
// provider registry satisfies this via a small adapter; tests
// supply a stub.
type ProviderResolver func(backend string) (PieceDeleter, bool)

// Logger is the minimal logging surface the worker uses. *log.Logger
// satisfies it; tests pass a no-op.
type Logger interface {
	Printf(format string, args ...any)
}

// OrphanGCConfig configures the sweep.
type OrphanGCConfig struct {
	// Index is the content_index store the sweep walks.
	Index Store

	// Manifests is the manifest store consulted to decide
	// whether a piece is still live.
	Manifests manifest_store.ManifestStore

	// Resolver maps backend names to PieceDeleters.
	Resolver ProviderResolver

	// Interval is the sweep cadence. Zero defers to the
	// caller's ticker; the worker itself only enforces a
	// floor at 1 second to avoid pathological tight loops.
	Interval time.Duration

	// Logger receives one-line operator-facing notes about
	// each sweep. Optional; defaults to log.Default().
	Logger Logger
}

// OrphanGC runs the orphan-row sweep on a configurable interval.
type OrphanGC struct {
	cfg OrphanGCConfig
}

// NewOrphanGC validates cfg and returns a worker.
func NewOrphanGC(cfg OrphanGCConfig) (*OrphanGC, error) {
	if cfg.Index == nil {
		return nil, errors.New("orphan_gc: Index is required")
	}
	if cfg.Manifests == nil {
		return nil, errors.New("orphan_gc: Manifests is required")
	}
	if cfg.Resolver == nil {
		return nil, errors.New("orphan_gc: Resolver is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &OrphanGC{cfg: cfg}, nil
}

// SweepStats summarises a single sweep pass.
type SweepStats struct {
	TenantsScanned   int
	RowsScanned      int
	RowsOrphaned     int
	PiecesDeleted    int
	IndexRowsDeleted int
	Errors           int
}

// Sweep performs one synchronous pass over every (tenant, row)
// the index reports. It returns aggregated counters; per-row
// errors are logged and counted in Stats.Errors but never abort
// the sweep.
func (g *OrphanGC) Sweep(ctx context.Context) (SweepStats, error) {
	tenants, err := g.cfg.Index.ListTenants(ctx)
	if err != nil {
		return SweepStats{}, fmt.Errorf("orphan_gc: list tenants: %w", err)
	}
	stats := SweepStats{TenantsScanned: len(tenants)}
	for _, tenantID := range tenants {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		tStats, terr := g.sweepTenant(ctx, tenantID)
		stats.RowsScanned += tStats.RowsScanned
		stats.RowsOrphaned += tStats.RowsOrphaned
		stats.PiecesDeleted += tStats.PiecesDeleted
		stats.IndexRowsDeleted += tStats.IndexRowsDeleted
		stats.Errors += tStats.Errors
		if terr != nil {
			g.cfg.Logger.Printf("orphan_gc: tenant=%s error: %v", tenantID, terr)
		}
	}
	return stats, nil
}

func (g *OrphanGC) sweepTenant(ctx context.Context, tenantID string) (SweepStats, error) {
	entries, err := g.cfg.Index.ScanAll(ctx, tenantID)
	if err != nil {
		return SweepStats{Errors: 1}, fmt.Errorf("scan: %w", err)
	}
	stats := SweepStats{}
	stats.RowsScanned = len(entries)
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		live, herr := g.cfg.Manifests.HasManifestWithPieceID(ctx, e.TenantID, e.PieceID)
		if herr != nil {
			stats.Errors++
			g.cfg.Logger.Printf("orphan_gc: tenant=%s hash=%s manifest probe failed: %v", e.TenantID, e.ContentHash, herr)
			continue
		}
		if live {
			continue
		}
		stats.RowsOrphaned++
		// No live manifest references this piece. Drop the
		// backend piece first; if the provider is gone we
		// still delete the row so the orphan does not stick
		// around forever.
		if provider, ok := g.cfg.Resolver(e.Backend); ok {
			if derr := provider.DeletePiece(ctx, e.PieceID); derr != nil {
				stats.Errors++
				g.cfg.Logger.Printf("orphan_gc: tenant=%s piece=%s backend=%s delete failed: %v",
					e.TenantID, e.PieceID, e.Backend, derr)
				continue
			}
			stats.PiecesDeleted++
		} else {
			g.cfg.Logger.Printf("orphan_gc: tenant=%s piece=%s backend=%s not registered; deleting index row only",
				e.TenantID, e.PieceID, e.Backend)
		}
		// The DELETE handler races with PUT IncrementRef; we
		// reuse the conditional Delete contract: the row only
		// vanishes when ref_count == 0. Real orphans always
		// have ref_count > 0 because the DELETE handler only
		// drops to zero (and Delete-row) on the racing path.
		// Bypass that constraint for the GC sweep by using
		// DecrementRef in a loop until the row vanishes or
		// ref_count returns 0; in production the DB-level
		// constraint ensures we never go negative.
		for {
			n, derr := g.cfg.Index.DecrementRef(ctx, e.TenantID, e.ContentHash)
			if errors.Is(derr, ErrNotFound) {
				stats.IndexRowsDeleted++
				break
			}
			if errors.Is(derr, ErrInvalidRefCount) {
				// already zero — try Delete; if a racer
				// bumped the row, leave it.
				_ = g.cfg.Index.Delete(ctx, e.TenantID, e.ContentHash)
				stats.IndexRowsDeleted++
				break
			}
			if derr != nil {
				stats.Errors++
				g.cfg.Logger.Printf("orphan_gc: tenant=%s hash=%s decrement failed: %v",
					e.TenantID, e.ContentHash, derr)
				break
			}
			if n == 0 {
				if delErr := g.cfg.Index.Delete(ctx, e.TenantID, e.ContentHash); delErr == nil {
					stats.IndexRowsDeleted++
				} else if errors.Is(delErr, ErrRefCountNonZero) {
					// A concurrent PUT bumped the row;
					// leave it alone.
				} else if !errors.Is(delErr, ErrNotFound) {
					stats.Errors++
					g.cfg.Logger.Printf("orphan_gc: tenant=%s hash=%s delete failed: %v",
						e.TenantID, e.ContentHash, delErr)
				}
				break
			}
			// n > 0 means a concurrent uploader bumped the
			// row between our HasManifestWithPieceID probe
			// and DecrementRef. Stop draining: the new
			// uploader's manifest is now the canonical
			// reference.
			break
		}
	}
	return stats, nil
}

// Run drives Sweep on g.cfg.Interval until ctx is cancelled. It
// is the entry point used by cmd/gateway as a background
// goroutine. The first sweep runs immediately; subsequent sweeps
// wait one Interval between completions.
func (g *OrphanGC) Run(ctx context.Context) {
	interval := g.cfg.Interval
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		stats, err := g.Sweep(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			g.cfg.Logger.Printf("orphan_gc: sweep error: %v", err)
		}
		if stats.RowsOrphaned > 0 || stats.PiecesDeleted > 0 || stats.IndexRowsDeleted > 0 {
			g.cfg.Logger.Printf("orphan_gc: tenants=%d rows=%d orphans=%d pieces_deleted=%d rows_deleted=%d errors=%d",
				stats.TenantsScanned, stats.RowsScanned, stats.RowsOrphaned,
				stats.PiecesDeleted, stats.IndexRowsDeleted, stats.Errors)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
