package s3compat

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
	"github.com/kennguy3n/zk-object-fabric/internal/requestid"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// fakeProvider is a minimal providers.StorageProvider backed by a
// map.
type fakeProvider struct {
	mu      sync.Mutex
	pieces  map[string][]byte
	etag    string
	backend string
}

func newFakeProvider(backend string) *fakeProvider {
	return &fakeProvider{pieces: map[string][]byte{}, etag: "etag-xyz", backend: backend}
}

func (f *fakeProvider) PutPiece(_ context.Context, pieceID string, r io.Reader, _ providers.PutOptions) (providers.PutResult, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return providers.PutResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pieces[pieceID] = b
	return providers.PutResult{
		PieceID:   pieceID,
		ETag:      f.etag,
		SizeBytes: int64(len(b)),
		Backend:   f.backend,
		Locator:   fmt.Sprintf("fake://%s/%s", f.backend, pieceID),
	}, nil
}
func (f *fakeProvider) GetPiece(_ context.Context, pieceID string, r *providers.ByteRange) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.pieces[pieceID]
	if !ok {
		return nil, errors.New("not found")
	}
	if r != nil {
		end := r.End
		if end < 0 || end >= int64(len(b)) {
			end = int64(len(b)) - 1
		}
		b = b[r.Start : end+1]
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (f *fakeProvider) HeadPiece(context.Context, string) (providers.PieceMetadata, error) {
	return providers.PieceMetadata{}, nil
}
func (f *fakeProvider) DeletePiece(_ context.Context, pieceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pieces, pieceID)
	return nil
}
func (f *fakeProvider) ListPieces(context.Context, string, string) (providers.ListResult, error) {
	return providers.ListResult{}, nil
}
func (f *fakeProvider) Capabilities() providers.ProviderCapabilities { return providers.ProviderCapabilities{} }
func (f *fakeProvider) CostModel() providers.ProviderCostModel       { return providers.ProviderCostModel{} }
func (f *fakeProvider) PlacementLabels() providers.PlacementLabels   { return providers.PlacementLabels{} }

// fixedPlacement returns a fixed backend for every call.
type fixedPlacement struct{ backend string }

func (f fixedPlacement) ResolveBackend(string, string, string) (string, metadata.PlacementPolicy, error) {
	return f.backend, metadata.PlacementPolicy{AllowedBackends: []string{f.backend}}, nil
}

type recordingBilling struct {
	mu     sync.Mutex
	events []billing.UsageEvent
}

func (r *recordingBilling) Emit(event billing.UsageEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}
func (r *recordingBilling) count(d billing.Dimension) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Dimension == d {
			n++
		}
	}
	return n
}

func newTestHandler() (*Handler, *fakeProvider, *recordingBilling, manifest_store.ManifestStore) {
	store := memory.New()
	fake := newFakeProvider("test")
	bill := &recordingBilling{}
	h := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"test": fake},
		Placement: fixedPlacement{backend: "test"},
		Billing:   bill,
		Now:       func() time.Time { return time.Unix(1700000000, 0) },
	})
	return h, fake, bill, store
}

