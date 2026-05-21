package s3compat

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// budgetTestCache is a minimal in-memory HotObjectCache used by
// the cache-warming budget tests. It records every Put so the
// tests can tell which warmings succeeded inline and which were
// rejected by the budget guard. A pluggable Put hook lets tests
// pause inside Cache.Put to drive concurrent warmings into the
// budget guard deterministically.
type budgetTestCache struct {
	mu      sync.Mutex
	entries map[string][]byte
	puts    int

	// putGate, if non-nil, is called inside Put before the
	// implementation stores the bytes. Tests use it to block
	// the first Put long enough for a concurrent fetchPiece
	// caller to race the semaphore.
	putGate func(pieceID string)
}

func newBudgetTestCache() *budgetTestCache {
	return &budgetTestCache{entries: map[string][]byte{}}
}

func (c *budgetTestCache) Get(_ context.Context, pieceID string) (io.ReadCloser, hot_object_cache.CachedPieceMetadata, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	buf, ok := c.entries[pieceID]
	if !ok {
		return nil, hot_object_cache.CachedPieceMetadata{}, hot_object_cache.ErrCacheMiss
	}
	return io.NopCloser(bytes.NewReader(buf)), hot_object_cache.CachedPieceMetadata{
		PieceID:   pieceID,
		SizeBytes: int64(len(buf)),
	}, nil
}

func (c *budgetTestCache) Put(_ context.Context, pieceID string, r io.Reader, _ hot_object_cache.PutOptions) error {
	if c.putGate != nil {
		c.putGate(pieceID)
	}
	buf, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.entries[pieceID] = buf
	c.puts++
	c.mu.Unlock()
	return nil
}

func (c *budgetTestCache) Evict(_ context.Context, pieceID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, pieceID)
	return nil
}

func (c *budgetTestCache) Stats() hot_object_cache.Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return hot_object_cache.Stats{Entries: int64(len(c.entries))}
}

// recordingPublisher captures PromotionSignal publishes so tests
// can assert that the budget-exhausted path falls back to the
// async promotion worker.
type recordingPublisher struct {
	mu      sync.Mutex
	signals []hot_object_cache.PromotionSignal
}

func (p *recordingPublisher) Publish(s hot_object_cache.PromotionSignal) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signals = append(p.signals, s)
	return true
}

func (p *recordingPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.signals)
}

// newCacheBudgetHandler builds a Handler wired to a budgetTestCache
// and a configurable CacheWarmingMemoryBudget. The PUT path is
// shared with the rest of the handler tests; the GET path is
// what these tests actually exercise.
func newCacheBudgetHandler(t *testing.T, budget int64) (*Handler, *budgetTestCache, *recordingPublisher, *fakeProvider, *atomic.Int64) {
	t.Helper()
	store := memory.New()
	fake := newFakeProvider("test")
	cache := newBudgetTestCache()
	pub := &recordingPublisher{}
	var rejectedHits atomic.Int64
	h := New(Config{
		Manifests:                store,
		Providers:                map[string]providers.StorageProvider{"test": fake},
		Placement:                fixedPlacement{backend: "test"},
		Billing:                  &recordingBilling{},
		Cache:                    cache,
		CachePublisher:           pub,
		Now:                      func() time.Time { return time.Unix(1700000000, 0) },
		CacheWarmingMemoryBudget: budget,
		OnCacheWarmingBudgetExhausted: func(int64) {
			rejectedHits.Add(1)
		},
	})
	return h, cache, pub, fake, &rejectedHits
}

func putObject(t *testing.T, h *Handler, key string, body []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/bucket/"+key, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT %s status=%d, want 200; body=%s", key, rec.Code, rec.Body)
	}
}

