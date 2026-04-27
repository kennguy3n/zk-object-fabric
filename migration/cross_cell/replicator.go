// Package cross_cell ships the asynchronous cross-cell
// replicator. The worker periodically scans manifests under a
// configured set of (tenant, bucket) scopes, looks for objects
// whose PlacementPolicy.ReplicationPolicy is non-nil and Mode is
// "async", and mirrors the underlying pieces from the source
// cell's provider into the destination cell's provider.
//
// The replicator is intentionally minimal: it does not handle
// sync (RPO=0) replication — that runs on the PUT critical path
// and lives in the s3compat handler — and it does not do
// cross-region failover. Its only contract is "for every async
// replication policy, the destination eventually carries the
// same bytes the source does", with a measurable lag exposed via
// the Lag accessor for the metrics scrape.
package cross_cell

import (
	"context"
	"errors"
	"io"
	"log"
	"sync/atomic"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// Cell bundles the manifest store and provider for one side of
// a replication pair.
type Cell struct {
	ID        string
	Manifests manifest_store.ManifestStore
	Provider  providers.StorageProvider
}

// ScopeKey limits which manifests the replicator scans on each
// tick. The replicator pages through (TenantID, Bucket) one
// (tenant, bucket) at a time so per-tenant work can be capped.
type ScopeKey struct {
	TenantID string
	Bucket   string
}

// Replicator drains async replication policies from a source
// cell to a destination cell.
type Replicator struct {
	Source   Cell
	Dest     Cell
	Scope    []ScopeKey
	Interval time.Duration
	PageSize int
	Logger   *log.Logger

	// lagNanos records the wall-clock duration of the most
	// recent replication tick. It is read by the metrics scrape
	// to surface "how stale is the destination cell".
	lagNanos atomic.Int64

	// copied counts the total number of pieces successfully
	// mirrored over the lifetime of the replicator.
	copied atomic.Int64
}

// NewReplicator returns a replicator wired to the source / dest
// pair. Interval defaults to 60s; PageSize to 100. Callers pass
// the desired Scope explicitly because there is no
// "list all tenants" surface on ManifestStore.
func NewReplicator(src, dst Cell, scope []ScopeKey) *Replicator {
	return &Replicator{
		Source:   src,
		Dest:     dst,
		Scope:    scope,
		Interval: 60 * time.Second,
		PageSize: 100,
	}
}

// Run blocks until ctx is cancelled. Replication ticks fire
// every Interval; cancellation is observed between ticks and
// between manifests so a long-running scan is not stuck on a
// dead context for an entire tick.
func (r *Replicator) Run(ctx context.Context) error {
	if r == nil {
		return errors.New("cross_cell: nil replicator")
	}
	if r.Interval <= 0 {
		r.Interval = 60 * time.Second
	}
	if r.PageSize <= 0 {
		r.PageSize = 100
	}
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	r.tick(ctx) // run once immediately so test harnesses do not have to wait the full Interval.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// LagNanos returns the duration of the most recent tick. A
// growing value indicates either a slow source-cell scan or a
// slow destination-cell write path.
func (r *Replicator) LagNanos() int64 { return r.lagNanos.Load() }

// CopiedPieces returns the lifetime count of successfully
// mirrored pieces.
func (r *Replicator) CopiedPieces() int64 { return r.copied.Load() }

func (r *Replicator) tick(ctx context.Context) {
	start := time.Now()
	defer func() { r.lagNanos.Store(time.Since(start).Nanoseconds()) }()
	for _, sk := range r.Scope {
		if ctx.Err() != nil {
			return
		}
		r.scanScope(ctx, sk)
	}
}

func (r *Replicator) scanScope(ctx context.Context, sk ScopeKey) {
	cursor := ""
	for {
		if ctx.Err() != nil {
			return
		}
		page, err := r.Source.Manifests.List(ctx, sk.TenantID, sk.Bucket, cursor, r.PageSize)
		if err != nil {
			r.logf("scan %s/%s: %v", sk.TenantID, sk.Bucket, err)
			return
		}
		for _, m := range page.Manifests {
			if ctx.Err() != nil {
				return
			}
			if m == nil || m.PlacementPolicy.ReplicationPolicy == nil {
				continue
			}
			pol := m.PlacementPolicy.ReplicationPolicy
			if pol.Mode != "async" {
				continue
			}
			if pol.SourceCell != "" && pol.SourceCell != r.Source.ID {
				continue
			}
			if pol.DestCell != "" && pol.DestCell != r.Dest.ID {
				continue
			}
			if err := r.replicateOne(ctx, m); err != nil {
				r.logf("replicate %s/%s/%s: %v",
					m.TenantID, m.Bucket, m.ObjectKey, err)
			}
		}
		if page.NextCursor == "" {
			return
		}
		cursor = page.NextCursor
	}
}

// replicateOne copies every Piece referenced by m from the
// source provider to the destination provider, then writes a
// replica manifest into the destination manifest store. The
// piece IDs are preserved so dest manifests address the same
// blobs by ID as the source.
func (r *Replicator) replicateOne(ctx context.Context, m *metadata.ObjectManifest) error {
	for _, p := range m.Pieces {
		if err := r.copyPiece(ctx, p); err != nil {
			return err
		}
		r.copied.Add(1)
	}
	key := manifest_store.ManifestKey{
		TenantID:      m.TenantID,
		Bucket:        m.Bucket,
		ObjectKeyHash: m.ObjectKeyHash,
		VersionID:     m.VersionID,
	}
	return r.Dest.Manifests.Put(ctx, key, m)
}

func (r *Replicator) copyPiece(ctx context.Context, p metadata.Piece) error {
	rc, err := r.Source.Provider.GetPiece(ctx, p.PieceID, nil)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = r.Dest.Provider.PutPiece(ctx, p.PieceID, rc, providers.PutOptions{})
	return err
}

// drainPiece is the io-helper used by tests so they can verify
// the body that landed in the dest provider.
func drainPiece(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(rc)
}

func (r *Replicator) logf(format string, args ...interface{}) {
	if r.Logger == nil {
		return
	}
	r.Logger.Printf(format, args...)
}