func TestPutGetHeadDelete_Roundtrip(t *testing.T) {
	h, fake, bill, _ := newTestHandler()
	body := []byte("hello world")

	// PUT
	req := httptest.NewRequest(http.MethodPut, "/bucket/path/to/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if etag := rec.Header().Get("ETag"); etag == "" || !strings.HasPrefix(etag, `"`) {
		t.Errorf("PUT ETag = %q, want quoted", etag)
	}
	versionID := rec.Header().Get("x-amz-version-id")
	if versionID == "" {
		t.Error("PUT missing x-amz-version-id")
	}
	if len(fake.pieces) != 1 {
		t.Errorf("fake pieces = %d, want 1", len(fake.pieces))
	}

	// GET
	req = httptest.NewRequest(http.MethodGet, "/bucket/path/to/obj", nil)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != string(body) {
		t.Errorf("GET body = %q, want %q", got, body)
	}

	// HEAD
	req = httptest.NewRequest(http.MethodHead, "/bucket/path/to/obj", nil)
	rec = httptest.NewRecorder()
	h.Head(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", rec.Code)
	}

	// DELETE
	req = httptest.NewRequest(http.MethodDelete, "/bucket/path/to/obj", nil)
	rec = httptest.NewRecorder()
	h.Delete(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", rec.Code)
	}
	if len(fake.pieces) != 0 {
		t.Errorf("fake pieces after delete = %d, want 0", len(fake.pieces))
	}

	// GET after delete
	req = httptest.NewRequest(http.MethodGet, "/bucket/path/to/obj", nil)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE status = %d, want 404", rec.Code)
	}

	if bill.count(billing.PutRequests) != 1 {
		t.Errorf("put_requests billing count = %d, want 1", bill.count(billing.PutRequests))
	}
	if bill.count(billing.GetRequests) < 2 {
		t.Errorf("get_requests billing count = %d, want >=2 (GET + HEAD)", bill.count(billing.GetRequests))
	}
	if bill.count(billing.DeleteRequests) != 1 {
		t.Errorf("delete_requests billing count = %d, want 1", bill.count(billing.DeleteRequests))
	}
	if bill.count(billing.OriginEgressBytes) == 0 {
		t.Error("origin_egress_bytes not emitted on GET")
	}
}

func TestGet_RangeRequest(t *testing.T) {
	h, _, _, _ := newTestHandler()
	body := []byte("0123456789")

	req := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	h.Put(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("GET status = %d, want 206; body=%s", rec.Code, rec.Body)
	}
	if rec.Body.String() != "2345" {
		t.Errorf("GET range body = %q, want %q", rec.Body.String(), "2345")
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 2-5/10")
	}
}

func TestGet_OpenEndedRange(t *testing.T) {
	h, _, _, _ := newTestHandler()
	body := []byte("0123456789")
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	h.Put(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
	req.Header.Set("Range", "bytes=5-")
	rec := httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("GET status = %d, want 206; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Length"); got != "5" {
		t.Errorf("open-ended range Content-Length = %q, want %q", got, "5")
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 5-9/10" {
		t.Errorf("open-ended range Content-Range = %q, want %q", got, "bytes 5-9/10")
	}
	if rec.Body.String() != "56789" {
		t.Errorf("open-ended range body = %q, want %q", rec.Body.String(), "56789")
	}
}

// TestHead_RangeRequest_Returns206WithSliceContentLength pins
// RFC 9110 §13.1 conformance: a HEAD on a target that the client
// would GET with Range must report the same metadata a GET would
// — 206 Partial Content, Content-Range, and Content-Length sized
// to the slice rather than the full object.
//
// Pre-fix the handler returned 200 OK with the full Content-Length
// for every HEAD regardless of Range, which silently broke AWS
// SDK / CDN pre-flight probes that size their download buffers
// off the HEAD response.
func TestHead_RangeRequest_Returns206WithSliceContentLength(t *testing.T) {
	h, _, _, _ := newTestHandler()
	body := []byte("0123456789")
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	h.Put(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodHead, "/bucket/obj", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	h.Head(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("HEAD with Range status = %d, want 206; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Length"); got != "4" {
		t.Errorf("HEAD with Range Content-Length = %q, want %q (slice size 2..5 inclusive)", got, "4")
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Errorf("HEAD with Range Content-Range = %q, want %q", got, "bytes 2-5/10")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD with Range body length = %d, want 0 (HEAD has no message body)", rec.Body.Len())
	}
}

// TestHead_OpenEndedRange pins the open-ended Range form ("bytes=N-")
// against HEAD. The 416 InvalidRange path is exercised separately;
// this case asserts the happy path for the most common CDN pre-flight
// shape: "give me the suffix starting at N".
func TestHead_OpenEndedRange(t *testing.T) {
	h, _, _, _ := newTestHandler()
	body := []byte("0123456789")
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	h.Put(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodHead, "/bucket/obj", nil)
	req.Header.Set("Range", "bytes=5-")
	rec := httptest.NewRecorder()
	h.Head(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("HEAD open-ended range status = %d, want 206; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Length"); got != "5" {
		t.Errorf("HEAD open-ended range Content-Length = %q, want %q", got, "5")
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 5-9/10" {
		t.Errorf("HEAD open-ended range Content-Range = %q, want %q", got, "bytes 5-9/10")
	}
}

// TestHead_InvalidRange_Returns416 pins the contract that an
// out-of-bounds or malformed Range on HEAD surfaces as
// 416 RequestedRangeNotSatisfiable, mirroring the GET path.
// Returning 200 + full Content-Length here would let a buggy
// client follow up with a ranged GET that we then reject, wasting
// a round-trip.
func TestHead_InvalidRange_Returns416(t *testing.T) {
	h, _, _, _ := newTestHandler()
	body := []byte("0123456789")
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	h.Put(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodHead, "/bucket/obj", nil)
	req.Header.Set("Range", "bytes=100-200")
	rec := httptest.NewRecorder()
	h.Head(rec, req)

	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("HEAD with out-of-bounds Range status = %d, want 416", rec.Code)
	}
}

// TestHead_NoRange_Returns200WithFullContentLength is the
// regression test for the original non-Range HEAD shape: 200 OK
// with the full ObjectSize as Content-Length. Easy to break if a
// future refactor accidentally drops the no-Range branch.
func TestHead_NoRange_Returns200WithFullContentLength(t *testing.T) {
	h, _, _, _ := newTestHandler()
	body := []byte("0123456789")
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	h.Put(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodHead, "/bucket/obj", nil)
	rec := httptest.NewRecorder()
	h.Head(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Length"); got != "10" {
		t.Errorf("HEAD Content-Length = %q, want %q", got, "10")
	}
	if got := rec.Header().Get("Content-Range"); got != "" {
		t.Errorf("HEAD with no Range must NOT set Content-Range, got %q", got)
	}
}

// TestHead_EmitsAuditRecord pins the compliance contract that HEAD
// — like GET, PUT, and DELETE — emits an AuditEntry through the
// configured AuditRecorder. In zero-knowledge deployments a HEAD
// reveals object existence and metadata, which is itself a
// privacy-sensitive read even though no bytes are served. The
// audit shape mirrors GET (operation="HEAD", same tenant / bucket
// / key / pieceID / backend / country fields).
func TestHead_EmitsAuditRecord(t *testing.T) {
	store := memory.New()
	fp := newFakeProvider("test")
	provWithCountry := &fakeProviderWithCountry{fakeProvider: fp, country: "US"}
	audit := &recordingAudit{}
	h := New(Config{
		Manifests:  store,
		Providers:  map[string]providers.StorageProvider{"test": provWithCountry},
		Placement:  fixedPlacement{backend: "test"},
		Now:        func() time.Time { return time.Unix(1700000000, 0) },
		Compliance: ComplianceHooks{Audit: audit},
	})

	body := []byte("hello-head")
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	h.Put(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodHead, "/bucket/obj", nil)
	rec := httptest.NewRecorder()
	h.Head(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	ops := audit.operations()
	if len(ops) != 2 || ops[0] != "PUT" || ops[1] != "HEAD" {
		t.Fatalf("audit ops = %v, want [PUT HEAD]", ops)
	}
	headEntry := audit.entries[1]
	if headEntry.PieceBackend != "test" {
		t.Errorf("HEAD audit backend = %q, want %q", headEntry.PieceBackend, "test")
	}
	if headEntry.BackendCountry != "US" {
		t.Errorf("HEAD audit country = %q, want %q", headEntry.BackendCountry, "US")
	}
	if headEntry.Bucket != "bucket" || headEntry.ObjectKey != "obj" {
		t.Errorf("HEAD audit (bucket, key) = (%q, %q), want (bucket, obj)", headEntry.Bucket, headEntry.ObjectKey)
	}
}

// recordingHotCache is a minimal HotObjectCache implementation that
// tracks Put calls so tests can assert when fetchPiece warms the
// cache. Get is intentionally a permanent miss: every test that
// uses this cache wants to exercise the cache-miss path so it can
// observe whether the warm happened.
type recordingHotCache struct {
	mu  sync.Mutex
	put map[string]int
}

func (c *recordingHotCache) Get(_ context.Context, _ string) (io.ReadCloser, hot_object_cache.CachedPieceMetadata, error) {
	return nil, hot_object_cache.CachedPieceMetadata{}, hot_object_cache.ErrCacheMiss
}

func (c *recordingHotCache) Put(_ context.Context, pieceID string, r io.Reader, _ hot_object_cache.PutOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.put == nil {
		c.put = map[string]int{}
	}
	c.put[pieceID]++
	_, _ = io.Copy(io.Discard, r)
	return nil
}

func (c *recordingHotCache) Evict(_ context.Context, _ string) error {
	return nil
}

func (c *recordingHotCache) Stats() hot_object_cache.Stats {
	return hot_object_cache.Stats{}
}

func (c *recordingHotCache) putCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.put {
		n += v
	}
	return n
}

// TestGet_RangeRequestWarmsCacheInline pins the optimisation
// added after the integrity branch: when a range request triggers
// a full-piece fetch (so the verifier can hash the whole piece),
// the verified buffer is already in memory and the cache is keyed
// by piece — so we warm it immediately instead of publishing a
// promotion signal that would cause the async worker to re-fetch.
// Without this test, a future refactor could re-gate the cache
// put on `byteRange == nil` without anything catching it.
func TestGet_RangeRequestWarmsCacheInline(t *testing.T) {
	store := memory.New()
	fake := newFakeProvider("test")
	cache := &recordingHotCache{}
	h := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"test": fake},
		Placement: fixedPlacement{backend: "test"},
		Billing:   &recordingBilling{},
		Cache:     cache,
		Now:       func() time.Time { return time.Unix(1700000000, 0) },
	})

	body := []byte("0123456789")
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	h.Put(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("GET range status = %d, want 206; body=%s", rec.Code, rec.Body)
	}
	if rec.Body.String() != "2345" {
		t.Fatalf("GET range body = %q, want %q", rec.Body.String(), "2345")
	}
	if got := cache.putCount(); got != 1 {
		t.Fatalf("recordingHotCache.Put calls = %d, want 1 (range cache miss must warm the verified full piece)", got)
	}
}

// TestGet_RangeRequestServedFromCache pins the cache-hit path for
// range requests. The pre-fix fetchPiece gated cache.Get on
// byteRange == nil, so every range request paid a backend
// round-trip even when the piece was already hot in the cache.
// After the fix the cache is consulted for any request shape; on
// a hit the cached body is sliced down to the requested range
// without touching the backend at all. The test enforces this by
// (1) seeding the cache with the full piece, (2) deleting the
// backing piece from the provider so any backend GET would fail,
// and (3) issuing a range GET that must still succeed.
func TestGet_RangeRequestServedFromCache(t *testing.T) {
	store := memory.New()
	fake := newFakeProvider("test")
	hotCache, err := hot_object_cache.NewMemoryCache(hot_object_cache.EvictionPolicy{
		Kind:     hot_object_cache.EvictionLRU,
		MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}
	h := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"test": fake},
		Placement: fixedPlacement{backend: "test"},
		Billing:   &recordingBilling{},
		Cache:     hotCache,
		Now:       func() time.Time { return time.Unix(1700000000, 0) },
	})

	body := []byte("0123456789ABCDEF")
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	h.Put(httptest.NewRecorder(), req)

	// Warm the cache with a full-piece GET; the fetchPiece path
	// puts the verified full piece into the cache on this call.
	warmReq := httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
	warmRec := httptest.NewRecorder()
	h.Get(warmRec, warmReq)
	if warmRec.Code != http.StatusOK {
		t.Fatalf("warm GET status = %d, want 200", warmRec.Code)
	}

	// Wipe the backend. If the range GET below still hits the
	// provider the request fails. The MemoryCache copy must be
	// the only source of truth.
	fake.mu.Lock()
	fake.pieces = map[string][]byte{}
	fake.mu.Unlock()

	rangeReq := httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
	rangeReq.Header.Set("Range", "bytes=4-9")
	rangeRec := httptest.NewRecorder()
	h.Get(rangeRec, rangeReq)
	if rangeRec.Code != http.StatusPartialContent {
		t.Fatalf("range GET status = %d, want 206; body=%s", rangeRec.Code, rangeRec.Body)
	}
	if rangeRec.Body.String() != "456789" {
		t.Fatalf("range GET body = %q, want %q (cache slice must respect the requested byte range)", rangeRec.Body.String(), "456789")
	}
	if got, want := rangeRec.Header().Get("Content-Range"), "bytes 4-9/16"; got != want {
		t.Fatalf("range GET Content-Range = %q, want %q", got, want)
	}
}

func TestHashObjectKey_DistinguishesSlashVariants(t *testing.T) {
	a := hashObjectKey("a//b")
	b := hashObjectKey("a/b")
	if a == b {
		t.Errorf("hashObjectKey collapses a//b and a/b to the same hash (%q)", a)
	}
	trailing := hashObjectKey("a/b/")
	if trailing == b {
		t.Errorf("hashObjectKey collapses a/b/ and a/b to the same hash (%q)", b)
	}
}

func TestDelete_ManifestFirstOrdering(t *testing.T) {
	// When piece delete fails, manifest is still gone: GET must 404.
	store := memory.New()
	bill := &recordingBilling{}
	broken := &fakeProvider{pieces: map[string][]byte{}, etag: "e", backend: "test"}
	// Hook DeletePiece to fail after manifest is already removed.
	h := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"test": &brokenDeleteProvider{fakeProvider: broken}},
		Placement: fixedPlacement{backend: "test"},
		Billing:   bill,
		Now:       func() time.Time { return time.Unix(1700000000, 0) },
	})

	req := httptest.NewRequest(http.MethodPut, "/bucket/key", bytes.NewReader([]byte("abc")))
	req.ContentLength = 3
	h.Put(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodDelete, "/bucket/key", nil)
	rec := httptest.NewRecorder()
	h.Delete(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204 even when piece cleanup fails", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE status = %d, want 404 (manifest must be gone)", rec.Code)
	}
}

type brokenDeleteProvider struct{ *fakeProvider }

func (b *brokenDeleteProvider) DeletePiece(context.Context, string) error {
	return errors.New("simulated backend failure")
}

func TestGet_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/bucket/missing", nil)
	rec := httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDelete_IdempotentOnMissing(t *testing.T) {
	h, _, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodDelete, "/bucket/missing", nil)
	rec := httptest.NewRecorder()
	h.Delete(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("delete-missing status = %d, want 204", rec.Code)
	}
}

func TestList_ReturnsPutItems(t *testing.T) {
	h, _, _, _ := newTestHandler()
	names := []string{"a", "nested/path/b", "c"}
	for _, name := range names {
		req := httptest.NewRequest(http.MethodPut, "/bucket/"+name, bytes.NewReader([]byte(name)))
		req.ContentLength = int64(len(name))
		rec := httptest.NewRecorder()
		h.Put(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s status = %d", name, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/bucket/?list-type=2", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	type content struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	}
	type resp struct {
		XMLName  xml.Name  `xml:"ListBucketResult"`
		Name     string    `xml:"Name"`
		Contents []content `xml:"Contents"`
	}
	var r resp
	if err := xml.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal LIST response: %v (body=%s)", err, rec.Body)
	}
	if r.Name != "bucket" {
		t.Errorf("LIST name = %q, want %q", r.Name, "bucket")
	}
	if len(r.Contents) != len(names) {
		t.Fatalf("LIST contents = %d, want %d", len(r.Contents), len(names))
	}
	listed := map[string]int64{}
	for _, c := range r.Contents {
		listed[c.Key] = c.Size
	}
	for _, name := range names {
		size, ok := listed[name]
		if !ok {
			t.Errorf("LIST missing key %q; got keys %v", name, listed)
			continue
		}
		if size != int64(len(name)) {
			t.Errorf("LIST size for %q = %d, want %d", name, size, len(name))
		}
		// Round-trip: the listed key must be directly usable for GET.
		getReq := httptest.NewRequest(http.MethodGet, "/bucket/"+name, nil)
		getRec := httptest.NewRecorder()
		h.Get(getRec, getReq)
		if getRec.Code != http.StatusOK {
			t.Errorf("GET /bucket/%s after LIST status = %d, want 200", name, getRec.Code)
		}
	}
}

func TestList_DedupesOverwrittenVersions(t *testing.T) {
	// Use an advancing clock so each PUT gets a distinct VersionID
	// (newPieceID mixes the timestamp) — otherwise the memory store's
	// ManifestKey collides and masks the duplicate-row bug.
	store := memory.New()
	fake := newFakeProvider("test")
	now := time.Unix(1700000000, 0)
	h := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"test": fake},
		Placement: fixedPlacement{backend: "test"},
		Billing:   &recordingBilling{},
		Now: func() time.Time {
			t := now
			now = now.Add(time.Second)
			return t
		},
	})

	for i := 0; i < 3; i++ {
		body := []byte(fmt.Sprintf("v%d", i))
		req := httptest.NewRequest(http.MethodPut, "/bucket/key", bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		rec := httptest.NewRecorder()
		h.Put(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %d status = %d", i, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/bucket/?list-type=2", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST status = %d; body=%s", rec.Code, rec.Body)
	}
	type content struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	}
	type resp struct {
		XMLName  xml.Name  `xml:"ListBucketResult"`
		Contents []content `xml:"Contents"`
	}
	var r resp
	if err := xml.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal LIST: %v (body=%s)", err, rec.Body)
	}
	if len(r.Contents) != 1 {
		t.Fatalf("LIST after 3 overwrites returned %d entries, want 1 (%+v)", len(r.Contents), r.Contents)
	}
	if r.Contents[0].Key != "key" {
		t.Errorf("LIST key = %q, want %q", r.Contents[0].Key, "key")
	}
	if r.Contents[0].Size != 2 {
		t.Errorf("LIST size = %d, want 2 (latest write)", r.Contents[0].Size)
	}
}

func TestParseBucketKey(t *testing.T) {
	cases := []struct {
		in, bucket, key string
	}{
		{"/b/k", "b", "k"},
		{"/b/k/subkey", "b", "k/subkey"},
		{"/b", "b", ""},
		{"/", "", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		b, k := parseBucketKey(tc.in)
		if b != tc.bucket || k != tc.key {
			t.Errorf("parseBucketKey(%q) = (%q,%q), want (%q,%q)", tc.in, b, k, tc.bucket, tc.key)
		}
	}
}

func TestParseHTTPRange(t *testing.T) {
	r, err := parseHTTPRange("bytes=0-99", 1000)
	if err != nil {
		t.Fatalf("parseHTTPRange: %v", err)
	}
	if r.Start != 0 || r.End != 99 {
		t.Errorf("parseHTTPRange = %+v, want [0,99]", r)
	}
	r, err = parseHTTPRange("bytes=500-", 1000)
	if err != nil {
		t.Fatalf("parseHTTPRange(open-ended): %v", err)
	}
	if r.Start != 500 || r.End != -1 {
		t.Errorf("parseHTTPRange open-ended = %+v, want [500,-1]", r)
	}
	if _, err := parseHTTPRange("bytes=-100", 1000); err == nil {
		t.Error("parseHTTPRange(suffix) should error")
	}
	if _, err := parseHTTPRange("bytes=10-5", 1000); err == nil {
		t.Error("parseHTTPRange(inverted) should error")
	}
	if _, err := parseHTTPRange("bytes=1000-", 1000); err == nil {
		t.Error("parseHTTPRange(start==size) should error")
	}
	if _, err := parseHTTPRange("bytes=2000-", 1000); err == nil {
		t.Error("parseHTTPRange(start>size) should error")
	}
}

func TestCopyObject_SameBucket_NoDedup(t *testing.T) {
	h, fake, _, store := newTestHandler()
	body := []byte("copy-me")
	// PUT source.
	pr := httptest.NewRequest(http.MethodPut, "/bucket/src.txt", bytes.NewReader(body))
	pw := httptest.NewRecorder()
	h.Put(pw, pr)
	if pw.Code != http.StatusOK {
		t.Fatalf("Put src: %d %s", pw.Code, pw.Body.String())
	}
	// COPY -> dst.
	cr := httptest.NewRequest(http.MethodPut, "/bucket/dst.txt", nil)
	cr.Header.Set("x-amz-copy-source", "/bucket/src.txt")
	cw := httptest.NewRecorder()
	h.Copy(cw, cr)
	if cw.Code != http.StatusOK {
		t.Fatalf("Copy: %d %s", cw.Code, cw.Body.String())
	}
	// GET dst returns the same body.
	gr := httptest.NewRequest(http.MethodGet, "/bucket/dst.txt", nil)
	gw := httptest.NewRecorder()
	h.Get(gw, gr)
	if gw.Code != http.StatusOK {
		t.Fatalf("Get dst: %d %s", gw.Code, gw.Body.String())
	}
	if !bytes.Equal(gw.Body.Bytes(), body) {
		t.Fatalf("dst body = %q, want %q", gw.Body.Bytes(), body)
	}
	// Two distinct piece IDs in fake provider (no dedup wired).
	if len(fake.pieces) != 2 {
		t.Fatalf("fake pieces = %d, want 2", len(fake.pieces))
	}
	// Both manifests resolvable.
	for _, key := range []string{"src.txt", "dst.txt"} {
		mkey := manifest_store.ManifestKey{
			TenantID:      "anonymous",
			Bucket:        "bucket",
			ObjectKeyHash: hashObjectKey(key),
		}
		if _, err := store.Get(context.Background(), mkey); err != nil {
			t.Fatalf("manifest %s: %v", key, err)
		}
	}
}

func TestCopyObject_MissingSource(t *testing.T) {
	h, _, _, _ := newTestHandler()
	cr := httptest.NewRequest(http.MethodPut, "/bucket/dst.txt", nil)
	cr.Header.Set("x-amz-copy-source", "/bucket/missing.txt")
	cw := httptest.NewRecorder()
	h.Copy(cw, cr)
	if cw.Code != http.StatusNotFound {
		t.Fatalf("Copy missing: %d %s", cw.Code, cw.Body.String())
	}
}

func TestParseCopySource(t *testing.T) {
	cases := []struct {
		in              string
		bucket, key, ver string
		wantErr          bool
	}{
		{"/b/k", "b", "k", "", false},
		{"b/k", "b", "k", "", false},
		{"b/nested/k", "b", "nested/k", "", false},
		{"b/k?versionId=abc", "b", "k", "abc", false},
		{"", "", "", "", true},
		{"bonly", "", "", "", true},
		{"b/", "", "", "", true},
	}
	for _, c := range cases {
		bk, ky, vr, err := parseCopySource(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseCopySource(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if bk != c.bucket || ky != c.key || vr != c.ver {
			t.Errorf("parseCopySource(%q) = %q %q %q, want %q %q %q", c.in, bk, ky, vr, c.bucket, c.key, c.ver)
		}
	}
}

// fakeProviderWithCountry extends fakeProvider with a configurable country label.
type fakeProviderWithCountry struct {
	*fakeProvider
	country string
}

func (f *fakeProviderWithCountry) PlacementLabels() providers.PlacementLabels {
	return providers.PlacementLabels{Country: f.country}
}

// fakeResidencyChecker implements ResidencyChecker for tests.
type fakeResidencyChecker struct{}

func (f *fakeResidencyChecker) Check(tenantID, backendCountry string, policyResidency []string) error {
	for _, c := range policyResidency {
		if c == backendCountry {
			return nil
		}
	}
	if len(policyResidency) == 0 {
		return nil
	}
	return fmt.Errorf("backend country %q not in tenant allowlist", backendCountry)
}

// fakeLegalHoldChecker implements LegalHoldChecker for tests.
type fakeLegalHoldChecker struct {
	holds map[string][]LegalHoldEntry
}

func (f *fakeLegalHoldChecker) Active(_ context.Context, tenantID, bucket, objectKey string) ([]LegalHoldEntry, error) {
	key := tenantID + "/" + bucket + "/" + objectKey
	return f.holds[key], nil
}

func TestDelete_BlockedByLegalHold(t *testing.T) {
	store := memory.New()
	fake := newFakeProvider("test")
	bill := &recordingBilling{}
	holdChecker := &fakeLegalHoldChecker{
		holds: map[string][]LegalHoldEntry{
			"anonymous/bucket/held-obj": {{ID: "hold-1"}},
		},
	}
	h := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"test": fake},
		Placement: fixedPlacement{backend: "test"},
		Billing:   bill,
		Now:       func() time.Time { return time.Unix(1700000000, 0) },
		Compliance: ComplianceHooks{
			LegalHoldStore: holdChecker,
		},
	})

	// PUT an object
	req := httptest.NewRequest(http.MethodPut, "/bucket/held-obj", bytes.NewReader([]byte("data")))
	req.ContentLength = 4
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	// DELETE should be blocked
	req = httptest.NewRequest(http.MethodDelete, "/bucket/held-obj", nil)
	rec = httptest.NewRecorder()
	h.Delete(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE status = %d, want 403; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "ObjectUnderLegalHold") {
		t.Errorf("DELETE body should contain ObjectUnderLegalHold, got: %s", rec.Body)
	}

	// Verify the object still exists
	if len(fake.pieces) != 1 {
		t.Errorf("piece should still exist, got %d pieces", len(fake.pieces))
	}

	// DELETE an object without a hold should succeed
	req = httptest.NewRequest(http.MethodPut, "/bucket/free-obj", bytes.NewReader([]byte("data")))
	req.ContentLength = 4
	h.Put(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodDelete, "/bucket/free-obj", nil)
	rec = httptest.NewRecorder()
	h.Delete(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE of non-held object status = %d, want 204; body=%s", rec.Code, rec.Body)
	}
}

// residencyPlacement returns a placement with a Residency allowlist.
type residencyPlacement struct {
	backend   string
	residency []string
}

func (p residencyPlacement) ResolveBackend(string, string, string) (string, metadata.PlacementPolicy, error) {
	return p.backend, metadata.PlacementPolicy{
		AllowedBackends: []string{p.backend},
		Residency:       p.residency,
	}, nil
}

func TestCreateMultipartUpload_ResidencyViolation(t *testing.T) {
	store := memory.New()
	fp := newFakeProvider("test")
	providerWithCountry := &fakeProviderWithCountry{fakeProvider: fp, country: "US"}
	bill := &recordingBilling{}
	mpStore := multipart.NewMemoryStore()
	h := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"test": providerWithCountry},
		Placement: residencyPlacement{backend: "test", residency: []string{"DE"}},
		Billing:   bill,
		Multipart: mpStore,
		Now:       func() time.Time { return time.Unix(1700000000, 0) },
		Compliance: ComplianceHooks{
			Residency: &fakeResidencyChecker{},
		},
	})

	// CreateMultipartUpload should be rejected: backend is US, tenant allows only DE
	req := httptest.NewRequest(http.MethodPost, "/bucket/key?uploads", nil)
	rec := httptest.NewRecorder()
	h.CreateMultipartUpload(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("CreateMultipartUpload status = %d, want 403; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "DataResidencyViolation") {
		t.Errorf("body should contain DataResidencyViolation, got: %s", rec.Body)
	}

	// With matching residency, it should succeed
	h2 := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"test": providerWithCountry},
		Placement: residencyPlacement{backend: "test", residency: []string{"US"}},
		Billing:   bill,
		Multipart: mpStore,
		Now:       func() time.Time { return time.Unix(1700000000, 0) },
		Compliance: ComplianceHooks{
			Residency: &fakeResidencyChecker{},
		},
	})
	req = httptest.NewRequest(http.MethodPost, "/bucket/key?uploads", nil)
	rec = httptest.NewRecorder()
	h2.CreateMultipartUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateMultipartUpload with matching residency status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
}

// recordingAudit captures every AuditEntry the handler records so
// tests can assert on the (operation, backend, country) tuple
// emitted by each S3 op.
type recordingAudit struct {
	mu      sync.Mutex
	entries []AuditEntry
}

func (r *recordingAudit) Record(_ context.Context, e AuditEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
	return nil
}

func (r *recordingAudit) operations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.Operation)
	}
	return out
}

// ecPlacement returns a placement that points every object at a
// single backend with the named ErasureProfile.
type ecPlacement struct {
	backend string
	profile string
}

func (p ecPlacement) ResolveBackend(string, string, string) (string, metadata.PlacementPolicy, error) {
	return p.backend, metadata.PlacementPolicy{
		AllowedBackends: []string{p.backend},
		ErasureProfile:  p.profile,
	}, nil
}

func TestGetErasureCoded_AuditsOnSuccess(t *testing.T) {
	store := memory.New()
	fp := newFakeProvider("test")
	provWithCountry := &fakeProviderWithCountry{fakeProvider: fp, country: "US"}
	audit := &recordingAudit{}
	h := New(Config{
		Manifests:     store,
		Providers:     map[string]providers.StorageProvider{"test": provWithCountry},
		Placement:     ecPlacement{backend: "test", profile: erasure_coding.Profile6Plus2.Name},
		ErasureCoding: erasure_coding.DefaultRegistry(),
		Now:           func() time.Time { return time.Unix(1700000000, 0) },
		Compliance:    ComplianceHooks{Audit: audit},
	})

	body := bytes.Repeat([]byte("ec-payload!"), 4096)
	req := httptest.NewRequest(http.MethodPut, "/bucket/ec-obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("EC PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	req = httptest.NewRequest(http.MethodGet, "/bucket/ec-obj", nil)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("EC GET status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Body.Len(); got != len(body) {
		t.Fatalf("EC GET body length = %d, want %d", got, len(body))
	}

	ops := audit.operations()
	if len(ops) != 2 || ops[0] != "PUT" || ops[1] != "GET" {
		t.Fatalf("EC audit ops = %v, want [PUT GET]", ops)
	}
	getEntry := audit.entries[1]
	if getEntry.PieceBackend != "test" {
		t.Errorf("EC GET audit backend = %q, want %q", getEntry.PieceBackend, "test")
	}
	if getEntry.BackendCountry != "US" {
		t.Errorf("EC GET audit country = %q, want US", getEntry.BackendCountry)
	}
	if getEntry.Bucket != "bucket" || getEntry.ObjectKey != "ec-obj" {
		t.Errorf("EC GET audit (bucket, key) = (%q, %q), want (bucket, ec-obj)", getEntry.Bucket, getEntry.ObjectKey)
	}
}

func TestGetMultipart_AuditsOnSuccess(t *testing.T) {
	store := memory.New()
	fp := newFakeProvider("test")
	provWithCountry := &fakeProviderWithCountry{fakeProvider: fp, country: "DE"}
	audit := &recordingAudit{}
	mpStore := multipart.NewMemoryStore()
	h := New(Config{
		Manifests:  store,
		Providers:  map[string]providers.StorageProvider{"test": provWithCountry},
		Placement:  fixedPlacement{backend: "test"},
		Multipart:  mpStore,
		Now:        func() time.Time { return time.Unix(1700000000, 0) },
		Compliance: ComplianceHooks{Audit: audit},
	})

	// Create + upload two parts + complete to land a multipart
	// manifest in the store.
	req := httptest.NewRequest(http.MethodPost, "/bucket/mp-obj?uploads", nil)
	rec := httptest.NewRecorder()
	h.CreateMultipartUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateMultipartUpload status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var initRes initiateMultipartUploadResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &initRes); err != nil {
		t.Fatalf("decode initiate: %v", err)
	}
	uploadID := initRes.UploadID
	if uploadID == "" {
		t.Fatal("CreateMultipartUpload returned empty UploadId")
	}

	parts := [][]byte{
		bytes.Repeat([]byte("part-1-"), 1024),
		bytes.Repeat([]byte("part-2-"), 1024),
	}
	completed := make([]completeUploadEntry, 0, len(parts))
	for i, body := range parts {
		partNum := i + 1
		url := fmt.Sprintf("/bucket/mp-obj?uploadId=%s&partNumber=%d", uploadID, partNum)
		req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		rec := httptest.NewRecorder()
		h.UploadPart(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("UploadPart %d status = %d, want 200; body=%s", partNum, rec.Code, rec.Body)
		}
		etag := strings.Trim(rec.Header().Get("ETag"), `"`)
		if etag == "" {
			t.Fatalf("UploadPart %d returned empty ETag", partNum)
		}
		completed = append(completed, completeUploadEntry{PartNumber: partNum, ETag: etag})
	}

	completeBody := completeMultipartUploadRequest{Parts: completed}
	completeXML, err := xml.Marshal(completeBody)
	if err != nil {
		t.Fatalf("marshal complete body: %v", err)
	}
	url := fmt.Sprintf("/bucket/mp-obj?uploadId=%s", uploadID)
	req = httptest.NewRequest(http.MethodPost, url, bytes.NewReader(completeXML))
	rec = httptest.NewRecorder()
	h.CompleteMultipartUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CompleteMultipartUpload status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	// GET should round-trip and add a GET audit record.
	req = httptest.NewRequest(http.MethodGet, "/bucket/mp-obj", nil)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("multipart GET status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	wantBytes := append(append([]byte{}, parts[0]...), parts[1]...)
	if !bytes.Equal(rec.Body.Bytes(), wantBytes) {
		t.Fatalf("multipart GET body mismatch: got %d bytes, want %d", rec.Body.Len(), len(wantBytes))
	}

	ops := audit.operations()
	if len(ops) != 2 || ops[0] != "PUT" || ops[1] != "GET" {
		t.Fatalf("multipart audit ops = %v, want [PUT GET]", ops)
	}
	getEntry := audit.entries[1]
	if getEntry.PieceBackend != "test" {
		t.Errorf("multipart GET audit backend = %q, want %q", getEntry.PieceBackend, "test")
	}
	if getEntry.BackendCountry != "DE" {
		t.Errorf("multipart GET audit country = %q, want DE", getEntry.BackendCountry)
	}
	if getEntry.PieceID == "" {
		t.Errorf("multipart GET audit PieceID is empty; want first piece ID")
	}
}

// TestHead_ErasureCoded_NoRange pins that HEAD on an EC manifest
// mirrors getErasureCoded's response shape: 200 OK with the full
// ObjectSize as Content-Length, NO ETag (EC piece hashes are
// per-shard, not per-object), and an audit record with the EC
// primary backend's placement labels.
func TestHead_ErasureCoded_NoRange(t *testing.T) {
	store := memory.New()
	fp := newFakeProvider("test")
	provWithCountry := &fakeProviderWithCountry{fakeProvider: fp, country: "US"}
	audit := &recordingAudit{}
	h := New(Config{
		Manifests:     store,
		Providers:     map[string]providers.StorageProvider{"test": provWithCountry},
		Placement:     ecPlacement{backend: "test", profile: erasure_coding.Profile6Plus2.Name},
		ErasureCoding: erasure_coding.DefaultRegistry(),
		Now:           func() time.Time { return time.Unix(1700000000, 0) },
		Compliance:    ComplianceHooks{Audit: audit},
	})

	body := bytes.Repeat([]byte("ec-payload!"), 4096)
	req := httptest.NewRequest(http.MethodPut, "/bucket/ec-obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	h.Put(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodHead, "/bucket/ec-obj", nil)
	rec := httptest.NewRecorder()
	h.Head(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("EC HEAD status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Length"); got != fmt.Sprintf("%d", len(body)) {
		t.Errorf("EC HEAD Content-Length = %q, want %q", got, fmt.Sprintf("%d", len(body)))
	}
	if got := rec.Header().Get("ETag"); got != "" {
		t.Errorf("EC HEAD must NOT set ETag (matches getErasureCoded which omits ETag for shard hashes), got %q", got)
	}
	ops := audit.operations()
	if len(ops) != 2 || ops[0] != "PUT" || ops[1] != "HEAD" {
		t.Fatalf("EC audit ops = %v, want [PUT HEAD]", ops)
	}
}

// TestHead_ErasureCoded_RangeReturns501 pins that HEAD on an EC
// manifest with a Range header returns 501 NotImplemented, exactly
// what getErasureCoded does. Returning 206 with a slice-sized
// Content-Length would advertise capability the follow-up GET
// doesn't have, breaking AWS SDK / CDN pre-flight probes that use
// HEAD to discover Range support.
func TestHead_ErasureCoded_RangeReturns501(t *testing.T) {
	store := memory.New()
	fp := newFakeProvider("test")
	h := New(Config{
		Manifests:     store,
		Providers:     map[string]providers.StorageProvider{"test": fp},
		Placement:     ecPlacement{backend: "test", profile: erasure_coding.Profile6Plus2.Name},
		ErasureCoding: erasure_coding.DefaultRegistry(),
		Now:           func() time.Time { return time.Unix(1700000000, 0) },
	})

	body := bytes.Repeat([]byte("ec-payload!"), 4096)
	req := httptest.NewRequest(http.MethodPut, "/bucket/ec-obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	h.Put(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodHead, "/bucket/ec-obj", nil)
	req.Header.Set("Range", "bytes=0-99")
	rec := httptest.NewRecorder()
	h.Head(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("EC HEAD with Range status = %d, want 501 (mirror getErasureCoded)", rec.Code)
	}
	if rec.Header().Get("Content-Range") != "" {
		t.Errorf("EC HEAD with Range must NOT set Content-Range on 501 response")
	}
}

// TestHead_Multipart_NoRange pins that HEAD on a multipart manifest
// mirrors getMultipart's response shape: 200 OK with the full
// ObjectSize as Content-Length, NO ETag (the multipart-ETag from
// CompleteMultipartUpload isn't recoverable from the manifest;
// matching GET, we omit rather than fabricate a Pieces[0]-based
// value), and an audit record.
func TestHead_Multipart_NoRange(t *testing.T) {
	store := memory.New()
	fp := newFakeProvider("test")
	provWithCountry := &fakeProviderWithCountry{fakeProvider: fp, country: "DE"}
	audit := &recordingAudit{}
	mpStore := multipart.NewMemoryStore()
	h := New(Config{
		Manifests:  store,
		Providers:  map[string]providers.StorageProvider{"test": provWithCountry},
		Placement:  fixedPlacement{backend: "test"},
		Multipart:  mpStore,
		Now:        func() time.Time { return time.Unix(1700000000, 0) },
		Compliance: ComplianceHooks{Audit: audit},
	})

	// Land a multipart manifest via create + upload + complete.
	req := httptest.NewRequest(http.MethodPost, "/bucket/mp-obj?uploads", nil)
	rec := httptest.NewRecorder()
	h.CreateMultipartUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateMultipartUpload status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var initRes initiateMultipartUploadResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &initRes); err != nil {
		t.Fatalf("decode initiate: %v", err)
	}
	uploadID := initRes.UploadID

	parts := [][]byte{
		bytes.Repeat([]byte("part-1-"), 1024),
		bytes.Repeat([]byte("part-2-"), 1024),
	}
	totalSize := 0
	completed := make([]completeUploadEntry, 0, len(parts))
	for i, partBody := range parts {
		partNum := i + 1
		totalSize += len(partBody)
		url := fmt.Sprintf("/bucket/mp-obj?uploadId=%s&partNumber=%d", uploadID, partNum)
		uploadReq := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(partBody))
		uploadReq.ContentLength = int64(len(partBody))
		uploadRec := httptest.NewRecorder()
		h.UploadPart(uploadRec, uploadReq)
		if uploadRec.Code != http.StatusOK {
			t.Fatalf("UploadPart %d status = %d, want 200; body=%s", partNum, uploadRec.Code, uploadRec.Body)
		}
		etag := strings.Trim(uploadRec.Header().Get("ETag"), `"`)
		completed = append(completed, completeUploadEntry{PartNumber: partNum, ETag: etag})
	}
	completeXML, err := xml.Marshal(completeMultipartUploadRequest{Parts: completed})
	if err != nil {
		t.Fatalf("marshal complete body: %v", err)
	}
	url := fmt.Sprintf("/bucket/mp-obj?uploadId=%s", uploadID)
	completeReq := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(completeXML))
	completeRec := httptest.NewRecorder()
	h.CompleteMultipartUpload(completeRec, completeReq)
	if completeRec.Code != http.StatusOK {
		t.Fatalf("CompleteMultipartUpload status = %d, want 200; body=%s", completeRec.Code, completeRec.Body)
	}

	// Now HEAD the assembled multipart object.
	headReq := httptest.NewRequest(http.MethodHead, "/bucket/mp-obj", nil)
	headRec := httptest.NewRecorder()
	h.Head(headRec, headReq)

	if headRec.Code != http.StatusOK {
		t.Fatalf("multipart HEAD status = %d, want 200; body=%s", headRec.Code, headRec.Body)
	}
	if got := headRec.Header().Get("Content-Length"); got != fmt.Sprintf("%d", totalSize) {
		t.Errorf("multipart HEAD Content-Length = %q, want %q", got, fmt.Sprintf("%d", totalSize))
	}
	if got := headRec.Header().Get("ETag"); got != "" {
		t.Errorf("multipart HEAD must NOT set ETag (matches getMultipart), got %q", got)
	}
	ops := audit.operations()
	// PUT (Complete) is the only audited write here; UploadPart
	// does not audit. So we expect ops = [PUT, HEAD].
	if len(ops) == 0 || ops[len(ops)-1] != "HEAD" {
		t.Errorf("multipart audit ops = %v, want last entry to be HEAD", ops)
	}
}

// TestHead_Multipart_RangeReturns501 pins that HEAD on a multipart
// manifest with a Range header returns 501 NotImplemented, exactly
// what getMultipart does. Same rationale as the EC case: HEAD must
// not advertise Range support the matching GET doesn't have.
func TestHead_Multipart_RangeReturns501(t *testing.T) {
	store := memory.New()
	fp := newFakeProvider("test")
	mpStore := multipart.NewMemoryStore()
	h := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"test": fp},
		Placement: fixedPlacement{backend: "test"},
		Multipart: mpStore,
		Now:       func() time.Time { return time.Unix(1700000000, 0) },
	})

	req := httptest.NewRequest(http.MethodPost, "/bucket/mp-obj?uploads", nil)
	rec := httptest.NewRecorder()
	h.CreateMultipartUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateMultipartUpload status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var initRes initiateMultipartUploadResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &initRes); err != nil {
		t.Fatalf("decode initiate: %v", err)
	}
	uploadID := initRes.UploadID

	parts := [][]byte{
		bytes.Repeat([]byte("part-1-"), 1024),
		bytes.Repeat([]byte("part-2-"), 1024),
	}
	completed := make([]completeUploadEntry, 0, len(parts))
	for i, partBody := range parts {
		partNum := i + 1
		url := fmt.Sprintf("/bucket/mp-obj?uploadId=%s&partNumber=%d", uploadID, partNum)
		uploadReq := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(partBody))
		uploadReq.ContentLength = int64(len(partBody))
		uploadRec := httptest.NewRecorder()
		h.UploadPart(uploadRec, uploadReq)
		etag := strings.Trim(uploadRec.Header().Get("ETag"), `"`)
		completed = append(completed, completeUploadEntry{PartNumber: partNum, ETag: etag})
	}
	completeXML, _ := xml.Marshal(completeMultipartUploadRequest{Parts: completed})
	url := fmt.Sprintf("/bucket/mp-obj?uploadId=%s", uploadID)
	completeReq := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(completeXML))
	completeRec := httptest.NewRecorder()
	h.CompleteMultipartUpload(completeRec, completeReq)
	if completeRec.Code != http.StatusOK {
		t.Fatalf("CompleteMultipartUpload status = %d, want 200; body=%s", completeRec.Code, completeRec.Body)
	}

	headReq := httptest.NewRequest(http.MethodHead, "/bucket/mp-obj", nil)
	headReq.Header.Set("Range", "bytes=0-99")
	headRec := httptest.NewRecorder()
	h.Head(headRec, headReq)

	if headRec.Code != http.StatusNotImplemented {
		t.Fatalf("multipart HEAD with Range status = %d, want 501 (mirror getMultipart)", headRec.Code)
	}
}

