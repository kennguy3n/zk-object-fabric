package hot_object_cache

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

type fakeProvider struct {
	body []byte
	gets atomic.Int64
}

func (f *fakeProvider) PutPiece(_ context.Context, _ string, _ io.Reader, _ providers.PutOptions) (providers.PutResult, error) {
	return providers.PutResult{SizeBytes: int64(len(f.body))}, nil
}

func (f *fakeProvider) GetPiece(_ context.Context, _ string, _ *providers.ByteRange) (io.ReadCloser, error) {
	f.gets.Add(1)
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

func (f *fakeProvider) HeadPiece(_ context.Context, _ string) (providers.PieceMetadata, error) {
	return providers.PieceMetadata{SizeBytes: int64(len(f.body))}, nil
}

func (f *fakeProvider) DeletePiece(_ context.Context, _ string) error { return nil }

func (f *fakeProvider) ListPieces(_ context.Context, _, _ string) (providers.ListResult, error) {
	return providers.ListResult{}, nil
}

func (f *fakeProvider) Capabilities() providers.ProviderCapabilities {
	return providers.ProviderCapabilities{}
}

func (f *fakeProvider) CostModel() providers.ProviderCostModel { return providers.ProviderCostModel{} }

func (f *fakeProvider) PlacementLabels() providers.PlacementLabels {
	return providers.PlacementLabels{}
}

func TestPromotionWorker_AggregatesReadCountAcrossSignals(t *testing.T) {
	body := []byte("ciphertext-blob")
	fp := &fakeProvider{body: body}
	cache, err := NewMemoryCache(EvictionPolicy{Kind: EvictionLRU, MaxBytes: 1024})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	worker := &PromotionWorker{
		Cache:    cache,
		Policies: []PromotionPolicy{{Tier: TierL0, DailyReadCountThreshold: 3, MinPieceSizeBytes: 1}},
		Fetcher:  StaticFetcher{Provider: fp},
	}

	now := time.Unix(1700000000, 0)
	sig := PromotionSignal{
		PieceID:        "p1",
		PieceSizeBytes: int64(len(body)),
		ReadBytes:      5,
		ReadCount:      1,
		ObservedAt:     now,
	}
	ctx := context.Background()

	worker.handle(ctx, sig)
	worker.handle(ctx, sig)
	if fp.gets.Load() != 0 {
		t.Fatalf("provider fetched before threshold: %d", fp.gets.Load())
	}
	if _, _, err := cache.Get(ctx, "p1"); err == nil {
		t.Fatalf("cache populated before threshold")
	}

	worker.handle(ctx, sig)
	if fp.gets.Load() != 1 {
		t.Fatalf("provider fetch count after threshold = %d, want 1", fp.gets.Load())
	}
	rc, _, err := cache.Get(ctx, "p1")
	if err != nil {
		t.Fatalf("cache miss after promotion: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Fatalf("cached body = %q, want %q", got, body)
	}
}

func TestPromotionWorker_ChecksPieceSizeNotReadBytes(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 128*1024) // 128 KiB piece
	fp := &fakeProvider{body: body}
	cache, err := NewMemoryCache(EvictionPolicy{Kind: EvictionLRU, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	worker := &PromotionWorker{
		Cache:    cache,
		Policies: []PromotionPolicy{{Tier: TierL0, MinPieceSizeBytes: 64 * 1024}},
		Fetcher:  StaticFetcher{Provider: fp},
	}

	// Small range read on a large piece: ReadBytes < MinPieceSizeBytes
	// but the full piece clears it.
	worker.handle(context.Background(), PromotionSignal{
		PieceID:        "p1",
		PieceSizeBytes: int64(len(body)),
		ReadBytes:      100,
		ObservedAt:     time.Unix(1700000000, 0),
	})
	if fp.gets.Load() != 1 {
		t.Fatalf("range read on large piece did not promote (gets=%d)", fp.gets.Load())
	}
}

func TestPromotionWorker_SkipsSmallPieces(t *testing.T) {
	fp := &fakeProvider{body: []byte("tiny")}
	cache, _ := NewMemoryCache(EvictionPolicy{Kind: EvictionLRU, MaxBytes: 1024})
	worker := &PromotionWorker{
		Cache:    cache,
		Policies: []PromotionPolicy{{Tier: TierL0, MinPieceSizeBytes: 1024}},
		Fetcher:  StaticFetcher{Provider: fp},
	}
	worker.handle(context.Background(), PromotionSignal{
		PieceID:        "p1",
		PieceSizeBytes: 16,
		ObservedAt:     time.Unix(1700000000, 0),
	})
	if fp.gets.Load() != 0 {
		t.Fatalf("undersized piece promoted: gets=%d", fp.gets.Load())
	}
}

func TestReadCounter_ResetsAfterWindow(t *testing.T) {
	c := newReadCounter(time.Hour)
	start := time.Unix(1700000000, 0)
	if got := c.observe("p", 1, start); got != 1 {
		t.Fatalf("first observe = %d, want 1", got)
	}
	if got := c.observe("p", 1, start.Add(30*time.Minute)); got != 2 {
		t.Fatalf("second observe inside window = %d, want 2", got)
	}
	if got := c.observe("p", 1, start.Add(2*time.Hour)); got != 1 {
		t.Fatalf("observe after window = %d, want 1 (reset)", got)
	}
}
