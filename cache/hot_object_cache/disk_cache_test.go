package hot_object_cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiskCache_PutGetRoundtrip(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCache(DiskCacheConfig{RootPath: dir, Policy: DefaultEvictionPolicy(1 << 20)})
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	body := []byte("hello disk cache")
	if err := c.Put(context.Background(), "pieceA", bytes.NewReader(body), PutOptions{SizeBytes: int64(len(body)), Hash: "hashA"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, md, err := c.Get(context.Background(), "pieceA")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body mismatch: got %q want %q", got, body)
	}
	if md.SizeBytes != int64(len(body)) {
		t.Fatalf("SizeBytes = %d, want %d", md.SizeBytes, len(body))
	}
	if md.Hash != "hashA" {
		t.Fatalf("Hash = %q, want %q", md.Hash, "hashA")
	}
	if md.HitCount != 1 {
		t.Fatalf("HitCount = %d, want 1", md.HitCount)
	}
}

func TestDiskCache_MissReturnsErrCacheMiss(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCache(DiskCacheConfig{RootPath: dir, Policy: DefaultEvictionPolicy(1 << 20)})
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	_, _, err = c.Get(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("Get miss: err = %v, want ErrCacheMiss", err)
	}
}

func TestDiskCache_EvictRemovesFiles(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCache(DiskCacheConfig{RootPath: dir, Policy: DefaultEvictionPolicy(1 << 20)})
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	body := []byte("evict-me")
	if err := c.Put(context.Background(), "p1", bytes.NewReader(body), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Evict(context.Background(), "p1"); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	// Both body and sidecar should be gone from disk.
	walkCount := 0
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".bin") || strings.HasSuffix(path, ".meta.json") {
			walkCount++
		}
		return nil
	})
	if walkCount != 0 {
		t.Fatalf("disk files remain after Evict: %d", walkCount)
	}
	if _, _, err := c.Get(context.Background(), "p1"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("Get after Evict: err = %v, want ErrCacheMiss", err)
	}
}

func TestDiskCache_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	policy := DefaultEvictionPolicy(1 << 20)
	first, err := NewDiskCache(DiskCacheConfig{RootPath: dir, Policy: policy})
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	body := []byte("persisted body")
	if err := first.Put(context.Background(), "persist-1", bytes.NewReader(body), PutOptions{Hash: "h1", PinHot: true}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Drop the first cache and re-open against the same directory.
	second, err := NewDiskCache(DiskCacheConfig{RootPath: dir, Policy: policy})
	if err != nil {
		t.Fatalf("re-open NewDiskCache: %v", err)
	}
	rc, md, err := second.Get(context.Background(), "persist-1")
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body mismatch after restart: got %q want %q", got, body)
	}
	if md.Hash != "h1" {
		t.Fatalf("Hash = %q, want %q", md.Hash, "h1")
	}
	if !md.Pinned {
		t.Fatalf("pin lost across restart")
	}
}

func TestDiskCache_TTLExpiry(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Unix(0, 0)}
	c, err := NewDiskCache(DiskCacheConfig{
		RootPath: dir,
		Policy:   DefaultEvictionPolicy(1 << 20),
		Clock:    clock.Now,
	})
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	if err := c.Put(context.Background(), "ttl-1", bytes.NewReader([]byte("short")), PutOptions{TTL: time.Minute}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	clock.advance(2 * time.Minute)
	if _, _, err := c.Get(context.Background(), "ttl-1"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("Get after TTL: err = %v, want ErrCacheMiss", err)
	}
}

func TestDiskCache_CapacityEviction(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCache(DiskCacheConfig{
		RootPath: dir,
		Policy:   EvictionPolicy{Kind: EvictionLRU, MaxBytes: 6},
	})
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	// Insert three 3-byte pieces; MaxBytes=6, so the first should evict.
	ctx := context.Background()
	for _, id := range []string{"p1", "p2", "p3"} {
		if err := c.Put(ctx, id, bytes.NewReader([]byte("abc")), PutOptions{}); err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
	}
	if _, _, err := c.Get(ctx, "p1"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("p1 should have been evicted; err = %v", err)
	}
	if _, _, err := c.Get(ctx, "p3"); err != nil {
		t.Fatalf("p3 should still be cached: %v", err)
	}
	stats := c.Stats()
	if stats.Evictions == 0 {
		t.Fatalf("expected eviction counter > 0, got %d", stats.Evictions)
	}
}

func TestDiskCache_RejectsOversizePiece(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCache(DiskCacheConfig{
		RootPath: dir,
		Policy:   EvictionPolicy{Kind: EvictionLRU, MaxBytes: 4},
	})
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	err = c.Put(context.Background(), "big", bytes.NewReader([]byte("this-is-too-big")), PutOptions{})
	if err == nil {
		t.Fatalf("Put oversize piece: want error, got nil")
	}
	// On-disk state should not contain a body for "big".
	if _, statErr := os.Stat(filepath.Join(dir, "bi", "big.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("oversize piece left a body on disk: %v", statErr)
	}
}

func TestDiskCache_CleansOrphansOnWarm(t *testing.T) {
	dir := t.TempDir()
	// Seed an orphan body and an orphan sidecar.
	shard := filepath.Join(dir, "or")
	if err := os.MkdirAll(shard, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shard, "orphan-body.bin"), []byte("x"), 0o644); err != nil {
		t.Fatalf("orphan body: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shard, "orphan-meta.meta.json"), []byte(`{"piece_id":"orphan-meta"}`), 0o644); err != nil {
		t.Fatalf("orphan meta: %v", err)
	}
	// Leftover tmp from a crashed write.
	if err := os.WriteFile(filepath.Join(shard, "stale.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatalf("stale tmp: %v", err)
	}

	c, err := NewDiskCache(DiskCacheConfig{RootPath: dir, Policy: DefaultEvictionPolicy(1 << 20)})
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	for _, name := range []string{"orphan-body.bin", "orphan-meta.meta.json", "stale.tmp"} {
		if _, statErr := os.Stat(filepath.Join(shard, name)); !os.IsNotExist(statErr) {
			t.Fatalf("%s should have been cleaned on warm: %v", name, statErr)
		}
	}
	if c.Stats().Entries != 0 {
		t.Fatalf("Entries = %d, want 0", c.Stats().Entries)
	}
}

type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time          { return f.now }
func (f *fakeClock) advance(d time.Duration) { f.now = f.now.Add(d) }
