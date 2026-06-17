package s3compat

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// newMultipartTestHandler builds a handler backed by an in-memory
// manifest store, a single fake backend, and an in-memory multipart
// store — the standard harness for the multipart round-trip tests.
func newMultipartTestHandler() (*Handler, *multipart.MemoryStore) {
	mpStore := multipart.NewMemoryStore()
	h := New(Config{
		Manifests: memory.New(),
		Providers: map[string]providers.StorageProvider{"test": newFakeProvider("test")},
		Placement: fixedPlacement{backend: "test"},
		Multipart: mpStore,
		Now:       func() time.Time { return time.Unix(1700000000, 0) },
	})
	return h, mpStore
}

// completeMultipart runs Create→UploadPart×N→Complete for the given
// object path, applying setHeaders to the CreateMultipartUpload request,
// and returns the Complete recorder. Each part is a small distinct blob.
func completeMultipart(t *testing.T, h *Handler, path string, setHeaders func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	createReq := httptest.NewRequest(http.MethodPost, path+"?uploads", nil)
	if setHeaders != nil {
		setHeaders(createReq)
	}
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
		url := fmt.Sprintf("%s?uploadId=%s&partNumber=%d", path, uploadID, partNum)
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
		t.Fatalf("marshal complete body: %v", err)
	}
	completeReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("%s?uploadId=%s", path, uploadID), bytes.NewReader(completeXML))
	completeRec := httptest.NewRecorder()
	h.CompleteMultipartUpload(completeRec, completeReq)
	return completeRec
}

// TestCompleteMultipartUpload_TagsAndMetadata pins that x-amz-tagging and
// the S3 system / user-metadata headers supplied on CreateMultipartUpload
// land on the completed object: GET returns the system + x-amz-meta-*
// headers, and the ?tagging subresource returns the tag set.
func TestCompleteMultipartUpload_TagsAndMetadata(t *testing.T) {
	h, _ := newMultipartTestHandler()

	rec := completeMultipart(t, h, "/bucket/mp-meta", func(r *http.Request) {
		r.Header.Set("x-amz-tagging", "team=drive&env=prod")
		r.Header.Set("Content-Type", "image/png")
		r.Header.Set("Content-Disposition", `attachment; filename="x.png"`)
		r.Header.Set("Cache-Control", "max-age=3600")
		r.Header.Set("x-amz-meta-author", "ken")
		r.Header.Set("x-amz-meta-Project", "ZK")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("CompleteMultipartUpload status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/bucket/mp-meta", nil)
	getRec := httptest.NewRecorder()
	h.Get(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", getRec.Code, getRec.Body)
	}
	if ct := getRec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("GET Content-Type = %q, want image/png", ct)
	}
	if cd := getRec.Header().Get("Content-Disposition"); cd != `attachment; filename="x.png"` {
		t.Errorf("GET Content-Disposition = %q, want attachment", cd)
	}
	if cc := getRec.Header().Get("Cache-Control"); cc != "max-age=3600" {
		t.Errorf("GET Cache-Control = %q, want max-age=3600", cc)
	}
	if a := getRec.Header().Get("x-amz-meta-author"); a != "ken" {
		t.Errorf("GET x-amz-meta-author = %q, want ken", a)
	}
	if p := getRec.Header().Get("x-amz-meta-project"); p != "ZK" {
		t.Errorf("GET x-amz-meta-project = %q, want ZK", p)
	}

	status, tags := objectTags(t, h, "/bucket/mp-meta")
	if status != http.StatusOK {
		t.Fatalf("GET ?tagging status = %d, want 200", status)
	}
	if !tagsEqual(tags, map[string]string{"team": "drive", "env": "prod"}) {
		t.Errorf("multipart object tags = %v, want team=drive,env=prod", tags)
	}
}

// TestCreateMultipartUpload_RejectsBadTagging pins that a malformed
// x-amz-tagging header fails CreateMultipartUpload with 400 before any
// session row is created.
func TestCreateMultipartUpload_RejectsBadTagging(t *testing.T) {
	h, mpStore := newMultipartTestHandler()

	req := httptest.NewRequest(http.MethodPost, "/bucket/mp-bad-tag?uploads", nil)
	req.Header.Set("x-amz-tagging", "k="+strings.Repeat("v", maxTagValueLength+1))
	rec := httptest.NewRecorder()
	h.CreateMultipartUpload(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("CreateMultipartUpload status = %d, want 400; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "InvalidTag") && !strings.Contains(rec.Body.String(), "InvalidArgument") {
		t.Errorf("body should carry a tag-validation error code, got: %s", rec.Body)
	}
	if uploads := mpStore.List(AnonymousTenant, "bucket"); len(uploads) != 0 {
		t.Errorf("rejected create left %d session(s); want none", len(uploads))
	}
}

// TestCreateMultipartUpload_RejectsOversizedMetadata pins that an
// oversized x-amz-meta-* set fails CreateMultipartUpload with 400
// MetadataTooLarge before any session row is created.
func TestCreateMultipartUpload_RejectsOversizedMetadata(t *testing.T) {
	h, mpStore := newMultipartTestHandler()

	req := httptest.NewRequest(http.MethodPost, "/bucket/mp-big-meta?uploads", nil)
	req.Header.Set("x-amz-meta-blob", strings.Repeat("a", maxUserMetadataBytes+1))
	rec := httptest.NewRecorder()
	h.CreateMultipartUpload(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("CreateMultipartUpload status = %d, want 400; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "MetadataTooLarge") {
		t.Errorf("body should carry MetadataTooLarge, got: %s", rec.Body)
	}
	if uploads := mpStore.List(AnonymousTenant, "bucket"); len(uploads) != 0 {
		t.Errorf("rejected create left %d session(s); want none", len(uploads))
	}
}