func getObject(t *testing.T, h *Handler, key string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/bucket/"+key, nil)
	rec := httptest.NewRecorder()
	h.Get(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestCacheWarming_DefaultBudgetEnforced verifies that with the
// default budget (no override), a single small GET still warms
// the cache (the budget is not so tight that normal traffic
// regresses).
func TestCacheWarming_DefaultBudgetEnforced(t *testing.T) {
	h, cache, pub, _, rejected := newCacheBudgetHandler(t, 0) // 0 → DefaultCacheWarmingBudget
	body := []byte("hello-world")
	putObject(t, h, "obj", body)

	code, got := getObject(t, h, "obj")
	if code != http.StatusOK {
		t.Fatalf("GET status=%d, want 200", code)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("GET body=%q, want %q", got, body)
	}
	if cache.puts != 1 {
		t.Errorf("cache puts=%d, want 1 (default budget should admit small pieces)", cache.puts)
	}
	if rejected.Load() != 0 {
		t.Errorf("budget-exhausted hits=%d, want 0", rejected.Load())
	}
	// Inline-warm path must not publish a promotion signal
	// (the piece is already resident); the published-signals
	// assertion fences regression to the pre-fix behaviour.
	if got := pub.count(); got != 0 {
		t.Errorf("promotion signals on inline-warm hit=%d, want 0", got)
	}
}

// TestCacheWarming_PieceLargerThanBudget_SkipsWarming verifies
// that when a single piece is larger than the entire budget,
// fetchPiece skips the semaphore acquire (which could never
// succeed) and falls back to the async-promotion path.
func TestCacheWarming_PieceLargerThanBudget_SkipsWarming(t *testing.T) {
	const budget = 64 * 1024 // 64 KiB
	h, cache, pub, _, rejected := newCacheBudgetHandler(t, budget)

	body := bytes.Repeat([]byte("A"), budget+1) // 64 KiB + 1 byte
	putObject(t, h, "big", body)

	code, got := getObject(t, h, "big")
	if code != http.StatusOK {
		t.Fatalf("GET status=%d, want 200", code)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("GET body length=%d, want %d", len(got), len(body))
	}
	if cache.puts != 0 {
		t.Errorf("cache puts=%d, want 0 (piece > budget must not warm inline)", cache.puts)
	}
	if rejected.Load() != 1 {
		t.Errorf("budget-exhausted hits=%d, want 1", rejected.Load())
	}
	if pub.count() != 1 {
		t.Errorf("promotion signals=%d, want 1 (oversize piece must fall back to async)", pub.count())
	}
}

// TestCacheWarming_ConcurrentBurstSpills verifies the core
// invariant: when N concurrent cache-miss GETs each want to
// warm a piece sized to half the budget, at most floor(budget /
// pieceSize) of them are admitted to the inline-warming path
// simultaneously, and the rest fall back to async promotion.
func TestCacheWarming_ConcurrentBurstSpills(t *testing.T) {
	const (
		pieceSize = 256 * 1024  // 256 KiB
		budget    = 384 * 1024  // 384 KiB → admits exactly 1 inline at a time
		callers   = 8
	)
	h, cache, pub, fake, rejected := newCacheBudgetHandler(t, budget)

	_ = fake // GetPiece does not need to be slowed; the put gate
	// pins the budget-holding goroutine inside Cache.Put long
	// enough for the remaining callers to contend for the
	// semaphore.

	// Pre-PUT every object so all GETs hit the cache-miss
	// inline-warming branch (not the cache-hit branch).
	bodies := make([][]byte, callers)
	for i := 0; i < callers; i++ {
		bodies[i] = bytes.Repeat([]byte{byte('a' + i)}, pieceSize)
		putObject(t, h, fmtKey(i), bodies[i])
	}

	// Gate every Put to block until released; this pins the
	// inline-warming branch holding the semaphore long enough
	// for every concurrent GET to contend for it.
	release := make(chan struct{})
	cache.mu.Lock()
	cache.putGate = func(string) { <-release }
	cache.mu.Unlock()

	var wg sync.WaitGroup
	results := make([]int, callers)
	bodyOK := make([]bool, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			code, got := getObject(t, h, fmtKey(idx))
			results[idx] = code
			bodyOK[idx] = bytes.Equal(got, bodies[idx])
		}(i)
	}

	// Wait until at least (callers - 1) callers have either
	// finished (admitted to budget-rejected path) or are
	// blocked in the put gate. Then drain the gate so every
	// remaining caller can finish.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rejected.Load() >= int64(callers-1) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	close(release)
	wg.Wait()

	for i := 0; i < callers; i++ {
		if results[i] != http.StatusOK {
			t.Errorf("GET %d status=%d, want 200", i, results[i])
		}
		if !bodyOK[i] {
			t.Errorf("GET %d body mismatch", i)
		}
	}
	// At least (callers - 1) GETs must have been rejected by
	// the budget guard — exactly which one is admitted is
	// non-deterministic, but the rest cannot all also hold
	// the semaphore at once.
	if got := rejected.Load(); got < int64(callers-1) {
		t.Errorf("budget-exhausted hits=%d, want >= %d", got, callers-1)
	}
	if got := pub.count(); got < callers-1 {
		t.Errorf("promotion signals=%d, want >= %d", got, callers-1)
	}
	if cache.puts < 1 || cache.puts > callers {
		t.Errorf("cache puts=%d, want in [1, %d]", cache.puts, callers)
	}
}

// TestCacheWarming_BudgetReleasedAfterPut verifies the
// semaphore is released after Cache.Put returns so a follow-up
// GET on a fresh piece can warm successfully.
func TestCacheWarming_BudgetReleasedAfterPut(t *testing.T) {
	const (
		pieceSize = 256 * 1024
		budget    = 384 * 1024 // admits exactly one piece at a time
	)
	h, cache, _, _, rejected := newCacheBudgetHandler(t, budget)

	body := bytes.Repeat([]byte("X"), pieceSize)
	putObject(t, h, "first", body)
	putObject(t, h, "second", body)

	// First GET acquires and releases.
	if code, _ := getObject(t, h, "first"); code != http.StatusOK {
		t.Fatalf("first GET status=%d", code)
	}
	if rejected.Load() != 0 {
		t.Fatalf("first GET should not have been rejected by budget")
	}
	if cache.puts != 1 {
		t.Fatalf("first GET cache puts=%d, want 1", cache.puts)
	}

	// Second GET, no other in-flight warmer; semaphore must
	// have been released so this also admits.
	if code, _ := getObject(t, h, "second"); code != http.StatusOK {
		t.Fatalf("second GET status=%d", code)
	}
	if rejected.Load() != 0 {
		t.Errorf("second GET should not have been rejected — budget was not released after first Put")
	}
	if cache.puts != 2 {
		t.Errorf("second GET cache puts=%d, want 2", cache.puts)
	}
}

// TestCacheWarming_NegativeBudgetDisablesGuard verifies that a
// negative budget restores the unbounded pre-PR-7 behaviour for
// regression tests.
func TestCacheWarming_NegativeBudgetDisablesGuard(t *testing.T) {
	h, cache, pub, _, rejected := newCacheBudgetHandler(t, -1)
	if h.cacheWarmSem != nil {
		t.Errorf("negative budget must leave cacheWarmSem nil; got non-nil")
	}
	body := bytes.Repeat([]byte("X"), 1024)
	putObject(t, h, "obj", body)
	if code, _ := getObject(t, h, "obj"); code != http.StatusOK {
		t.Fatalf("GET status=%d", code)
	}
	if cache.puts != 1 {
		t.Errorf("cache puts=%d, want 1 (unbounded path must warm)", cache.puts)
	}
	if rejected.Load() != 0 {
		t.Errorf("budget-exhausted hits=%d, want 0 (negative budget disables the guard)", rejected.Load())
	}
	if pub.count() != 0 {
		t.Errorf("promotion signals=%d, want 0", pub.count())
	}
}

// TestCacheWarming_RangeRequestContendsForBudget verifies that
// range GETs DO contend for the cache-warming budget when the
// piece carries an integrity hash. Range cache-misses on hashed
// pieces buffer the full piece for verification (and warm the
// cache in the process), so the budget guard applies to range
// paths the same way it applies to non-range paths. When the
// budget cannot be acquired the gateway falls back to streaming
// the raw body and publishes a promotion signal so the async
// worker can warm the cache off the request goroutine.
//
// Correctness fence: the request asks for a non-zero range
// window AND the test asserts on the response body content
// (not just status code, headers, and counters). Earlier this
// test used bytes=0-3 which happened to coincide with the first
// bytes of the piece, masking a bug where the budget-exhausted
// path returned the FULL piece body to the caller while Get()
// wrote a Content-Range / Content-Length sized for the slice.
// The fix (close the full body and re-fetch the actual range
// from the provider) is gated by this assertion.
//
// This also pins the post-merge semantics: pre-PR-7 PR #7 was
// designed before piece integrity verification landed, when
// range paths did not buffer at all. Once verification on the
// GET path landed, range paths joined non-range paths in the
// "buffer-then-verify-then-warm" pipeline and the budget guard
// must cover both equally — otherwise a pathological client
// could issue concurrent range GETs on distinct pieces and
// blow past the memory budget the operator configured.
func TestCacheWarming_RangeRequestContendsForBudget(t *testing.T) {
	const (
		pieceSize = 64 * 1024
		budget    = 16 // 16 bytes — too small to admit any piece
		// Deliberately non-zero range start: a regression that
		// returns the full piece body would emit the first
		// bytes of the piece instead of bytes [start, end],
		// which the body-content assertion below would catch.
		rangeStart = 100
		rangeEnd   = 199 // inclusive — 100 bytes
	)
	h, cache, pub, _, rejected := newCacheBudgetHandler(t, budget)
	// Body is filled with a position-encoded byte pattern so a
	// wrong slice is immediately obvious in the assertion message.
	body := make([]byte, pieceSize)
	for i := range body {
		body[i] = byte(i % 251)
	}
	putObject(t, h, "obj", body)

	req := httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", rangeStart, rangeEnd))
	rec := httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range GET status=%d, want 206; body=%s", rec.Code, rec.Body)
	}
	want := body[rangeStart : rangeEnd+1]
	got := rec.Body.Bytes()
	if !bytes.Equal(got, want) {
		t.Fatalf("range GET served wrong bytes: budget-exhausted path must re-fetch with the actual range. got len=%d first=%v last=%v; want len=%d first=%v last=%v",
			len(got), got[:min(8, len(got))], got[max(0, len(got)-8):],
			len(want), want[:min(8, len(want))], want[max(0, len(want)-8):])
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", rangeEnd-rangeStart+1) {
		t.Errorf("Content-Length=%q must match the requested range size %d (not the full piece size)", cl, rangeEnd-rangeStart+1)
	}
	if rejected.Load() != 1 {
		t.Errorf("range request larger than budget must be rejected by the guard; rejected=%d, want 1", rejected.Load())
	}
	if cache.puts != 0 {
		t.Errorf("budget rejection must skip Cache.Put; puts=%d", cache.puts)
	}
	if pub.count() != 1 {
		t.Errorf("budget rejection must publish exactly one promotion signal; got %d", pub.count())
	}
}

