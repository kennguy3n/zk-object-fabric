package hot_object_cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestMemoryCache_PutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	c, err := NewMemoryCache(EvictionPolicy{Kind: EvictionLRU, MaxBytes: 1024})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	if err := c.Put(ctx, "piece-1", bytes.NewReader([]byte("hello")), PutOptions{Hash: "h1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, md, err := c.Get(ctx, "piece-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "hello" {
		t.Fatalf("body = %q, want hello", got)
	}
	if md.Hash != "h1" || md.SizeBytes != 5 {
		t.Fatalf("metadata mismatch: %+v", md)
	}
	s := c.Stats()
	if s.Hits != 1 || s.Misses != 0 {
		t.Fatalf("stats hits/misses = %d/%d, want 1/0", s.Hits, s.Misses)
	}
}

func TestMemoryCache_Miss(t *testing.T) {
	ctx := context.Background()
	c, err := NewMemoryCache(EvictionPolicy{Kind: EvictionLRU, MaxBytes: 1024})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	if _, _, err := c.Get(ctx, "missing"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("Get missing: want ErrCacheMiss, got %v", err)
	}
}

func TestMemoryCache_EvictsLRU(t *testing.T) {
	ctx := context.Background()
	c, err := NewMemoryCache(EvictionPolicy{Kind: EvictionLRU, MaxBytes: 10})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := c.Put(ctx, id, bytes.NewReader([]byte("1234")), PutOptions{}); err != nil {
			t.Fatalf("Put %q: %v", id, err)
		}
	}
	// First inserted must have been evicted.
	if _, _, err := c.Get(ctx, "a"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("Get a after eviction: want miss, got %v", err)
	}
	// Most recent stays.
	if _, _, err := c.Get(ctx, "c"); err != nil {
		t.Fatalf("Get c: %v", err)
	}
}

func TestMemoryCache_HotPinSurvivesLRU(t *testing.T) {
	ctx := context.Background()
	c, err := NewMemoryCache(EvictionPolicy{
		Kind:              EvictionLRUHotPin,
		MaxBytes:          20,
		HotRegionFraction: 0.5,
	})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	if err := c.Put(ctx, "hot", bytes.NewReader([]byte("1234")), PutOptions{PinHot: true}); err != nil {
		t.Fatalf("Put hot: %v", err)
	}
	for _, id := range []string{"cold1", "cold2", "cold3", "cold4"} {
		if err := c.Put(ctx, id, bytes.NewReader([]byte("1234")), PutOptions{}); err != nil {
			t.Fatalf("Put cold: %v", err)
		}
	}
	if _, _, err := c.Get(ctx, "hot"); err != nil {
		t.Fatalf("hot-pinned entry missing: %v", err)
	}
}

func TestMemoryCache_Evict(t *testing.T) {
	ctx := context.Background()
	c, err := NewMemoryCache(EvictionPolicy{Kind: EvictionLRU, MaxBytes: 1024})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	_ = c.Put(ctx, "x", bytes.NewReader([]byte("abc")), PutOptions{})
	if err := c.Evict(ctx, "x"); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, _, err := c.Get(ctx, "x"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("Get after Evict: want miss, got %v", err)
	}
	if err := c.Evict(ctx, "missing"); err != nil {
		t.Fatalf("Evict missing: %v (expected idempotent)", err)
	}
}

func TestMemoryCache_PutOverCapacity(t *testing.T) {
	ctx := context.Background()
	c, err := NewMemoryCache(EvictionPolicy{Kind: EvictionLRU, MaxBytes: 4})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	if err := c.Put(ctx, "big", bytes.NewReader([]byte("12345")), PutOptions{}); err == nil {
		t.Fatal("Put oversized: want error, got nil")
	}
}

func TestSignalBus_PublishAfterCloseIsFalse(t *testing.T) {
	bus := NewSignalBus(1)
	bus.Close()
	if bus.Publish(PromotionSignal{PieceID: "x"}) {
		t.Fatal("Publish after Close: want false, got true")
	}
}

func TestSignalBus_DropsOnFullBuffer(t *testing.T) {
	bus := NewSignalBus(1)
	defer bus.Close()
	if !bus.Publish(PromotionSignal{PieceID: "a"}) {
		t.Fatal("first Publish: want true")
	}
	if bus.Publish(PromotionSignal{PieceID: "b"}) {
		t.Fatal("second Publish on full buffer: want false, got true")
	}
}
