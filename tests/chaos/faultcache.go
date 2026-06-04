package chaos

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
)

// FaultCache wraps a hot_object_cache.HotObjectCache with the same
// fault-injection vocabulary as FaultProvider and FaultManifestStore,
// applied to the gateway's L0/L1 hot-object-cache path.
//
// It models the cache-tier failure modes that actually happen in
// production:
//
//   - The cache partition is wedged: every Get errors (NVMe mount
//     gone, L1 node unreachable). The gateway must treat the error as
//     a miss and fall through to the origin provider — modelled with
//     GetFault.Mode == ModeAlwaysFail.
//   - The cache is slow enough to blow the request's latency budget.
//     Modelled with GetFault.Mode == ModeSlowResponse plus a Latency
//     larger than the caller's context deadline.
//   - The cache returns bytes that no longer match the manifest hash
//     (bit-rot / truncated spill file). Modelled with CorruptReads so
//     the gateway's post-read integrity check trips instead of serving
//     bad data.
//
// The zero value (with a nil Inner) behaves as a permanently-empty
// cache: every Get is a miss and every Put/Evict is a no-op. That is
// the most common chaos setup — the test only cares that the gateway
// keeps serving correct data when the cache is useless — so it needs
// no backing store.
//
// FaultCache is safe for concurrent use.
type FaultCache struct {
	// Inner is the wrapped cache. Optional: a nil Inner makes Get a
	// guaranteed miss (ErrCacheMiss) and Put/Evict no-ops, which is
	// how a test models "the cache holds nothing" without standing
	// up a real MemoryCache.
	Inner hot_object_cache.HotObjectCache

	// Now is the clock used by ModeFailUntilTime. Defaults to
	// time.Now if nil. Tests override this for determinism.
	Now func() time.Time

	// Sleep is the time-skip primitive used by ModeSlowResponse and
	// any Latency injection. Defaults to a ctx-aware timer if nil.
	// Tests override this to avoid real sleeps.
	Sleep func(time.Duration)

	// Per-operation faults. Each Get/Put/Evict consults exactly one.
	// Embedding by-value keeps the zero-value usage clean (everything
	// passes through to Inner, or misses when Inner is nil).
	GetFault   FaultConfig
	PutFault   FaultConfig
	EvictFault FaultConfig

	// CorruptReads, when true, flips the bytes of any body returned
	// by a cache hit so the gateway's post-read BLAKE3 verification
	// fails. It only affects hits that would otherwise be served; a
	// miss or an injected Get error is returned unchanged. Use this
	// to prove the gateway refuses to serve a corrupted cache entry
	// rather than streaming it to the client.
	CorruptReads bool

	mu       sync.Mutex
	counters map[string]int64

	// Calls / Failures track total / faulted operations across all
	// method kinds. Hits / Misses break the Get path down further so
	// a test can assert "every request fell through to the origin"
	// (Hits == 0) or "the cache was actually consulted" (Calls > 0).
	Calls    atomic.Int64
	Failures atomic.Int64
	Hits     atomic.Int64
	Misses   atomic.Int64
}

// NewFaultCache returns a fault-injecting wrapper around inner. inner
// may be nil, in which case the cache behaves as permanently empty
// (see the FaultCache doc comment).
func NewFaultCache(inner hot_object_cache.HotObjectCache) *FaultCache {
	return &FaultCache{Inner: inner}
}

