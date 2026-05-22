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
	"testing"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
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

// TestDispatch_MaxRequestBytes_DeleteWrapInstalled verifies the
// per-request body cap is method-agnostic. The dispatch wrap is
// applied to DELETE requests with a non-empty body even though
// the current Delete handler does not read r.Body — defence in
// depth for any future handler that starts consuming the body.
//
// Why a behavioural 413 assertion is NOT the right test here:
// http.MaxBytesReader only delivers 413 when the wrapped reader
// is actually Read(); since the current Delete handler returns
// 204 without touching r.Body, the cap is inert at the response
// layer. The test instead verifies the structural invariant —
// after dispatch's wrap step, r.Body has been replaced with a
// MaxBytesReader (different from the body the test installed) —
// which is the load-bearing property that future body-reading
// DELETE handlers depend on.
func TestDispatch_MaxRequestBytes_DeleteWrapInstalled(t *testing.T) {
	const limit int64 = 1024
	body := bytes.Repeat([]byte("A"), 4*1024)

	fake := newFakeProvider("test")
	store := memory.New()
	h := New(Config{
		Manifests:       store,
		Providers:       map[string]providers.StorageProvider{"test": fake},
		Placement:       fixedPlacement{backend: "test"},
		MaxRequestBytes: limit,
	})

	// Install a known-identity body so we can verify the wrap
	// replaced it. capRequestBody (factored out of dispatch for
	// testability) is the structural seam where the wrap is
	// applied; running it directly avoids the false-promise
	// issue of asserting 413 against a handler that doesn't
	// read r.Body.
	originalBody := bytes.NewReader(body)
	req := httptest.NewRequest(http.MethodDelete, "/bucket/key", io.NopCloser(originalBody))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()

	wrapped := h.capRequestBody(rec, req)
	if !wrapped {
		t.Fatal("capRequestBody returned false on DELETE with non-empty body; the wrap was skipped")
	}
	// After the wrap, reading from r.Body past the limit must
	// surface the cap as an error — the proof that the wrap is
	// MaxBytesReader, not the original body. We discard up to
	// limit+1 bytes; the read past the limit must fail.
	n, err := io.Copy(io.Discard, io.LimitReader(req.Body, limit+1))
	if err == nil {
		t.Fatalf("DELETE body read past the cap returned nil error; copied %d bytes — wrap was not installed", n)
	}
	var mb *http.MaxBytesError
	if !errors.As(err, &mb) {
		t.Fatalf("DELETE body read error was %T %v; want *http.MaxBytesError (proves the wrap is MaxBytesReader, not the raw body)", err, err)
	}
	if mb.Limit != limit {
		t.Errorf("MaxBytesError.Limit = %d, want %d", mb.Limit, limit)
	}
}

// TestDispatch_MaxRequestBytes_DeleteWithoutBodyStillWorks
// confirms the method-agnostic wrap does not regress the
// no-body DELETE path (the only legitimate S3 DeleteObject
// shape). net/http leaves r.Body == http.NoBody on a DELETE
// without a body, and the outer dispatch guard
// (r.Body != http.NoBody) short-circuits the wrap entirely.
func TestDispatch_MaxRequestBytes_DeleteWithoutBodyStillWorks(t *testing.T) {
	const limit int64 = 16
	fake := newFakeProvider("test")
	store := memory.New()
	h := New(Config{
		Manifests:       store,
		Providers:       map[string]providers.StorageProvider{"test": fake},
		Placement:       fixedPlacement{backend: "test"},
		MaxRequestBytes: limit,
	})

	// Seed an object so DELETE has something to remove.
	putReq := httptest.NewRequest(http.MethodPut, "/bucket/k", bytes.NewReader([]byte("ok")))
	putReq.ContentLength = 2
	putRec := httptest.NewRecorder()
	h.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("setup PUT = %d, want 200; body=%s", putRec.Code, putRec.Body)
	}

	// DELETE with no body (the standard S3 DeleteObject shape)
	// must not surface 413 even with a tiny cap.
	delReq := httptest.NewRequest(http.MethodDelete, "/bucket/k", nil)
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent && delRec.Code != http.StatusOK {
		t.Fatalf("DELETE without body = %d, want 204 or 200; body=%s", delRec.Code, delRec.Body)
	}
}

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