// TestHandler_RequireAuth_NoAuthenticator_Returns500 verifies the
// production-mode safety net: when RequireAuth=true and Auth is
// nil, every request returns 500 InternalAuthMisconfigured instead
// of silently serving under AnonymousTenant. cmd/gateway turns on
// RequireAuth whenever Env="production".
func TestHandler_RequireAuth_NoAuthenticator_Returns500(t *testing.T) {
	store := memory.New()
	fake := newFakeProvider("test")
	bill := &recordingBilling{}
	h := New(Config{
		Manifests:   store,
		Providers:   map[string]providers.StorageProvider{"test": fake},
		Placement:   fixedPlacement{backend: "test"},
		Billing:     bill,
		RequireAuth: true,
		// Auth intentionally nil to simulate the
		// misconfiguration we want this safety net to catch.
		Auth: nil,
		Now:  func() time.Time { return time.Unix(1700000000, 0) },
	})

	cases := []struct {
		name    string
		method  string
		path    string
		bodyStr string
		fn      func(http.ResponseWriter, *http.Request)
	}{
		{"PUT", http.MethodPut, "/bucket/obj", "hello", h.Put},
		{"GET", http.MethodGet, "/bucket/obj", "", h.Get},
		{"HEAD", http.MethodHead, "/bucket/obj", "", h.Head},
		{"DELETE", http.MethodDelete, "/bucket/obj", "", h.Delete},
		// LIST must exercise h.List (which calls listBucket and
		// hits its own authenticate() → writeAuthError() path).
		// Calling h.Get here would route through resolve() →
		// writeResolveError() which is an entirely different
		// branch and would mask a regression in listBucket's
		// auth handling even though both currently return 500.
		{"LIST", http.MethodGet, "/bucket/?list-type=2", "", h.List},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body *bytes.Reader
			if tc.bodyStr != "" {
				body = bytes.NewReader([]byte(tc.bodyStr))
			} else {
				body = bytes.NewReader(nil)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.ContentLength = int64(body.Len())
			rec := httptest.NewRecorder()
			tc.fn(rec, req)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("%s status = %d, want 500; body=%s", tc.name, rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), "InternalAuthMisconfigured") {
				t.Errorf("%s body = %s, want InternalAuthMisconfigured", tc.name, rec.Body)
			}
		})
	}
}