func (c *FaultCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// sleep blocks for d, honouring ctx cancellation iff the default
// timer path is used. Mirrors FaultProvider.sleep so a slow cache
// respects the caller's deadline instead of pinning the goroutine —
// otherwise a ModeSlowResponse Get would outlive the request it is
// supposed to be slowing down.
func (c *FaultCache) sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	if ctx == nil {
		time.Sleep(d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
		return
	}
}

// shouldFail mirrors FaultProvider.shouldFail / FaultManifestStore.
// shouldFail. We keep a separate copy (rather than sharing) so the
// ModeFailEveryNth / ModeFailFirstN call counters live in the cache's
// own operation-name namespace — a test that wraps the provider, the
// manifest store, AND the cache at once gets independent cadences for
// each, which is what makes "fail the cache but not the origin"
// expressible.
func (c *FaultCache) shouldFail(op string, cfg FaultConfig) (bool, error) {
	switch cfg.Mode {
	case ModeNone:
		return false, nil
	case ModeAlwaysFail:
		return true, faultErr(cfg)
	case ModeFailEveryNth:
		n := cfg.EveryNth
		if n <= 1 {
			return true, faultErr(cfg)
		}
		if c.bumpCounter(op)%int64(n) == 0 {
			return true, faultErr(cfg)
		}
		return false, nil
	case ModeFailFirstN:
		if cfg.FirstN <= 0 {
			return false, nil
		}
		if c.bumpCounter(op) <= int64(cfg.FirstN) {
			return true, faultErr(cfg)
		}
		return false, nil
	case ModeFailUntilTime:
		if c.now().Before(cfg.FailUntil) {
			return true, faultErr(cfg)
		}
		return false, nil
	case ModeTruncatedRead:
		// Handled inside Get (it wraps the returned reader);
		// treat as pass-through here so shouldFail never claims a
		// truncated read "failed" before the body is produced.
		return false, nil
	case ModeSlowResponse:
		// Pass-through with extra latency; handled by applyLatency.
		return false, nil
	default:
		return true, errors.New("chaos: unknown FaultMode " + cfg.Mode.String())
	}
}

func (c *FaultCache) bumpCounter(op string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counters == nil {
		c.counters = map[string]int64{}
	}
	c.counters[op]++
	return c.counters[op]
}

func (c *FaultCache) applyLatency(ctx context.Context, cfg FaultConfig) {
	if cfg.Mode == ModeSlowResponse || cfg.Latency > 0 {
		c.sleep(ctx, cfg.Latency)
	}
}

// Get dispatches through GetFault. An injected fault is returned to
// the caller verbatim: the gateway's fetchPiece treats ANY Get error
// (including ErrCacheMiss) as a miss and falls through to the origin,
// so a test can model either a hard cache-backend error or a forced
// miss by choosing FaultConfig.Err.
//
// ModeTruncatedRead and CorruptReads only bite on a real hit from
// Inner; with a nil Inner there is nothing to truncate or corrupt and
// Get is an unconditional miss.
func (c *FaultCache) Get(ctx context.Context, pieceID string) (io.ReadCloser, hot_object_cache.CachedPieceMetadata, error) {
	c.Calls.Add(1)
	c.applyLatency(ctx, c.GetFault)
	if err := ctx.Err(); err != nil {
		c.Failures.Add(1)
		return nil, hot_object_cache.CachedPieceMetadata{}, err
	}
	if c.GetFault.Mode == ModeTruncatedRead && c.Inner != nil {
		rc, meta, err := c.Inner.Get(ctx, pieceID)
		if err != nil {
			c.Misses.Add(1)
			return nil, hot_object_cache.CachedPieceMetadata{}, err
		}
		c.Hits.Add(1)
		if c.GetFault.TruncateAfterBytes <= 0 {
			return rc, meta, nil
		}
		c.Failures.Add(1)
		return &truncatedReader{
			inner:     rc,
			remaining: c.GetFault.TruncateAfterBytes,
			tripErr:   faultErr(c.GetFault),
		}, meta, nil
	}
	fail, err := c.shouldFail("GET", c.GetFault)
	if fail {
		c.Failures.Add(1)
		c.Misses.Add(1)
		return nil, hot_object_cache.CachedPieceMetadata{}, err
	}
	if c.Inner == nil {
		c.Misses.Add(1)
		return nil, hot_object_cache.CachedPieceMetadata{}, hot_object_cache.ErrCacheMiss
	}
	rc, meta, err := c.Inner.Get(ctx, pieceID)
	if err != nil {
		c.Misses.Add(1)
		return nil, hot_object_cache.CachedPieceMetadata{}, err
	}
	c.Hits.Add(1)
	if c.CorruptReads {
		buf, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			return nil, hot_object_cache.CachedPieceMetadata{}, rerr
		}
		return io.NopCloser(bytes.NewReader(corrupt(buf))), meta, nil
	}
	return rc, meta, nil
}

// Put dispatches through PutFault. The gateway warms the cache
// best-effort and ignores Put errors, so a faulted Put models a cache
// that cannot accept writes (full disk, read-only mount) without
// breaking the read path.
func (c *FaultCache) Put(ctx context.Context, pieceID string, r io.Reader, opts hot_object_cache.PutOptions) error {
	c.Calls.Add(1)
	c.applyLatency(ctx, c.PutFault)
	if err := ctx.Err(); err != nil {
		c.Failures.Add(1)
		return err
	}
	fail, err := c.shouldFail("PUT", c.PutFault)
	if fail {
		c.Failures.Add(1)
		// Drain the body so a caller that streams into Put does not
		// see a short write race; the bytes are discarded because the
		// fault models the cache rejecting the entry.
		_, _ = io.Copy(io.Discard, r)
		return err
	}
	if c.Inner == nil {
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	return c.Inner.Put(ctx, pieceID, r, opts)
}

// Evict dispatches through EvictFault. Eviction is idempotent, so a
// nil Inner is a no-op success.
func (c *FaultCache) Evict(ctx context.Context, pieceID string) error {
	c.Calls.Add(1)
	c.applyLatency(ctx, c.EvictFault)
	if err := ctx.Err(); err != nil {
		c.Failures.Add(1)
		return err
	}
	fail, err := c.shouldFail("EVICT", c.EvictFault)
	if fail {
		c.Failures.Add(1)
		return err
	}
	if c.Inner == nil {
		return nil
	}
	return c.Inner.Evict(ctx, pieceID)
}

// Stats delegates to Inner when present, otherwise reports the
// FaultCache's own hit/miss tallies so a test driving a nil-Inner
// cache can still assert on observed traffic.
func (c *FaultCache) Stats() hot_object_cache.Stats {
	if c.Inner != nil {
		return c.Inner.Stats()
	}
	return hot_object_cache.Stats{
		Hits:   uint64(c.Hits.Load()),
		Misses: uint64(c.Misses.Load()),
	}
}

// corrupt returns a copy of b with its bytes perturbed so a hash
// check over the result cannot match the original. A single flipped
// byte is enough; flipping the first byte (or appending one for an
// empty input) keeps the helper allocation-cheap and deterministic.
func corrupt(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	if len(out) == 0 {
		return []byte{0xFF}
	}
	out[0] ^= 0xFF
	return out
}
