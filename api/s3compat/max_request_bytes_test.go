package s3compat

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// TestDispatch_MaxRequestBytes_PutTooLarge confirms the
// per-request body cap surfaces 413 RequestEntityTooLarge with
// the S3 error code "EntityTooLarge" when a PUT body exceeds
// Config.MaxRequestBytes. The dispatch wraps r.Body in
// http.MaxBytesReader before any io.ReadAll / TeeReader touches
// it, so the cap fires regardless of which downstream sub-handler
// picks up the request.
func TestDispatch_MaxRequestBytes_PutTooLarge(t *testing.T) {
	const limit int64 = 1024
	// 4 KiB > 1 KiB limit
	body := bytes.Repeat([]byte("A"), 4*1024)

	fake := newFakeProvider("test")
	h := New(Config{
		Manifests:       memory.New(),
		Providers:       map[string]providers.StorageProvider{"test": fake},
		Placement:       fixedPlacement{backend: "test"},
		MaxRequestBytes: limit,
	})

	req := httptest.NewRequest(http.MethodPut, "/bucket/key", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "EntityTooLarge") {
		t.Errorf("response body missing S3 error code EntityTooLarge: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), fmt.Sprintf("%d", limit)) {
		t.Errorf("response body should quote the configured limit %d so the caller can retry with the right chunk size; got: %s", limit, rec.Body)
	}
	if len(fake.pieces) != 0 {
		t.Errorf("oversized PUT wrote %d pieces, want 0", len(fake.pieces))
	}
}

// TestDispatch_MaxRequestBytes_PutAtLimit confirms a PUT exactly
// at the limit still succeeds — the cap is inclusive in the sense
// that the request must not strictly exceed it. This guards against
// off-by-one regressions where the cap is enforced as a strictly
// less-than comparison.
func TestDispatch_MaxRequestBytes_PutAtLimit(t *testing.T) {
	const limit int64 = 1024
	body := bytes.Repeat([]byte("A"), int(limit))

	fake := newFakeProvider("test")
	h := New(Config{
		Manifests:       memory.New(),
		Providers:       map[string]providers.StorageProvider{"test": fake},
		Placement:       fixedPlacement{backend: "test"},
		MaxRequestBytes: limit,
	})

	req := httptest.NewRequest(http.MethodPut, "/bucket/key", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT at limit status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if len(fake.pieces) != 1 {
		t.Errorf("PUT at limit wrote %d pieces, want 1", len(fake.pieces))
	}
}

// TestDispatch_MaxRequestBytes_ZeroDisables confirms a zero cap
// leaves the legacy unbounded behaviour in place, so callers that
// upgrade without setting MaxRequestBytes keep working. This is
// the load-bearing non-breaking promise.
func TestDispatch_MaxRequestBytes_ZeroDisables(t *testing.T) {
	body := bytes.Repeat([]byte("A"), 8*1024)

	fake := newFakeProvider("test")
	h := New(Config{
		Manifests: memory.New(),
		Providers: map[string]providers.StorageProvider{"test": fake},
		Placement: fixedPlacement{backend: "test"},
		// MaxRequestBytes intentionally unset (== 0)
	})

	req := httptest.NewRequest(http.MethodPut, "/bucket/key", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT with zero MaxRequestBytes = %d, want 200 (legacy behaviour); body=%s", rec.Code, rec.Body)
	}
}

// TestDispatch_MaxRequestBytes_DoesNotApplyToGet confirms the cap
// only fires on body-consuming methods. GET / HEAD must not see
// 413 even when the URL or query string is large — those have no
// body for MaxBytesReader to attach to and net/http leaves
// r.Body as http.NoBody anyway.
func TestDispatch_MaxRequestBytes_DoesNotApplyToGet(t *testing.T) {
	const limit int64 = 16
	fake := newFakeProvider("test")
	store := memory.New()
	h := New(Config{
		Manifests:       store,
		Providers:       map[string]providers.StorageProvider{"test": fake},
		Placement:       fixedPlacement{backend: "test"},
		MaxRequestBytes: limit,
	})

	// First write a small object so the GET has something to
	// fetch. The PUT is within the 16-byte limit.
	putReq := httptest.NewRequest(http.MethodPut, "/bucket/k", bytes.NewReader([]byte("ok")))
	putReq.ContentLength = 2
	putRec := httptest.NewRecorder()
	h.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("setup PUT = %d, want 200; body=%s", putRec.Code, putRec.Body)
	}

	// GET the object back; no body, so MaxBytesReader is a no-op.
	getReq := httptest.NewRequest(http.MethodGet, "/bucket/k", nil)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", getRec.Code, getRec.Body)
	}
}