// TestHandler_RequireAuthFalse_NoAuthenticator_AllowsAnonymous
// pins the legacy behaviour: when RequireAuth=false (the default,
// covering dev and non-production deploys) and Auth is nil, the
// handler keeps falling back to AnonymousTenant.
func TestHandler_RequireAuthFalse_NoAuthenticator_AllowsAnonymous(t *testing.T) {
	h, _, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader([]byte("ok")))
	req.ContentLength = 2
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (dev mode anonymous); body=%s", rec.Code, rec.Body)
	}
}

// integritySinkRecorder is an IntegrityFailureSink used by tests
// to verify the GET path emits the correct observability counter
// for each verifier outcome. Two channels are tracked separately:
//
//   - hits["backend"] counts ErrIntegrityCheckFailed events (the
//     gateway returned 502 IntegrityCheckFailed).
//   - unrecognised["backend"] counts ErrIntegrityClaimUnrecognized
//     events (the gateway served the bytes but flagged a legacy
//     manifest with an opaque hash format).
//
// Concurrent-safe so the tests can run in parallel with the rest
// of the suite if a future maintainer adds t.Parallel().
type integritySinkRecorder struct {
	mu           sync.Mutex
	hits         map[string]int
	unrecognised map[string]int
}

func (s *integritySinkRecorder) Inc(backend string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hits == nil {
		s.hits = make(map[string]int)
	}
	s.hits[backend]++
}