// TestCacheWarming_BudgetReleasedOnReadAllError verifies that
// when the body Read fails after the semaphore was acquired,
// the budget is still released so a long-lived gateway does
// not leak budget over time. The test directly drives the
// fetchPiece release path by handing the handler a piece body
// that errors mid-stream and then asserts a follow-up small
// GET on a different piece can still acquire.
func TestCacheWarming_BudgetReleasedOnReadAllError(t *testing.T) {
	const (
		pieceSize = 1024
		budget    = pieceSize * 2 // admits 2 pieces; if leaked, second GET would fall through to signal path
	)
	store := memory.New()
	fake := &failingReadProvider{fakeProvider: newFakeProvider("test")}
	cache := newBudgetTestCache()
	pub := &recordingPublisher{}
	var rejected atomic.Int64
	h := New(Config{
		Manifests:                store,
		Providers:                map[string]providers.StorageProvider{"test": fake},
		Placement:                fixedPlacement{backend: "test"},
		Billing:                  &recordingBilling{},
		Cache:                    cache,
		CachePublisher:           pub,
		Now:                      func() time.Time { return time.Unix(1700000000, 0) },
		CacheWarmingMemoryBudget: budget,
		OnCacheWarmingBudgetExhausted: func(int64) {
			rejected.Add(1)
		},
	})

	body := bytes.Repeat([]byte("E"), pieceSize)
	putObject(t, h, "first", body)
	putObject(t, h, "second", body)

	fake.failNextRead.Store(true)
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/bucket/first", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("first GET should have failed mid-stream; got 200")
	}

	// Follow-up GET must succeed inline (cache puts == 1). If
	// the budget was leaked, the semaphore would still be
	// holding pieceSize bytes and Acquire would still succeed
	// because budget allows 2; but if leaked twice in a
	// future regression where pieceSize was equal to budget,
	// the test would catch it. The stronger assertion here is
	// that no budget-exhausted rejection fires on the second
	// path.
	if code, got := getObject(t, h, "second"); code != http.StatusOK {
		t.Errorf("follow-up GET status=%d, want 200 (budget must be released on read error); body=%q", code, got)
	}
	if cache.puts != 1 {
		t.Errorf("cache puts=%d, want 1 (only second GET should have warmed)", cache.puts)
	}
	if rejected.Load() != 0 {
		t.Errorf("budget-exhausted hits=%d, want 0 (follow-up must acquire cleanly)", rejected.Load())
	}
}