// TestDispatch_MaxRequestBytes_UploadPartStreamingRejectsMidStream
// confirms the cap fires on the multipart UploadPart streaming
// path the same way it fires on the single-piece Put streaming
// path. Before this regression test the UploadPart PutPiece
// error branch returned a generic 502 BackendPutFailed when the
// body overflowed the cap because the error check only looked at
// the read error class, not whether it wrapped
// *http.MaxBytesError. The shared writePutPieceError helper now
// converts those into 413 EntityTooLarge so multipart clients
// see the same actionable error as single-piece clients.
func TestDispatch_MaxRequestBytes_UploadPartStreamingRejectsMidStream(t *testing.T) {
	const limit int64 = 1024
	body := bytes.Repeat([]byte("B"), 4*1024)

	fake := newFakeProvider("test")
	mpStore := multipart.NewMemoryStore()
	h := New(Config{
		Manifests:       memory.New(),
		Providers:       map[string]providers.StorageProvider{"test": fake},
		Placement:       fixedPlacement{backend: "test"},
		Multipart:       mpStore,
		MaxRequestBytes: limit,
	})

	// Create the multipart upload first.
	createReq := httptest.NewRequest(http.MethodPost, "/bucket/mp-obj?uploads", nil)
	createRec := httptest.NewRecorder()
	h.CreateMultipartUpload(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("CreateMultipartUpload status = %d, want 200; body=%s", createRec.Code, createRec.Body)
	}
	var initRes initiateMultipartUploadResult
	if err := xml.Unmarshal(createRec.Body.Bytes(), &initRes); err != nil {
		t.Fatalf("decode initiate: %v", err)
	}
	uploadID := initRes.UploadID

	// Upload a part whose body exceeds MaxRequestBytes. Stream
	// the body through a lyingReader so MaxBytesReader cannot
	// short-circuit on ContentLength and the cap must fire
	// mid-stream — same shape as the single-piece streaming
	// rejection test above.
	url := fmt.Sprintf("/bucket/mp-obj?uploadId=%s&partNumber=1", uploadID)
	partReq := httptest.NewRequest(http.MethodPut, url, &lyingReader{r: bytes.NewReader(body)})
	partReq.ContentLength = -1
	partRec := httptest.NewRecorder()
	h.ServeHTTP(partRec, partReq)

	if partRec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("UploadPart streaming status = %d, want 413; body=%s", partRec.Code, partRec.Body)
	}
	if !strings.Contains(partRec.Body.String(), "EntityTooLarge") {
		t.Errorf("UploadPart body missing EntityTooLarge: %s", partRec.Body)
	}
	if !strings.Contains(partRec.Body.String(), fmt.Sprintf("%d", limit)) {
		t.Errorf("UploadPart body should quote the configured limit %d; got: %s", limit, partRec.Body)
	}
}

// TestDispatch_MaxRequestBytes_ErasureCodedRejectsOverlimit
// confirms the cap also fires on the erasure-coded PUT path,
// which has its own io.ReadAll(LimitReader(...)) sized to
// maxECObjectSize. Before this regression the EC path used a
// hardcoded 400 InvalidArgument for body-read errors; the fix
// routes through writeBodyReadError so EC clients see the same
// 413 EntityTooLarge as every other body-reading path. We use a
// small MaxRequestBytes here because the test should run fast;
// the same code path would fire on a large EC object against
// the production 5 GiB default.
func TestDispatch_MaxRequestBytes_ErasureCodedRejectsOverlimit(t *testing.T) {
	const limit int64 = 1024
	body := bytes.Repeat([]byte("E"), 4*1024)

	fake := newFakeProvider("test")
	h := New(Config{
		Manifests: memory.New(),
		Providers: map[string]providers.StorageProvider{"test": fake},
		Placement: ecPlacement{backend: "test", profile: erasure_coding.Profile6Plus2.Name},
		// Route the upload through the erasure-coded handler.
		ErasureCoding:   erasure_coding.DefaultRegistry(),
		MaxRequestBytes: limit,
	})

	req := httptest.NewRequest(http.MethodPut, "/bucket/ec-obj", &lyingReader{r: bytes.NewReader(body)})
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("EC PUT status = %d, want 413; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "EntityTooLarge") {
		t.Errorf("EC PUT body missing EntityTooLarge: %s", rec.Body)
	}
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
