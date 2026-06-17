package s3compat

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// getWithQuery / headWithQuery issue a GET/HEAD for an object with an
// arbitrary set of query parameters (the S3 response-* overrides) and
// return the recorder.
func getWithQuery(t *testing.T, h *Handler, path string, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, path+"?"+q.Encode(), nil))
	return rec
}

func headWithQuery(t *testing.T, h *Handler, path string, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Head(rec, httptest.NewRequest(http.MethodHead, path+"?"+q.Encode(), nil))
	return rec
}

// TestResponseOverrides pins the S3 response-* query parameters on
// GetObject/HeadObject: each one overrides the matching response header
// (over the object's stored metadata or the default Content-Type), an
// absent parameter leaves the stored value intact, and the override set
// is identical for GET and HEAD.
func TestResponseOverrides(t *testing.T) {
	stored := map[string]string{
		"Content-Type":        "image/png",
		"Content-Disposition": `attachment; filename="src.png"`,
		"Cache-Control":       "max-age=600",
	}

	override := url.Values{
		"response-content-type":        {"application/json"},
		"response-content-encoding":    {"gzip"},
		"response-content-disposition": {`attachment; filename="report.pdf"`},
		"response-content-language":    {"fr-FR"},
		"response-cache-control":       {"no-store"},
		"response-expires":             {"Wed, 21 Oct 2026 07:28:00 GMT"},
	}
	want := map[string]string{
		"Content-Type":        "application/json",
		"Content-Encoding":    "gzip",
		"Content-Disposition": `attachment; filename="report.pdf"`,
		"Content-Language":    "fr-FR",
		"Cache-Control":       "no-store",
		"Expires":             "Wed, 21 Oct 2026 07:28:00 GMT",
	}

	t.Run("override every header on GET and HEAD", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		if rec := putWithHeaders(t, h, "/bucket/obj", []byte("payload"), stored); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", rec.Code, rec.Body)
		}
		for _, verb := range []string{"GET", "HEAD"} {
			var rec *httptest.ResponseRecorder
			if verb == "GET" {
				rec = getWithQuery(t, h, "/bucket/obj", override)
			} else {
				rec = headWithQuery(t, h, "/bucket/obj", override)
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
	})

	t.Run("absent parameters leave stored metadata intact", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		if rec := putWithHeaders(t, h, "/bucket/obj", []byte("payload"), stored); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", rec.Code, rec.Body)
		}
		// Only Content-Type is overridden; the rest fall through to stored.
		rec := getWithQuery(t, h, "/bucket/obj", url.Values{"response-content-type": {"text/plain"}})
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d; body=%s", rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Content-Type"); got != "text/plain" {
			t.Errorf("Content-Type = %q, want %q", got, "text/plain")
		}
		if got := rec.Header().Get("Content-Disposition"); got != stored["Content-Disposition"] {
			t.Errorf("Content-Disposition = %q, want stored %q", got, stored["Content-Disposition"])
		}
		if got := rec.Header().Get("Cache-Control"); got != stored["Cache-Control"] {
			t.Errorf("Cache-Control = %q, want stored %q", got, stored["Cache-Control"])
		}
	})

	t.Run("response-content-type overrides the default for an untyped object", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		if rec := putWithHeaders(t, h, "/bucket/plain", []byte("payload"), nil); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", rec.Code, rec.Body)
		}
		// Without an override the object would be served binary/octet-stream.
		if rec := getObjectRec(t, h, "/bucket/plain"); rec.Header().Get("Content-Type") != defaultContentType {
			t.Fatalf("control Content-Type = %q, want %q", rec.Header().Get("Content-Type"), defaultContentType)
		}
		rec := getWithQuery(t, h, "/bucket/plain", url.Values{"response-content-type": {"application/pdf"}})
		if got := rec.Header().Get("Content-Type"); got != "application/pdf" {
			t.Errorf("Content-Type = %q, want %q", got, "application/pdf")
		}
	})
}
