// Package repair drives the automated repair queue. The queue
// polls a Ceph health source (or any equivalent
// HealthSignalSource), correlates degraded shards with the
// gateway's EC manifests, and re-encodes any object whose data
// shards have been compromised below the recovery threshold.
//
// Repair is a best-effort, eventually-consistent loop: a single
// failed re-encode does not stop the queue from advancing to
// the next damaged manifest. Failures are logged and counted.
package repair

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"sync/atomic"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// HealthSignal carries a single health "tick" from a backend.
// AffectedPieceIDs is the canonical input the repair queue acts
// on; the upstream Ceph integration is responsible for resolving
// OSD-down / PG-degraded events into the piece IDs that live on
// the failed media.
type HealthSignal struct {
	// Healthy is true when the backend reports HEALTH_OK and the
	// queue can short-circuit. False indicates one or more
	// AffectedPieceIDs are degraded.
	Healthy bool

	// AffectedPieceIDs is the set of pieces the upstream
	// integration believes are degraded. Empty when Healthy is
	// true.
	AffectedPieceIDs []string

	// ObservedAt is the wall-clock timestamp of the source poll.
	ObservedAt time.Time
}

// HealthSignalSource is the polling interface for backend
// health. The Ceph adapter wraps the /api/v0.1/health endpoint;
// tests pass an in-memory fake.
type HealthSignalSource interface {
	Poll(ctx context.Context) (HealthSignal, error)
}

// ManifestScanner is the read surface the repair queue uses to
// discover which manifests reference a degraded piece. The
// concrete implementation iterates a small set of (tenant,
// bucket) tuples (the same scope shape the cross-cell
// replicator uses).
type ManifestScanner interface {
	// FindManifestsByPieceID returns the manifests that
	// reference any of pieceIDs. Implementations MAY return
	// approximate matches; the caller revalidates each manifest
	// before re-encoding.
	FindManifestsByPieceID(ctx context.Context, pieceIDs []string) ([]*metadata.ObjectManifest, error)
}

// RepairQueue is the background worker that drains health
// signals into re-encode jobs.
type RepairQueue struct {
	Source        HealthSignalSource
	Scanner       ManifestScanner
	Manifests     manifest_store.ManifestStore
	Providers     map[string]providers.StorageProvider
	ErasureCoding *erasure_coding.Registry
	PollInterval  time.Duration
	Logger        *log.Logger

	repaired atomic.Int64
	failed   atomic.Int64
	last     atomic.Int64 // unix nanos of most recent poll
}

// NewRepairQueue returns a queue wired to source / scanner /
// store / registry. PollInterval defaults to 30 seconds.
func NewRepairQueue(
	src HealthSignalSource,
	sc ManifestScanner,
	ms manifest_store.ManifestStore,
	reg map[string]providers.StorageProvider,
	ec *erasure_coding.Registry,
) *RepairQueue {
	return &RepairQueue{
		Source:        src,
		Scanner:       sc,
		Manifests:     ms,
		Providers:     reg,
		ErasureCoding: ec,
		PollInterval:  30 * time.Second,
	}
}

// Run blocks until ctx is cancelled. Polls fire every
// PollInterval; cancellation is observed between polls and
// between manifests so the queue does not stall on a dead ctx.
func (q *RepairQueue) Run(ctx context.Context) error {
	if q == nil {
		return errors.New("repair: nil queue")
	}
	if q.PollInterval <= 0 {
		q.PollInterval = 30 * time.Second
	}
	t := time.NewTicker(q.PollInterval)
	defer t.Stop()
	q.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			q.poll(ctx)
		}
	}
}

// RepairedCount returns the lifetime number of manifests
// successfully re-encoded.
func (q *RepairQueue) RepairedCount() int64 { return q.repaired.Load() }

// FailedCount returns the lifetime number of re-encode attempts
// that returned an error.
func (q *RepairQueue) FailedCount() int64 { return q.failed.Load() }

