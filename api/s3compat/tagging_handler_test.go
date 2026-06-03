package s3compat

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// putTestObject writes a one-piece object through the Put handler so
// tagging tests have a manifest to operate on. It returns the version
// id the gateway assigned.
func putTestObject(t *testing.T, h *Handler, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader([]byte("body")))
	req.ContentLength = 4
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed PUT %s = %d, want 200; body=%s", path, rec.Code, rec.Body)
	}
	return rec.Header().Get("x-amz-version-id")
}

func putTaggingXML(tags ...[2]string) string {
	var b strings.Builder
	b.WriteString("<Tagging><TagSet>")
	for _, kv := range tags {
		b.WriteString("<Tag><Key>" + kv[0] + "</Key><Value>" + kv[1] + "</Value></Tag>")
	}
	b.WriteString("</TagSet></Tagging>")
	return b.String()
}

func TestObjectTagging_RoundTripViaDispatch(t *testing.T) {
	h, _, _, _ := newTestHandler()
	putTestObject(t, h, "/bucket/obj")

	// PUT ?tagging
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj?tagging", strings.NewReader(putTaggingXML([2]string{"env", "prod"}, [2]string{"team", "storage"})))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT ?tagging = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	// GET ?tagging
	req = httptest.NewRequest(http.MethodGet, "/bucket/obj?tagging", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?tagging = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var doc taggingDocument
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal tagging response: %v; body=%s", err, rec.Body)
	}
	if len(doc.TagSet) != 2 {
		t.Fatalf("TagSet len = %d, want 2", len(doc.TagSet))
	}
	want := map[string]string{"env": "prod", "team": "storage"}
	for _, tg := range doc.TagSet {
		if want[tg.Key] != tg.Value {
			t.Fatalf("tag %q = %q, want %q", tg.Key, tg.Value, want[tg.Key])
		}
	}

	// DELETE ?tagging
	req = httptest.NewRequest(http.MethodDelete, "/bucket/obj?tagging", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE ?tagging = %d, want 204; body=%s", rec.Code, rec.Body)
	}

	// GET ?tagging again → empty set
	req = httptest.NewRequest(http.MethodGet, "/bucket/obj?tagging", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	doc = taggingDocument{}
	_ = xml.Unmarshal(rec.Body.Bytes(), &doc)
	if len(doc.TagSet) != 0 {
		t.Fatalf("after delete TagSet len = %d, want 0", len(doc.TagSet))
	}
}

func TestObjectTagging_BucketLevelNotImplemented(t *testing.T) {
	h, _, _, _ := newTestHandler()
	for _, method := range []string{http.MethodPut, http.MethodGet, http.MethodDelete} {
		var body *strings.Reader
		if method == http.MethodPut {
			body = strings.NewReader(putTaggingXML([2]string{"a", "b"}))
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(method, "/bucket?tagging", body)
		rec := httptest.NewRecorder()
		h.dispatch(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s /bucket?tagging = %d, want 501; body=%s", method, rec.Code, rec.Body)
		}
	}
}

func TestObjectTagging_MissingKeyIs404(t *testing.T) {
	h, _, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/bucket/missing?tagging", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET ?tagging missing = %d, want 404; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "NoSuchKey") {
		t.Fatalf("missing-key body = %s, want NoSuchKey", rec.Body)
	}
}

func TestObjectTagging_ValidationRejected(t *testing.T) {
	h, _, _, _ := newTestHandler()
	putTestObject(t, h, "/bucket/obj")

	cases := []struct {
		name string
		body string
	}{
		{"too many tags", putTaggingXML(
			[2]string{"a", "1"}, [2]string{"b", "1"}, [2]string{"c", "1"}, [2]string{"d", "1"},
			[2]string{"e", "1"}, [2]string{"f", "1"}, [2]string{"g", "1"}, [2]string{"h", "1"},
			[2]string{"i", "1"}, [2]string{"j", "1"}, [2]string{"k", "1"})},
		{"empty key", putTaggingXML([2]string{"", "v"})},
		{"key too long", putTaggingXML([2]string{strings.Repeat("k", maxTagKeyLength+1), "v"})},
		{"value too long", putTaggingXML([2]string{"k", strings.Repeat("v", maxTagValueLength+1)})},
		{"duplicate keys", putTaggingXML([2]string{"dup", "1"}, [2]string{"dup", "2"})},
		{"malformed xml", "<Tagging><TagSet><Tag>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/bucket/obj?tagging", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.dispatch(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT ?tagging (%s) = %d, want 400; body=%s", tc.name, rec.Code, rec.Body)
			}
		})
	}
}

func TestObjectTagging_LimitsAtBoundary(t *testing.T) {
	h, _, _, _ := newTestHandler()
	putTestObject(t, h, "/bucket/obj")

	// Exactly 10 tags, max-length key, max-length value: all accepted.
	tags := make([][2]string, 0, maxObjectTags)
	for i := 0; i < maxObjectTags; i++ {
		tags = append(tags, [2]string{"k" + string(rune('a'+i)), "v"})
	}
	tags[0] = [2]string{strings.Repeat("k", maxTagKeyLength), strings.Repeat("v", maxTagValueLength)}
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj?tagging", strings.NewReader(putTaggingXML(tags...)))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT ?tagging (10 tags at boundary) = %d, want 200; body=%s", rec.Code, rec.Body)
	}
}
