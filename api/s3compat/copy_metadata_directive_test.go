package s3compat

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// copyWithHeaders issues a CopyObject of /bucket/src into dst with an
// arbitrary set of request headers (x-amz-metadata-directive, x-amz-meta-*,
// system headers, …) and returns the recorder.
func copyWithHeaders(t *testing.T, h *Handler, dst string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	cr := httptest.NewRequest(http.MethodPut, dst, nil)
	cr.Header.Set("x-amz-copy-source", "/bucket/src")
	for k, v := range headers {
		cr.Header.Set(k, v)
	}
	cw := httptest.NewRecorder()
	h.Copy(cw, cr)
	return cw
}

// TestCopyObject_MetadataDirective covers x-amz-metadata-directive on
// CopyObject: the default (and explicit COPY) preserves the source object's
// stored metadata — previously dropped entirely — while REPLACE takes the
// destination metadata from the copy request's own headers, an unknown
// directive 400s, and an oversized REPLACE metadata set 400s, all before any
// bytes move. Assertions read the destination back through GET so the whole
// path (persist on copy → emit on read) is exercised end to end.
func TestCopyObject_MetadataDirective(t *testing.T) {
	srcHeaders := map[string]string{
		"Content-Type":        "image/png",
		"Content-Disposition": `attachment; filename="src.png"`,
		"Cache-Control":       "max-age=600",
		"x-amz-meta-owner":    "team-a",
		"x-amz-meta-env":      "prod",
	}

	seedSrc := func(t *testing.T, h *Handler) {
		t.Helper()
		if rec := putWithHeaders(t, h, "/bucket/src", []byte("payload"), srcHeaders); rec.Code != http.StatusOK {
			t.Fatalf("seed src = %d; body=%s", rec.Code, rec.Body)
		}
	}

	assertDstHeaders := func(t *testing.T, h *Handler, want map[string]string, wantAbsent ...string) {
		t.Helper()
		for _, verb := range []string{"GET", "HEAD"} {
			var rec *httptest.ResponseRecorder
			if verb == "GET" {
				rec = getObjectRec(t, h, "/bucket/dst")
			} else {
				rec = headObject(t, h, "/bucket/dst")
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("%s dst = %d, want 200; body=%s", verb, rec.Code, rec.Body)
			}
			for name, val := range want {
				if got := rec.Header().Get(name); got != val {
					t.Errorf("%s dst %s = %q, want %q", verb, name, got, val)
				}
			}
			for _, name := range wantAbsent {
				if got := rec.Header().Get(name); got != "" {
					t.Errorf("%s dst %s = %q, want empty", verb, name, got)
				}
			}
		}
	}

	t.Run("default preserves source metadata", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		seedSrc(t, h)
		if rec := copyWithHeaders(t, h, "/bucket/dst", nil); rec.Code != http.StatusOK {
			t.Fatalf("copy = %d; body=%s", rec.Code, rec.Body)
		}
		assertDstHeaders(t, h, map[string]string{
			"Content-Type":        "image/png",
			"Content-Disposition": `attachment; filename="src.png"`,
			"Cache-Control":       "max-age=600",
			"x-amz-meta-owner":    "team-a",
			"x-amz-meta-env":      "prod",
		})
	})

	t.Run("explicit COPY preserves source metadata", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		seedSrc(t, h)
		if rec := copyWithHeaders(t, h, "/bucket/dst", map[string]string{"x-amz-metadata-directive": "COPY"}); rec.Code != http.StatusOK {
			t.Fatalf("copy = %d; body=%s", rec.Code, rec.Body)
		}
		assertDstHeaders(t, h, map[string]string{
			"Content-Type":     "image/png",
			"x-amz-meta-owner": "team-a",
		})
	})

	t.Run("REPLACE takes request metadata", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		seedSrc(t, h)
		rec := copyWithHeaders(t, h, "/bucket/dst", map[string]string{
			"x-amz-metadata-directive": "REPLACE",
			"Content-Type":             "application/json",
			"Content-Language":         "fr",
			"x-amz-meta-stage":         "copy",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("copy = %d; body=%s", rec.Code, rec.Body)
		}
		// New metadata applied; the source's metadata is gone.
		assertDstHeaders(t, h,
			map[string]string{
				"Content-Type":     "application/json",
				"Content-Language": "fr",
				"x-amz-meta-stage": "copy",
			},
			"Content-Disposition", "Cache-Control", "x-amz-meta-owner", "x-amz-meta-env",
		)
	})

	t.Run("REPLACE with no headers defaults content-type and drops metadata", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		seedSrc(t, h)
		if rec := copyWithHeaders(t, h, "/bucket/dst", map[string]string{"x-amz-metadata-directive": "REPLACE"}); rec.Code != http.StatusOK {
			t.Fatalf("copy = %d; body=%s", rec.Code, rec.Body)
		}
		assertDstHeaders(t, h,
			map[string]string{"Content-Type": defaultContentType},
			"Content-Disposition", "Cache-Control", "x-amz-meta-owner", "x-amz-meta-env",
		)
	})

	t.Run("invalid directive is 400", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		seedSrc(t, h)
		if rec := copyWithHeaders(t, h, "/bucket/dst", map[string]string{"x-amz-metadata-directive": "MERGE"}); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid directive = %d, want 400; body=%s", rec.Code, rec.Body)
		}
		// The destination must not have been created.
		if rec := headObject(t, h, "/bucket/dst"); rec.Code == http.StatusOK {
			t.Fatalf("dst should not exist after a rejected copy")
		}
	})

	t.Run("REPLACE with oversized metadata is 400", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		seedSrc(t, h)
		rec := copyWithHeaders(t, h, "/bucket/dst", map[string]string{
			"x-amz-metadata-directive": "REPLACE",
			"x-amz-meta-big":           strings.Repeat("a", maxUserMetadataBytes+1),
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("oversized REPLACE metadata = %d, want 400; body=%s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "MetadataTooLarge") {
			t.Errorf("error body = %s, want MetadataTooLarge", rec.Body)
		}
	})
}
