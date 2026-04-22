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

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/metadata"
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
	for i, name := range []string{"a", "b", "c"} {
		_ = i
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
	if len(r.Contents) != 3 {
		t.Errorf("LIST contents = %d, want 3", len(r.Contents))
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