// TestDispatch_MaxRequestBytes_StreamingPutRejectsMidStream
// confirms the cap fires on the streaming PUT path (where the
// provider drains r.Body directly via the TeeReader) rather than
// only on the buffered read paths. ContentLength is set to a
// value below the limit to trick the early-exit checks in the
// stdlib MaxBytesReader; the actual body is larger and triggers
// the error mid-stream.
func TestDispatch_MaxRequestBytes_StreamingPutRejectsMidStream(t *testing.T) {
	const limit int64 = 1024
	body := bytes.Repeat([]byte("A"), 4*1024)

	fake := newFakeProvider("test")
	h := New(Config{
		Manifests:       memory.New(),
		Providers:       map[string]providers.StorageProvider{"test": fake},
		Placement:       fixedPlacement{backend: "test"},
		MaxRequestBytes: limit,
	})

	// Wrap the body in a lying-reader that under-reports its
	// length so the stdlib short-circuit on ContentLength does
	// not engage. The MaxBytesReader still trips on the actual
	// byte count.
	req := httptest.NewRequest(http.MethodPut, "/bucket/key", &lyingReader{r: bytes.NewReader(body)})
	req.ContentLength = -1 // unknown; forces streaming
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("streaming PUT status = %d, want 413; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "EntityTooLarge") {
		t.Errorf("streaming PUT body missing EntityTooLarge: %s", rec.Body)
	}
}

// lyingReader wraps a bytes.Reader and lies about its content
// length to force the streaming PUT path. It satisfies io.Reader.
type lyingReader struct{ r *bytes.Reader }

func (l *lyingReader) Read(p []byte) (int, error) { return l.r.Read(p) }

// TestDispatch_MaxRequestBytes_DefaultDoesNotBreakRoundtrip is a
// regression test against the cap accidentally tripping on a
// normal small-object PUT/GET with the production-ish 5 GiB
// default. The PUT body is ~1 KiB and must succeed.
func TestDispatch_MaxRequestBytes_DefaultDoesNotBreakRoundtrip(t *testing.T) {
	body := bytes.Repeat([]byte("A"), 1024)

	fake := newFakeProvider("test")
	h := New(Config{
		Manifests:       memory.New(),
		Providers:       map[string]providers.StorageProvider{"test": fake},
		Placement:       fixedPlacement{backend: "test"},
		MaxRequestBytes: 5 * 1024 * 1024 * 1024, // 5 GiB, the package default
	})

	req := httptest.NewRequest(http.MethodPut, "/bucket/key", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT with 5 GiB cap = %d, want 200; body=%s", rec.Code, rec.Body)
	}
}

// ServeHTTP exposes the package-internal dispatch through the
// http.Handler interface so tests can drive the full middleware
// chain rather than calling Put / Get directly.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(w, r)
}

// drainXMLError is a tiny helper used by table tests below. It
// returns the parsed S3 error body and bubbles up a t.Helper
// failure when the body does not parse — keeps the call sites
// concise.
func drainXMLError(t *testing.T, body io.Reader) s3ErrorResponse {
	t.Helper()
	var out s3ErrorResponse
	if err := xml.NewDecoder(body).Decode(&out); err != nil {
		t.Fatalf("parse S3 error body: %v", err)
	}
	return out
}

// silence "imported and not used" warnings on the optional
// imports above when running with stripped builds.
var _ = context.Background
var _ = drainXMLError
