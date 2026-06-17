package s3compat

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// putWithTagging seeds an object through the live Put handler with an
// optional x-amz-tagging header and returns the recorder so callers can
// assert both the inline-tagging success and rejection paths.
func putWithTagging(t *testing.T, h *Handler, path, tagging string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader([]byte("payload")))
	req.ContentLength = int64(len("payload"))
	if tagging != "" {
		req.Header.Set("x-amz-tagging", tagging)
	}
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	return rec
}

// objectTags reads an object's tag set back through the GET ?tagging
// subresource (the same path PutObjectTagging round-trips through),
// returning the HTTP status and the decoded key→value map.
func objectTags(t *testing.T, h *Handler, path string) (int, map[string]string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path+"?tagging", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var doc taggingDocument
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal tags: %v; body=%s", err, rec.Body)
	}
	out := make(map[string]string, len(doc.TagSet.Tags))
	for _, tg := range doc.TagSet.Tags {
		out[tg.Key] = tg.Value
	}
	return rec.Code, out
}

func tagsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestPutObject_InlineTagging covers the x-amz-tagging header on
// PutObject: tags are set at creation, URL-encoding is decoded, an
// absent header creates an untagged object, and the header replaces any
// need for a follow-up ?tagging round-trip.
func TestPutObject_InlineTagging(t *testing.T) {
	h, _, _, _ := newTestHandler()

	if rec := putWithTagging(t, h, "/bucket/tagged", "env=prod&team=storage"); rec.Code != http.StatusOK {
		t.Fatalf("PUT inline tagging = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	code, tags := objectTags(t, h, "/bucket/tagged")
	if code != http.StatusOK {
		t.Fatalf("GET ?tagging = %d, want 200", code)
	}
	if want := map[string]string{"env": "prod", "team": "storage"}; !tagsEqual(tags, want) {
		t.Fatalf("inline tags = %v, want %v", tags, want)
	}

	// Percent-encoding in the query string is decoded.
	if rec := putWithTagging(t, h, "/bucket/encoded", "project=a%20b&path=x%2Fy"); rec.Code != http.StatusOK {
		t.Fatalf("PUT encoded tagging = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	_, tags = objectTags(t, h, "/bucket/encoded")
	if want := map[string]string{"project": "a b", "path": "x/y"}; !tagsEqual(tags, want) {
		t.Fatalf("decoded tags = %v, want %v", tags, want)
	}

	// No header: object is created untagged.
	if rec := putWithTagging(t, h, "/bucket/plain", ""); rec.Code != http.StatusOK {
		t.Fatalf("PUT no tagging = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if _, tags = objectTags(t, h, "/bucket/plain"); len(tags) != 0 {
		t.Fatalf("untagged object tags = %v, want empty", tags)
	}
}

// TestPutObject_InlineTagging_Rejected pins that a malformed or
// over-limit x-amz-tagging header fails with 400 BEFORE the object is
// written — no partial object, no partial tag set.
func TestPutObject_InlineTagging_Rejected(t *testing.T) {
	tooMany := make([]string, 0, maxObjectTags+1)
	for i := 0; i < maxObjectTags+1; i++ {
		tooMany = append(tooMany, fmt.Sprintf("k%d=v", i))
	}

	cases := []struct {
		name    string
		tagging string
	}{
		{"too many tags", strings.Join(tooMany, "&")},
		{"duplicate keys", "dup=1&dup=2"},
		{"empty key", "=v"},
		{"key too long", strings.Repeat("k", maxTagKeyLength+1) + "=v"},
		{"value too long", "k=" + strings.Repeat("v", maxTagValueLength+1)},
		{"malformed encoding", "k=%zz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _ := newTestHandler()
			rec := putWithTagging(t, h, "/bucket/obj", tc.tagging)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT (%s) = %d, want 400; body=%s", tc.name, rec.Code, rec.Body)
			}
			// The guard runs before the backend write, so nothing
			// was persisted: the object must not exist.
			if code, _ := objectTags(t, h, "/bucket/obj"); code != http.StatusNotFound {
				t.Fatalf("after rejected PUT, GET ?tagging = %d, want 404 (no object written)", code)
			}
		})
	}
}

// TestPutObject_InlineTagging_ErasureCoded proves the inline tag set is
// wired into the erasure-coded write path too, not just the single-piece
// path — the manifest persisted by putErasureCoded carries the tags.
func TestPutObject_InlineTagging_ErasureCoded(t *testing.T) {
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
	req.Header.Set("x-amz-tagging", "env=prod&tier=cold")
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("EC PUT inline tagging = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	mkey := manifest_store.ManifestKey{
		TenantID:      AnonymousTenant,
		Bucket:        "bucket",
		ObjectKeyHash: hashObjectKey("ec-obj"),
	}
	man, err := store.Get(context.Background(), mkey)
	if err != nil {
		t.Fatalf("manifest get: %v", err)
	}
	if want := map[string]string{"env": "prod", "tier": "cold"}; !tagsEqual(man.Tags, want) {
		t.Fatalf("EC manifest tags = %v, want %v", man.Tags, want)
	}
}

// TestCopyObject_TaggingDirective covers x-amz-tagging-directive on
// CopyObject: the default (and explicit COPY) preserves the source's
// tags — previously dropped entirely — while REPLACE takes the
// destination tags from x-amz-tagging, and an unknown directive 400s.
func TestCopyObject_TaggingDirective(t *testing.T) {
	copyTo := func(t *testing.T, h *Handler, dst string, headers map[string]string) *httptest.ResponseRecorder {
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

	srcTags := map[string]string{"env": "prod", "owner": "team-a"}

	t.Run("default preserves source tags", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		if rec := putWithTagging(t, h, "/bucket/src", "env=prod&owner=team-a"); rec.Code != http.StatusOK {
			t.Fatalf("seed src = %d; body=%s", rec.Code, rec.Body)
		}
		if rec := copyTo(t, h, "/bucket/dst", nil); rec.Code != http.StatusOK {
			t.Fatalf("copy = %d; body=%s", rec.Code, rec.Body)
		}
		if _, tags := objectTags(t, h, "/bucket/dst"); !tagsEqual(tags, srcTags) {
			t.Fatalf("default-copy dst tags = %v, want %v (source set)", tags, srcTags)
		}
	})

	t.Run("explicit COPY preserves source tags", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		putWithTagging(t, h, "/bucket/src", "env=prod&owner=team-a")
		if rec := copyTo(t, h, "/bucket/dst", map[string]string{"x-amz-tagging-directive": "COPY"}); rec.Code != http.StatusOK {
			t.Fatalf("copy = %d; body=%s", rec.Code, rec.Body)
		}
		if _, tags := objectTags(t, h, "/bucket/dst"); !tagsEqual(tags, srcTags) {
			t.Fatalf("COPY-directive dst tags = %v, want %v", tags, srcTags)
		}
	})

	t.Run("REPLACE sets new tags", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		putWithTagging(t, h, "/bucket/src", "env=prod&owner=team-a")
		rec := copyTo(t, h, "/bucket/dst", map[string]string{
			"x-amz-tagging-directive": "REPLACE",
			"x-amz-tagging":           "stage=copy&keep=yes",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("copy = %d; body=%s", rec.Code, rec.Body)
		}
		if _, tags := objectTags(t, h, "/bucket/dst"); !tagsEqual(tags, map[string]string{"stage": "copy", "keep": "yes"}) {
			t.Fatalf("REPLACE dst tags = %v, want the x-amz-tagging set", tags)
		}
	})

	t.Run("REPLACE with empty header clears tags", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		putWithTagging(t, h, "/bucket/src", "env=prod&owner=team-a")
		if rec := copyTo(t, h, "/bucket/dst", map[string]string{"x-amz-tagging-directive": "REPLACE"}); rec.Code != http.StatusOK {
			t.Fatalf("copy = %d; body=%s", rec.Code, rec.Body)
		}
		if _, tags := objectTags(t, h, "/bucket/dst"); len(tags) != 0 {
			t.Fatalf("REPLACE empty dst tags = %v, want none", tags)
		}
	})

	t.Run("invalid directive is 400", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		putWithTagging(t, h, "/bucket/src", "env=prod")
		if rec := copyTo(t, h, "/bucket/dst", map[string]string{"x-amz-tagging-directive": "MERGE"}); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid directive = %d, want 400; body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("REPLACE with malformed tagging is 400", func(t *testing.T) {
		h, _, _, _ := newTestHandler()
		putWithTagging(t, h, "/bucket/src", "env=prod")
		rec := copyTo(t, h, "/bucket/dst", map[string]string{
			"x-amz-tagging-directive": "REPLACE",
			"x-amz-tagging":           "dup=1&dup=2",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed REPLACE tagging = %d, want 400; body=%s", rec.Code, rec.Body)
		}
	})
}