// LastPollAt returns the wall-clock time of the most recent
// poll, or the zero time if the queue has not yet ticked.
func (q *RepairQueue) LastPollAt() time.Time {
	n := q.last.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (q *RepairQueue) poll(ctx context.Context) {
	q.last.Store(time.Now().UnixNano())
	sig, err := q.Source.Poll(ctx)
	if err != nil {
		q.logf("poll: %v", err)
		return
	}
	if sig.Healthy || len(sig.AffectedPieceIDs) == 0 {
		return
	}
	manifests, err := q.Scanner.FindManifestsByPieceID(ctx, sig.AffectedPieceIDs)
	if err != nil {
		q.logf("scan: %v", err)
		return
	}
	for _, m := range manifests {
		if ctx.Err() != nil {
			return
		}
		if !isErasureCoded(m) {
			continue
		}
		if err := q.repairManifest(ctx, m, sig.AffectedPieceIDs); err != nil {
			q.failed.Add(1)
			q.logf("repair %s/%s/%s: %v", m.TenantID, m.Bucket, m.ObjectKey, err)
			continue
		}
		q.repaired.Add(1)
	}
}

// repairManifest reads every surviving shard, decodes the
// plaintext via the EC profile, re-encodes a full set of
// shards, and writes back any shard that was previously
// degraded. The manifest itself is untouched on success —
// piece IDs and the pieces slice are preserved.
func (q *RepairQueue) repairManifest(ctx context.Context, m *metadata.ObjectManifest, degraded []string) error {
	if m == nil || len(m.Pieces) == 0 {
		return errors.New("manifest has no pieces")
	}
	enc, err := q.encoderFor(m)
	if err != nil {
		return err
	}
	input := make([]erasure_coding.Shard, 0, len(m.Pieces))
	for _, p := range m.Pieces {
		shard := erasure_coding.Shard{
			StripeIndex: p.StripeIndex,
			ShardIndex:  p.ShardIndex,
			Kind:        shardKindFor(p.ShardKind),
		}
		if !contains(degraded, p.PieceID) {
			body, err := q.fetchPiece(ctx, p)
			if err != nil {
				q.logf("survivor read %s: %v", p.PieceID, err)
			} else {
				shard.Bytes = body
			}
		}
		input = append(input, shard)
	}
	plain, err := enc.Decode(input)
	if err != nil {
		return err
	}
	fresh, err := enc.Encode(plain)
	if err != nil {
		return err
	}
	freshIdx := make(map[[2]int]erasure_coding.Shard, len(fresh))
	for _, sh := range fresh {
		freshIdx[[2]int{sh.StripeIndex, sh.ShardIndex}] = sh
	}
	for _, p := range m.Pieces {
		if !contains(degraded, p.PieceID) {
			continue
		}
		sh, ok := freshIdx[[2]int{p.StripeIndex, p.ShardIndex}]
		if !ok {
			return errors.New("encoder did not produce shard for degraded piece")
		}
		if err := q.writePiece(ctx, p, sh.Bytes); err != nil {
			return err
		}
	}
	return nil
}

func (q *RepairQueue) encoderFor(m *metadata.ObjectManifest) (*erasure_coding.Encoder, error) {
	if q.ErasureCoding == nil {
		return nil, errors.New("no erasure-coding registry")
	}
	name := m.PlacementPolicy.ErasureProfile
	if name == "" {
		return nil, errors.New("manifest has no erasure profile")
	}
	return q.ErasureCoding.Lookup(name)
}

func shardKindFor(s string) erasure_coding.ShardKind {
	if s == "parity" {
		return erasure_coding.ShardKindParity
	}
	return erasure_coding.ShardKindData
}

func (q *RepairQueue) fetchPiece(ctx context.Context, p metadata.Piece) ([]byte, error) {
	prov, ok := q.Providers[p.Backend]
	if !ok {
		return nil, errors.New("provider not registered: " + p.Backend)
	}
	rc, err := prov.GetPiece(ctx, p.PieceID, nil)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (q *RepairQueue) writePiece(ctx context.Context, p metadata.Piece, body []byte) error {
	prov, ok := q.Providers[p.Backend]
	if !ok {
		return errors.New("provider not registered: " + p.Backend)
	}
	_, err := prov.PutPiece(ctx, p.PieceID, bytes.NewReader(body), providers.PutOptions{ContentLength: int64(len(body))})
	return err
}

func (q *RepairQueue) logf(format string, args ...interface{}) {
	if q.Logger == nil {
		return
	}
	q.Logger.Printf(format, args...)
}

func isErasureCoded(m *metadata.ObjectManifest) bool {
	if m == nil {
		return false
	}
	return m.PlacementPolicy.ErasureProfile != ""
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}



// NoopScanner is a placeholder ManifestScanner that returns an
// empty manifest list. Production deployments inject a
// CRUSH-aware scanner here; the noop scanner exists so the
// repair queue can be wired into the gateway's lifecycle without
// the production scanner being available yet.
type NoopScanner struct{}

// FindManifestsByPieceID always returns nil, nil.
func (NoopScanner) FindManifestsByPieceID(_ context.Context, _ []string) ([]*metadata.ObjectManifest, error) {
	return nil, nil
}
