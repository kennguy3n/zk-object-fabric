package hot_object_cache

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// DefaultPromotionWindow is the rolling aggregation window the
// PromotionWorker uses when evaluating DailyReadCountThreshold. It
// matches the "daily" intent of the policy while keeping the counter
// simple (fixed-width bucket that resets after the window elapses).
const DefaultPromotionWindow = 24 * time.Hour

// promotionPruneThreshold is the size at which the read counter
// sweeps expired entries opportunistically. Kept small so tests that
// exercise many pieces don't retain them forever.
const promotionPruneThreshold = 1024

// PromotionFetcher resolves a PromotionSignal to the ciphertext
// bytes it should copy into the cache. Phase 2 wires this to the
// gateway's StorageProvider registry so PromotionWorker can pull
// directly from the Wasabi origin or any other backend.
type PromotionFetcher interface {
	Fetch(ctx context.Context, sig PromotionSignal) (providers.StorageProvider, error)
}

// PromotionWorker consumes PromotionSignal from a channel, evaluates
// each signal against the configured policies, and promotes eligible
// pieces into the Cache. Multiple policies may be configured (L0 and
// L1) and are evaluated in order — the first match wins.
type PromotionWorker struct {
	Cache    HotObjectCache
	Policies []PromotionPolicy
	Fetcher  PromotionFetcher

	// Window overrides the per-piece read-count aggregation window.
	// Zero uses DefaultPromotionWindow. DailyReadCountThreshold in
	// the policy is evaluated against the count observed within
	// this window.
	Window time.Duration

	// Logger is optional. Nil disables internal logging.
	Logger *log.Logger

	counterOnce sync.Once
	counter     *readCounter
}

// readCounter is a per-piece sliding-window read counter. A piece's
// bucket resets when the current observation lands outside its
// window relative to the bucket's start. Expired buckets are pruned
// opportunistically once the map exceeds promotionPruneThreshold.
type readCounter struct {
	mu      sync.Mutex
	window  time.Duration
	entries map[string]*readCounterEntry
}

type readCounterEntry struct {
	count       uint64
	windowStart time.Time
}

func newReadCounter(window time.Duration) *readCounter {
	return &readCounter{window: window, entries: make(map[string]*readCounterEntry)}
}

// observe records `delta` reads for pieceID at `now` and returns the
// total read count currently attributed to the active window.
func (c *readCounter) observe(pieceID string, delta uint64, now time.Time) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[pieceID]
	if !ok || now.Sub(e.windowStart) >= c.window {
		e = &readCounterEntry{windowStart: now}
		c.entries[pieceID] = e
	}
	e.count += delta
	if len(c.entries) > promotionPruneThreshold {
		for k, v := range c.entries {
			if now.Sub(v.windowStart) >= c.window {
				delete(c.entries, k)
			}
		}
	}
	return e.count
}

func (w *PromotionWorker) readCounter() *readCounter {
	w.counterOnce.Do(func() {
		window := w.Window
		if window <= 0 {
			window = DefaultPromotionWindow
		}
		w.counter = newReadCounter(window)
	})
	return w.counter
}

// Run drains signals until ctx is cancelled or the channel is closed.
// It returns only when the channel is drained.
func (w *PromotionWorker) Run(ctx context.Context, signals <-chan PromotionSignal) {
	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-signals:
			if !ok {
				return
			}
			w.handle(ctx, sig)
		}
	}
}

func (w *PromotionWorker) handle(ctx context.Context, sig PromotionSignal) {
	observedAt := sig.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	delta := sig.ReadCount
	if delta == 0 {
		delta = 1
	}
	windowCount := w.readCounter().observe(sig.PieceID, delta, observedAt)

	pol, ok := w.match(sig, windowCount)
	if !ok {
		return
	}
	if w.Cache == nil || w.Fetcher == nil {
		return
	}
	provider, err := w.Fetcher.Fetch(ctx, sig)
	if err != nil {
		w.logf("promotion: fetch signal for %s: %v", sig.PieceID, err)
		return
	}
	body, err := provider.GetPiece(ctx, sig.PieceID, nil)
	if err != nil {
		w.logf("promotion: get %s from %s: %v", sig.PieceID, sig.OriginBackend, err)
		return
	}
	defer body.Close()

	if err := w.Cache.Put(ctx, sig.PieceID, body, PutOptions{
		SizeBytes: sig.PieceSizeBytes,
		PinHot:    pol.PinHotByDefault,
	}); err != nil {
		w.logf("promotion: cache put %s: %v", sig.PieceID, err)
	}
}

// match picks the first policy whose thresholds sig crosses. Piece
// size comes from sig.PieceSizeBytes (the full piece, not the range
// read) and the read-count threshold is evaluated against
// windowCount — the aggregated read count for this piece inside the
// worker's rolling window. A missing policy means no promotion.
func (w *PromotionWorker) match(sig PromotionSignal, windowCount uint64) (PromotionPolicy, bool) {
	for _, p := range w.Policies {
		if p.MinPieceSizeBytes > 0 && sig.PieceSizeBytes < p.MinPieceSizeBytes {
			continue
		}
		if p.MaxPieceSizeBytes > 0 && sig.PieceSizeBytes > p.MaxPieceSizeBytes {
			continue
		}
		if p.DailyReadCountThreshold > 0 && windowCount < p.DailyReadCountThreshold {
			continue
		}
		return p, true
	}
	return PromotionPolicy{}, false
}

func (w *PromotionWorker) logf(format string, args ...any) {
	if w.Logger == nil {
		return
	}
	w.Logger.Printf(format, args...)
}

// StaticFetcher is a PromotionFetcher that returns the same provider
// for every signal. It is the wiring the gateway uses when there is
// a single origin per cell; richer fetchers can pick between primary
// and secondary backends on the manifest.
type StaticFetcher struct {
	Provider providers.StorageProvider
}

// Fetch returns the static provider.
func (s StaticFetcher) Fetch(_ context.Context, _ PromotionSignal) (providers.StorageProvider, error) {
	return s.Provider, nil
}

// SignalPublisher is the narrow interface the S3 handler uses to
// emit PromotionSignals on the read path. *SignalBus satisfies it.
type SignalPublisher interface {
	Publish(sig PromotionSignal) bool
}

// SignalBus is a thread-safe fan-in for PromotionSignal producers.
// The gateway hands a SignalBus to any code path that observes a
// cache miss; the bus forwards to the buffered channel the
// PromotionWorker drains.
type SignalBus struct {
	ch chan PromotionSignal

	mu     sync.RWMutex
	closed bool
}

// NewSignalBus returns a bus buffered to bufferSize. When the buffer
// is full Publish drops the signal rather than blocking the hot
// read path.
func NewSignalBus(bufferSize int) *SignalBus {
	if bufferSize <= 0 {
		bufferSize = 1024
	}
	return &SignalBus{ch: make(chan PromotionSignal, bufferSize)}
}

// Publish enqueues a signal for the worker. It returns true if the
// signal was accepted and false if the buffer was full or the bus
// was closed.
func (b *SignalBus) Publish(sig PromotionSignal) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return false
	}
	select {
	case b.ch <- sig:
		return true
	default:
		return false
	}
}

// Channel returns the receive-only side of the bus for consumers.
func (b *SignalBus) Channel() <-chan PromotionSignal { return b.ch }

// Close idempotently closes the bus; after Close, Publish returns
// false and Channel drains to completion.
func (b *SignalBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	close(b.ch)
}
