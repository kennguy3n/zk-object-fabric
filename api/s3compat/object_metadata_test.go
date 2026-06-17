package s3compat

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// putWithHeaders seeds an object through the live Put handler with an
// arbitrary set of request headers (Content-Type, x-amz-meta-*, …) and
// returns the recorder.
func putWithHeaders(t *testing.T, h *Handler, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	return rec
}

func getObjectRec(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func headObject(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Head(rec, httptest.NewRequest(http.MethodHead, path, nil))
	return rec
}

// TestObjectMetadata_Roundtrip pins that the system HTTP headers and
// x-amz-meta-* user metadata supplied at PUT are persisted and replayed
// identically on both GET and HEAD.
func TestObjectMetadata_Roundtrip(t *testing.T) {
	h, _, _, _ := newTestHandler()

	headers := map[string]string{
		"Content-Type":        "image/png",
		"Content-Encoding":    "gzip",
		"Content-Disposition": `attachment; filename="report.pdf"`,
		"Content-Language":    "en-US",
		"Cache-Control":       "max-age=3600",
		"Expires":             "Wed, 21 Oct 2026 07:28:00 GMT",
		"x-amz-meta-team":     "storage",
		"x-amz-meta-Reviewed": "yes",
	}
	if rec := putWithHeaders(t, h, "/bucket/meta-obj", []byte("payload"), headers); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	want := map[string]string{
		"Content-Type":        "image/png",
		"Content-Encoding":    "gzip",
		"Content-Disposition": `attachment; filename="report.pdf"`,
		"Content-Language":    "en-US",
		"Cache-Control":       "max-age=3600",
		"Expires":             "Wed, 21 Oct 2026 07:28:00 GMT",
		// Header names are case-insensitive; canonicalized on the wire.
		"x-amz-meta-team":     "storage",
		"x-amz-meta-reviewed": "yes",
	}

	for _, verb := range []string{"GET", "HEAD"} {
		var rec *httptest.ResponseRecorder
		if verb == "GET" {
			rec = getObjectRec(t, h, "/bucket/meta-obj")
		} else {
			rec = headObject(t, h, "/bucket/meta-obj")
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200; body=%s", verb, rec.Code, rec.Body)
		}
		for name, val := range want {
			if got := rec.Header().Get(name); got != val {
				t.Errorf("%s %s = %q, want %q", verb, name, got, val)
			}
		}
	}
}

// TestObjectMetadata_DefaultContentType pins that an object stored
// without a Content-Type is served as binary/octet-stream (matching
// AWS), not with an empty or sniffed type, and carries no stray system
// or user-metadata headers.
func TestObjectMetadata_DefaultContentType(t *testing.T) {
	h, _, _, _ := newTestHandler()

	if rec := putWithHeaders(t, h, "/bucket/plain", []byte("payload"), nil); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	rec := getObjectRec(t, h, "/bucket/plain")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != defaultContentType {
		t.Errorf("Content-Type = %q, want %q", got, defaultContentType)
	}
	for _, h := range []string{"Content-Encoding", "Content-Disposition", "Cache-Control", "Expires"} {
		if got := rec.Header().Get(h); got != "" {
			t.Errorf("%s = %q, want empty", h, got)
		}
	}
}

// TestObjectMetadata_ErasureCoded proves the metadata round-trip is wired
// through the erasure-coded write/read paths, not just single-piece.
func TestObjectMetadata_ErasureCoded(t *testing.T) {
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
	headers := map[string]string{
		"Content-Type":    "application/zip",
		"x-amz-meta-tier": "cold",
	}
	if rec := putWithHeaders(t, h, "/bucket/ec-obj", body, headers); rec.Code != http.StatusOK {
		t.Fatalf("EC PUT = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	rec := getObjectRec(t, h, "/bucket/ec-obj")
	if rec.Code != http.StatusOK {
		t.Fatalf("EC GET = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/zip" {
		t.Errorf("EC Content-Type = %q, want application/zip", got)
	}
	if got := rec.Header().Get("x-amz-meta-tier"); got != "cold" {
		t.Errorf("EC x-amz-meta-tier = %q, want cold", got)
	}
}

// TestObjectMetadata_TooLarge pins that an over-limit user-metadata set
// is rejected with 400 MetadataTooLarge BEFORE any backend write — no
// partial object is left behind.
func TestObjectMetadata_TooLarge(t *testing.T) {
	h, fake, _, _ := newTestHandler()

	rec := putWithHeaders(t, h, "/bucket/big-meta", []byte("payload"), map[string]string{
		"x-amz-meta-blob": strings.Repeat("x", maxUserMetadataBytes+1),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized metadata PUT = %d, want 400; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "MetadataTooLarge") {
		t.Errorf("error body = %s, want MetadataTooLarge", rec.Body)
	}
	if len(fake.pieces) != 0 {
		t.Errorf("backend pieces after rejected PUT = %d, want 0 (no partial write)", len(fake.pieces))
	}
	if rec := getObjectRec(t, h, "/bucket/big-meta"); rec.Code != http.StatusNotFound {
		t.Errorf("GET after rejected PUT = %d, want 404", rec.Code)
	}
}

// TestCollectUserMetadata unit-tests the header→map extraction in
// isolation: prefix stripping, lower-casing, and nil-when-absent.
func TestCollectUserMetadata(t *testing.T) {
	h := http.Header{}
	if got := collectUserMetadata(h); got != nil {
		t.Errorf("no headers = %v, want nil", got)
	}
	h.Set("x-amz-meta-Foo", "bar")
	h.Set("Content-Type", "text/plain")
	h.Set("x-amz-meta-Empty-Key-Test", "v")
	got := collectUserMetadata(h)
	want := map[string]string{"foo": "bar", "empty-key-test": "v"}
	if len(got) != len(want) {
		t.Fatalf("collected = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("collected[%q] = %q, want %q", k, got[k], v)
		}
	}
}
