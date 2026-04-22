package hot_object_cache

import (
	"context"
	"log"
	"sync"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

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

	// Logger is optional. Nil disables internal logging.
	Logger *log.Logger
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
	pol, ok := w.match(sig)
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
		SizeBytes: sig.ReadBytes,
		PinHot:    pol.PinHotByDefault,
	}); err != nil {
		w.logf("promotion: cache put %s: %v", sig.PieceID, err)
	}
}

// match picks the first policy whose thresholds sig crosses. A
// missing policy means no promotion.
func (w *PromotionWorker) match(sig PromotionSignal) (PromotionPolicy, bool) {
	for _, p := range w.Policies {
		if p.MinPieceSizeBytes > 0 && sig.ReadBytes < p.MinPieceSizeBytes {
			continue
		}
		if p.MaxPieceSizeBytes > 0 && sig.ReadBytes > p.MaxPieceSizeBytes {
			continue
		}
		if p.DailyReadCountThreshold > 0 && sig.ReadCount < p.DailyReadCountThreshold {
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
