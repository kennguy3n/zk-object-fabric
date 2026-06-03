package s3compat

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// newVersioningTestHandler builds a handler with a BucketConfig store
// wired and an advancing clock (so successive PUTs/DELETEs of the same
// key get distinct version ids — newPieceID mixes the timestamp).
func newVersioningTestHandler() (*Handler, bucket_config.Store) {
	cfg := bucket_config.NewMemoryStore()
	now := time.Unix(1700000000, 0)
	h := New(Config{
		Manifests:    memory.New(),
		Providers:    map[string]providers.StorageProvider{"test": newFakeProvider("test")},
		Placement:    fixedPlacement{backend: "test"},
		Billing:      &recordingBilling{},
		BucketConfig: cfg,
		Now: func() time.Time {
			t := now
			now = now.Add(time.Second)
			return t
		},
	})
	return h, cfg
}

func setVersioning(t *testing.T, h *Handler, bucket, status string) {
	t.Helper()
	body := "<VersioningConfiguration><Status>" + status + "</Status></VersioningConfiguration>"
	req := httptest.NewRequest(http.MethodPut, "/"+bucket+"?versioning", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT ?versioning %s = %d, want 200; body=%s", status, rec.Code, rec.Body)
	}
}

func TestPutGetBucketVersioning_RoundTrip(t *testing.T) {
	h, _ := newVersioningTestHandler()

	// Unconfigured bucket: empty config, no <Status>.
	req := httptest.NewRequest(http.MethodGet, "/bucket?versioning", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?versioning unconfigured = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var doc versioningConfiguration
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Status != "" {
		t.Fatalf("unconfigured Status = %q, want empty", doc.Status)
	}

	// Enable, then read back.
	setVersioning(t, h, "bucket", "Enabled")
	req = httptest.NewRequest(http.MethodGet, "/bucket?versioning", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	doc = versioningConfiguration{}
	_ = xml.Unmarshal(rec.Body.Bytes(), &doc)
	if doc.Status != "Enabled" {
		t.Fatalf("Status after enable = %q, want Enabled", doc.Status)
	}

	// Suspend, then read back.
	setVersioning(t, h, "bucket", "Suspended")
	req = httptest.NewRequest(http.MethodGet, "/bucket?versioning", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	doc = versioningConfiguration{}
	_ = xml.Unmarshal(rec.Body.Bytes(), &doc)
	if doc.Status != "Suspended" {
		t.Fatalf("Status after suspend = %q, want Suspended", doc.Status)
	}
}

func TestPutBucketVersioning_Rejections(t *testing.T) {
	h, _ := newVersioningTestHandler()

	// Invalid status → 400.
	req := httptest.NewRequest(http.MethodPut, "/bucket?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Bogus</Status></VersioningConfiguration>"))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status = %d, want 400; body=%s", rec.Code, rec.Body)
	}

	// Empty status (the never-configured sentinel) is not settable → 400.
	req = httptest.NewRequest(http.MethodPut, "/bucket?versioning",
		strings.NewReader("<VersioningConfiguration></VersioningConfiguration>"))
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty status = %d, want 400; body=%s", rec.Code, rec.Body)
	}

	// Object-level path → 400 (versioning is bucket-level only).
	req = httptest.NewRequest(http.MethodPut, "/bucket/key?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>"))
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("object-level ?versioning = %d, want 400; body=%s", rec.Code, rec.Body)
	}
}

// TestPutBucketVersioning_CannotSuspendWithObjectLock verifies the
// AWS invariant that once Object Lock is enabled on a bucket,
// versioning can no longer be suspended (Object Lock relies on
// immutable versions). The transition must be refused with 409.
func TestPutBucketVersioning_CannotSuspendWithObjectLock(t *testing.T) {
	h, _ := newVersioningTestHandler()
	setVersioning(t, h, "bucket", "Enabled")
	enableObjectLock(t, h, "bucket", objectLockEnabledNoRule)

	req := httptest.NewRequest(http.MethodPut, "/bucket?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>"))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("suspend with Object Lock = %d, want 409; body=%s", rec.Code, rec.Body)
	}

	// The bucket must still report Enabled (the suspend was a no-op).
	req = httptest.NewRequest(http.MethodGet, "/bucket?versioning", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	var doc versioningConfiguration
	_ = xml.Unmarshal(rec.Body.Bytes(), &doc)
	if doc.Status != "Enabled" {
		t.Fatalf("Status after rejected suspend = %q, want Enabled", doc.Status)
	}

	// Re-affirming Enabled is still allowed (idempotent).
	setVersioning(t, h, "bucket", "Enabled")
}