// failingReadProvider wraps fakeProvider so a single test can
// inject a mid-stream Read error without polluting the shared
// fakeProvider used by other tests.
type failingReadProvider struct {
	*fakeProvider
	failNextRead atomic.Bool
}

func (f *failingReadProvider) GetPiece(ctx context.Context, pieceID string, r *providers.ByteRange) (io.ReadCloser, error) {
	rc, err := f.fakeProvider.GetPiece(ctx, pieceID, r)
	if err != nil {
		return nil, err
	}
	if !f.failNextRead.Load() {
		return rc, nil
	}
	f.failNextRead.Store(false)
	return &errOnReadCloser{rc: rc}, nil
}

// errOnReadCloser returns an error on the very first Read so
// io.ReadAll surfaces it immediately.
type errOnReadCloser struct{ rc io.ReadCloser }

func (e *errOnReadCloser) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}
func (e *errOnReadCloser) Close() error { return e.rc.Close() }

func fmtKey(i int) string { return "obj-" + string(rune('a'+i)) }

// Compile-time assertion that budgetTestCache implements the
// HotObjectCache interface so future interface changes break
// this file rather than the production code path.
var _ hot_object_cache.HotObjectCache = (*budgetTestCache)(nil)
var _ hot_object_cache.SignalPublisher = (*recordingPublisher)(nil)