func (s *integritySinkRecorder) IncUnrecognized(backend string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unrecognised == nil {
		s.unrecognised = make(map[string]int)
	}
	s.unrecognised[backend]++
}

func (s *integritySinkRecorder) count(backend string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[backend]
}

func (s *integritySinkRecorder) countUnrecognized(backend string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unrecognised[backend]
}

// TestGet_TamperedPiece_FailsClosed exercises PR-2: a backend piece
// that has been mutated since PUT must be rejected with HTTP 502
// IntegrityCheckFailed, the bytes must NOT reach the client, and a
// per-backend zkof_integrity_failure_total metric must be emitted.
func TestGet_TamperedPiece_FailsClosed(t *testing.T) {
	store := memory.New()
	fake := newFakeProvider("test")
	bill := &recordingBilling{}
	sink := &integritySinkRecorder{}
	h := New(Config{
		Manifests:         store,
		Providers:         map[string]providers.StorageProvider{"test": fake},
		Placement:         fixedPlacement{backend: "test"},
		Billing:           bill,
		IntegrityFailures: sink,
		Now:               func() time.Time { return time.Unix(1700000000, 0) },
	})

	body := []byte("zkof piece integrity end-to-end")

	// PUT through the handler so the manifest carries a real
	// blake3 hash. This is the only writer of "well-formed"
	// pieces in this test.
	req := httptest.NewRequest(http.MethodPut, "/bucket/integrity.txt", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	// Sanity: the unmodified GET works.
	req = httptest.NewRequest(http.MethodGet, "/bucket/integrity.txt", nil)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("baseline GET status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != string(body) {
		t.Fatalf("baseline GET body = %q, want %q", got, body)
	}
	if hits := sink.count("test"); hits != 0 {
		t.Fatalf("integrity hits on clean GET = %d, want 0", hits)
	}

	// Mutate the on-backend bytes behind the manifest's back.
	// This is what bit-rot, a buggy backend, or an attacker who
	// can write to the backend looks like to the gateway.
	fake.mu.Lock()
	if len(fake.pieces) == 0 {
		fake.mu.Unlock()
		t.Fatalf("fake backend has no pieces after PUT")
	}
	for k := range fake.pieces {
		fake.pieces[k] = []byte("tampered bytes that do not match the manifest hash")
	}
	fake.mu.Unlock()

	// GET must now fail closed, with no client bytes, an explicit
	// IntegrityCheckFailed code, and a metric sample.
	req = httptest.NewRequest(http.MethodGet, "/bucket/integrity.txt", nil)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("tampered GET status = %d, want 502", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "IntegrityCheckFailed") {
		t.Fatalf("tampered GET body missing IntegrityCheckFailed code: %q", got)
	}
	if got := rec.Body.String(); strings.Contains(got, "tampered bytes") {
		t.Fatalf("tampered GET leaked backend bytes to the client: %q", got)
	}
	if hits := sink.count("test"); hits != 1 {
		t.Fatalf("integrity hits after tampered GET = %d, want 1", hits)
	}
}

// TestGet_TamperedPiece_FailsClosed_NoCache exercises the OOM-guard
// fix in fetchPiece: verification must still run (and fail closed)
// when the handler is configured without a hot cache. Before the
// fix, the oversize guard AND the verification block were both
// gated behind `cfg.Cache != nil`, so a no-cache deployment skipped
// verification entirely. This test pins the no-cache + in-budget
// branch.
func TestGet_TamperedPiece_FailsClosed_NoCache(t *testing.T) {
	store := memory.New()
	fake := newFakeProvider("test")
	sink := &integritySinkRecorder{}
	h := New(Config{
		Manifests:         store,
		Providers:         map[string]providers.StorageProvider{"test": fake},
		Placement:         fixedPlacement{backend: "test"},
		IntegrityFailures: sink,
		Now:               func() time.Time { return time.Unix(1700000000, 0) },
		// Cache deliberately nil — this is the path the OOM fix
		// also rescued from unbounded buffering.
	})

	body := []byte("integrity matters when there is no cache to hide behind")
	req := httptest.NewRequest(http.MethodPut, "/bucket/no-cache.txt", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	fake.mu.Lock()
	for k := range fake.pieces {
		fake.pieces[k] = []byte("tampered no-cache bytes")
	}
	fake.mu.Unlock()

	req = httptest.NewRequest(http.MethodGet, "/bucket/no-cache.txt", nil)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("tampered no-cache GET status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "IntegrityCheckFailed") {
		t.Fatalf("tampered no-cache GET body missing IntegrityCheckFailed: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "tampered no-cache bytes") {
		t.Fatalf("tampered no-cache GET leaked backend bytes: %q", rec.Body.String())
	}
	if hits := sink.count("test"); hits != 1 {
		t.Fatalf("integrity hits after tampered no-cache GET = %d, want 1", hits)
	}
}

// TestGetMultipart_TamperedPart_FailsClosed verifies the multipart
// GET path now re-hashes every part body before assembling the
// response. Before this fix the multipart path relied on an
// aggregate object-size check which a same-size tamper could slip
// past silently.
func TestGetMultipart_TamperedPart_FailsClosed(t *testing.T) {
	store := memory.New()
	fake := newFakeProvider("test")
	sink := &integritySinkRecorder{}
	mpStore := multipart.NewMemoryStore()
	h := New(Config{
		Manifests:         store,
		Providers:         map[string]providers.StorageProvider{"test": fake},
		Placement:         fixedPlacement{backend: "test"},
		Multipart:         mpStore,
		IntegrityFailures: sink,
		Now:               func() time.Time { return time.Unix(1700000000, 0) },
	})

	// Create + upload two parts + complete.
	req := httptest.NewRequest(http.MethodPost, "/bucket/mp-tamper?uploads", nil)
	rec := httptest.NewRecorder()
	h.CreateMultipartUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateMultipartUpload status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var initRes initiateMultipartUploadResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &initRes); err != nil {
		t.Fatalf("decode initiate: %v", err)
	}

	parts := [][]byte{
		bytes.Repeat([]byte("aaaa"), 256),
		bytes.Repeat([]byte("bbbb"), 256),
	}
	completed := make([]completeUploadEntry, 0, len(parts))
	for i, body := range parts {
		partNum := i + 1
		url := fmt.Sprintf("/bucket/mp-tamper?uploadId=%s&partNumber=%d", initRes.UploadID, partNum)
		req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		rec := httptest.NewRecorder()
		h.UploadPart(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("UploadPart %d status = %d, want 200; body=%s", partNum, rec.Code, rec.Body)
		}
		etag := strings.Trim(rec.Header().Get("ETag"), `"`)
		completed = append(completed, completeUploadEntry{PartNumber: partNum, ETag: etag})
	}
	completeXML, err := xml.Marshal(completeMultipartUploadRequest{Parts: completed})
	if err != nil {
		t.Fatalf("marshal complete: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/bucket/mp-tamper?uploadId=%s", initRes.UploadID), bytes.NewReader(completeXML))
	rec = httptest.NewRecorder()
	h.CompleteMultipartUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CompleteMultipartUpload status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	// Tamper one part on the backend while preserving the exact
	// byte length (this is the case the aggregate object-size
	// check could not catch).
	fake.mu.Lock()
	var pickedID string
	for id := range fake.pieces {
		pickedID = id
		break
	}
	if pickedID == "" {
		fake.mu.Unlock()
		t.Fatal("no piece to tamper")
	}
	if got, want := len(fake.pieces[pickedID]), len(parts[0]); got != want {
		fake.mu.Unlock()
		t.Fatalf("picked piece size = %d, want %d", got, want)
	}
	fake.pieces[pickedID] = bytes.Repeat([]byte("zzzz"), 256)
	fake.mu.Unlock()

	req = httptest.NewRequest(http.MethodGet, "/bucket/mp-tamper", nil)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("tampered multipart GET status = %d, want 502; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "IntegrityCheckFailed") {
		t.Fatalf("tampered multipart GET body missing IntegrityCheckFailed: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "zzzz") {
		t.Fatalf("tampered multipart GET leaked backend bytes: %q", rec.Body.String())
	}
	if hits := sink.count("test"); hits != 1 {
		t.Fatalf("integrity hits after tampered multipart GET = %d, want 1", hits)
	}
}

// TestGetErasureCoded_TamperedShard_FailsClosed verifies an EC GET
// treats a hash-mismatched shard as lost. With a 6+2 profile and a
// single tampered shard the parity should still reconstruct, so the
// GET succeeds, but the integrity metric must fire so operators see
// the bit-rot signal even when parity hides it.
func TestGetErasureCoded_TamperedShard_FailsClosed(t *testing.T) {
	store := memory.New()
	fake := newFakeProvider("test")
	sink := &integritySinkRecorder{}
	h := New(Config{
		Manifests:         store,
		Providers:         map[string]providers.StorageProvider{"test": fake},
		Placement:         ecPlacement{backend: "test", profile: erasure_coding.Profile6Plus2.Name},
		ErasureCoding:     erasure_coding.DefaultRegistry(),
		IntegrityFailures: sink,
		Now:               func() time.Time { return time.Unix(1700000000, 0) },
	})

	body := bytes.Repeat([]byte("ec-tamper!"), 4096)
	req := httptest.NewRequest(http.MethodPut, "/bucket/ec-tamper", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("EC PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	// Tamper exactly one shard (within parity tolerance for
	// 6+2). The decode should still succeed by reconstructing
	// from the remaining 7 shards. Pick the first piece in the
	// fake backend; its length is preserved so the only thing
	// that flags it is the hash check.
	fake.mu.Lock()
	var pickedID string
	for id := range fake.pieces {
		pickedID = id
		break
	}
	if pickedID == "" {
		fake.mu.Unlock()
		t.Fatal("no piece to tamper")
	}
	shardLen := len(fake.pieces[pickedID])
	fake.pieces[pickedID] = bytes.Repeat([]byte{0xff}, shardLen)
	fake.mu.Unlock()

	req = httptest.NewRequest(http.MethodGet, "/bucket/ec-tamper", nil)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("EC GET with 1 tampered shard status = %d, want 200 (parity recovers); body=%s", rec.Code, rec.Body)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, body) {
		t.Fatalf("EC GET with 1 tampered shard returned wrong bytes: len(got)=%d, len(want)=%d", len(got), len(body))
	}
	if hits := sink.count("test"); hits != 1 {
		t.Fatalf("integrity hits after tampered EC GET = %d, want 1 (single shard mismatch)", hits)
	}
}

// TestGet_UnrecognizedHashFormat_ServesWithObservabilityCounter
// pins the legacy-manifest behaviour: when the manifest's
// Piece.Hash is non-empty but not in any recognised format (e.g.
// a legacy multipart / copy / dedup manifest that stamped a
// backend ETag into Hash), the verifier returns
// ErrIntegrityClaimUnrecognized rather than
// ErrIntegrityCheckFailed. The handler must serve the bytes
// (there is no proof they're wrong) and increment the dedicated
// observability counter so operators can plan a one-shot
// rewrite. The hard-fail counter must stay at zero.
func TestGet_UnrecognizedHashFormat_ServesWithObservabilityCounter(t *testing.T) {
	store := memory.New()
	fake := newFakeProvider("test")
	sink := &integritySinkRecorder{}
	h := New(Config{
		Manifests:         store,
		Providers:         map[string]providers.StorageProvider{"test": fake},
		Placement:         fixedPlacement{backend: "test"},
		IntegrityFailures: sink,
		Now:               func() time.Time { return time.Unix(1700000000, 0) },
	})

	body := []byte("legacy manifest with an opaque ETag in Hash")

	// PUT through the handler so the manifest exists. We will
	// then rewrite Piece.Hash on the stored manifest to simulate
	// what a pre-PR-2 multipart / copy / dedup writer would have
	// produced: an opaque backend ETag in the Hash slot, with the
	// piece bytes themselves untouched.
	req := httptest.NewRequest(http.MethodPut, "/bucket/legacy.txt", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	mkey := manifest_store.ManifestKey{
		TenantID:      "anonymous",
		Bucket:        "bucket",
		ObjectKeyHash: hashObjectKey("legacy.txt"),
	}
	man, err := store.Get(context.Background(), mkey)
	if err != nil {
		t.Fatalf("manifest get: %v", err)
	}
	if len(man.Pieces) != 1 {
		t.Fatalf("manifest pieces = %d, want 1", len(man.Pieces))
	}
	// Overwrite the recorded hash with a value that looks like an
	// AWS S3 opaque ETag (32-char lowercase hex without the
	// blake3: prefix). The verifier must classify this as
	// ErrIntegrityClaimUnrecognized, NOT as a SHA-256 hash that
	// happens to mismatch.
	man.Pieces[0].Hash = "d41d8cd98f00b204e9800998ecf8427e"
	if err := store.Put(context.Background(), mkey, man); err != nil {
		t.Fatalf("manifest put: %v", err)
	}

	// GET must serve the original body and emit ONLY the
	// observability counter; the hard-fail counter must stay
	// flat.
	req = httptest.NewRequest(http.MethodGet, "/bucket/legacy.txt", nil)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy-manifest GET status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != string(body) {
		t.Fatalf("legacy-manifest GET body = %q, want %q", got, body)
	}
	if hits := sink.count("test"); hits != 0 {
		t.Fatalf("integrity_failure counter = %d, want 0 (unrecognised hash != content mismatch)", hits)
	}
	if u := sink.countUnrecognized("test"); u != 1 {
		t.Fatalf("integrity_claim_unrecognized counter = %d, want 1", u)
	}

	// A second GET (with cache nil) must increment the counter
	// again -- the verifier does not silently latch the legacy
	// state, so each unverifiable GET adds to the count.
	req = httptest.NewRequest(http.MethodGet, "/bucket/legacy.txt", nil)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second legacy-manifest GET status = %d, want 200", rec.Code)
	}
	if u := sink.countUnrecognized("test"); u != 2 {
		t.Fatalf("integrity_claim_unrecognized counter after 2 GETs = %d, want 2", u)
	}
}

// TestAudit_RequestIDFromContext verifies that the audit record's
// RequestID field reads from the request context (where
// requestid.Middleware installs it) rather than from the request
// header (where the middleware does NOT install it for
// server-generated ids). Pre-fix, the audit code did
// r.Header.Get("x-amz-request-id"), which only caught the
// upstream-supplied case — server-generated ids leaked through
// as empty strings on the response header and the audit record
// despite the middleware setting them in the context. The fix
// flips the audit code to requestid.FromContext(r.Context()).
//
// The test drives a PUT through requestid.Middleware so the
// middleware's normal context-only installation is exercised
// (no x-amz-request-id on the inbound request), then asserts
// that (1) the audit record's RequestID is non-empty and (2)
// it matches the x-amz-request-id the middleware echoed back
// on the response header — proving they came from the same
// source.
func TestAudit_RequestIDFromContext(t *testing.T) {
	store := memory.New()
	fake := newFakeProvider("test")
	audit := &recordingAudit{}
	h := New(Config{
		Manifests:  store,
		Providers:  map[string]providers.StorageProvider{"test": fake},
		Placement:  fixedPlacement{backend: "test"},
		Compliance: ComplianceHooks{Audit: audit},
		Now:        func() time.Time { return time.Unix(1700000000, 0) },
	})

	// Wrap Handler.Put in requestid.Middleware so the request
	// context carries the middleware-installed id when audit()
	// reads it. The inbound request deliberately omits the
	// x-amz-request-id header so the middleware generates a
	// fresh id (the path the bug regressed).
	wrapped := requestid.Middleware(http.HandlerFunc(h.Put))

	body := []byte("audit-requestid-ctx")
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	responseID := rec.Header().Get(requestid.HeaderName)
	if responseID == "" {
		t.Fatal("response x-amz-request-id was empty; requestid.Middleware did not run or did not set the header")
	}

	if len(audit.entries) == 0 {
		t.Fatal("audit recorded no entries; PUT should have emitted one")
	}
	got := audit.entries[0].RequestID
	if got == "" {
		t.Fatalf("audit RequestID = %q; want the middleware-generated id %q. The pre-fix audit code read from r.Header which the middleware never sets — this regression test pins the FromContext path.", got, responseID)
	}
	if got != responseID {
		t.Errorf("audit RequestID = %q, response header x-amz-request-id = %q; they must match because both must come from requestid.FromContext(r.Context()) / requestid.Middleware's single source of truth.", got, responseID)
	}
}