func TestBucketVersioning_NotImplementedWithoutStore(t *testing.T) {
	h, _, _, _ := newTestHandler() // no BucketConfig wired
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		req := httptest.NewRequest(method, "/bucket?versioning",
			strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>"))
		rec := httptest.NewRecorder()
		h.dispatch(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s ?versioning without store = %d, want 501; body=%s", method, rec.Code, rec.Body)
		}
	}
}

// TestGetObjectWithVersioningQuery_NotRoutedToBucketConfig guards the
// dispatch fix where GET /{bucket}/{key}?versioning must fall through
// to the object GET (a key is present) rather than being routed to
// GetBucketVersioning, which would return the bucket's versioning XML
// in place of the object body.
func TestGetObjectWithVersioningQuery_NotRoutedToBucketConfig(t *testing.T) {
	h, _ := newVersioningTestHandler()
	setVersioning(t, h, "bucket", "Enabled")
	const body = "object-bytes"
	putObj(t, h, "/bucket/obj", body)

	req := httptest.NewRequest(http.MethodGet, "/bucket/obj?versioning", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /bucket/obj?versioning = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if rec.Body.String() != body {
		t.Fatalf("GET /bucket/obj?versioning body = %q, want object data %q (was the request misrouted to GetBucketVersioning?)", rec.Body.String(), body)
	}
	if strings.Contains(rec.Body.String(), "VersioningConfiguration") {
		t.Fatalf("GET /bucket/obj?versioning returned the bucket versioning document instead of object data; body=%s", rec.Body)
	}
}

// putObj writes an object through the Put handler and returns its
// version id.
func putObj(t *testing.T, h *Handler, path, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader([]byte(body)))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT %s = %d, want 200; body=%s", path, rec.Code, rec.Body)
	}
	return rec.Header().Get("x-amz-version-id")
}

// TestVersionedDelete_CreatesDeleteMarker exercises the full WS8.4
// delete-marker lifecycle on a versioning-enabled bucket.
func TestVersionedDelete_CreatesDeleteMarker(t *testing.T) {
	h, _ := newVersioningTestHandler()
	setVersioning(t, h, "bucket", "Enabled")

	v1 := putObj(t, h, "/bucket/obj", "hello")
	if v1 == "" {
		t.Fatal("expected a version id from PUT")
	}

	// DELETE without versionId inserts a delete marker.
	req := httptest.NewRequest(http.MethodDelete, "/bucket/obj", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("versioned DELETE = %d, want 204; body=%s", rec.Code, rec.Body)
	}
	marker := rec.Header().Get("x-amz-version-id")
	if rec.Header().Get("x-amz-delete-marker") != "true" || marker == "" {
		t.Fatalf("delete response headers = %v, want x-amz-delete-marker:true + version id", rec.Header())
	}

	// Unversioned GET now 404s with the delete-marker header.
	req = httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after delete = %d, want 404; body=%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("x-amz-delete-marker") != "true" {
		t.Fatalf("GET after delete missing x-amz-delete-marker header; headers=%v", rec.Header())
	}

	// GET of the delete marker by versionId → 405.
	req = httptest.NewRequest(http.MethodGet, "/bucket/obj?versionId="+marker, nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET delete marker by versionId = %d, want 405; body=%s", rec.Code, rec.Body)
	}

	// The old version is still readable by versionId.
	req = httptest.NewRequest(http.MethodGet, "/bucket/obj?versionId="+v1, nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("GET v1 by versionId = %d body=%q, want 200 'hello'", rec.Code, rec.Body)
	}

	// ListObjectsV2 hides the logically-deleted key.
	req = httptest.NewRequest(http.MethodGet, "/bucket/?list-type=2", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if strings.Contains(rec.Body.String(), "<Key>obj</Key>") {
		t.Fatalf("ListObjectsV2 still lists deleted key; body=%s", rec.Body)
	}

	// ListObjectVersions surfaces both the version and the marker.
	req = httptest.NewRequest(http.MethodGet, "/bucket/?versions", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "<Version>") || !strings.Contains(body, "<DeleteMarker>") {
		t.Fatalf("ListObjectVersions missing Version or DeleteMarker; body=%s", body)
	}
}

// TestUnversionedDelete_PermanentlyRemoves confirms that with no
// versioning configured, DELETE still hard-removes the object (no
// delete marker), preserving the pre-WS8.4 behaviour.
func TestUnversionedDelete_PermanentlyRemoves(t *testing.T) {
	h, _ := newVersioningTestHandler() // store present but bucket left unconfigured

	putObj(t, h, "/bucket/obj", "hello")
	req := httptest.NewRequest(http.MethodDelete, "/bucket/obj", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", rec.Code)
	}
	if rec.Header().Get("x-amz-delete-marker") == "true" {
		t.Fatalf("unconfigured-bucket DELETE created a delete marker; headers=%v", rec.Header())
	}
	req = httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after permanent delete = %d, want 404", rec.Code)
	}
}
