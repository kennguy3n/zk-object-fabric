package s3compat

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// enableObjectLock turns on bucket Object Lock via the dispatch path
// (requires versioning to already be Enabled).
func enableObjectLock(t *testing.T, h *Handler, bucket, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/"+bucket+"?object-lock", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT ?object-lock = %d, want 200; body=%s", rec.Code, rec.Body)
	}
}

const objectLockEnabledNoRule = "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>"

func TestPutObjectLockConfiguration_RequiresVersioning(t *testing.T) {
	h, _ := newVersioningTestHandler()
	// Versioning is Unset on the bucket → enabling Object Lock is a
	// 409 conflict (Object Lock depends on versioning).
	req := httptest.NewRequest(http.MethodPut, "/bucket?object-lock", strings.NewReader(objectLockEnabledNoRule))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("object-lock without versioning = %d, want 409; body=%s", rec.Code, rec.Body)
	}
}

func TestObjectLockConfiguration_RoundTrip(t *testing.T) {
	h, _ := newVersioningTestHandler()
	setVersioning(t, h, "bucket", "Enabled")

	// 404 before configured (matches AWS ObjectLockConfigurationNotFoundError).
	req := httptest.NewRequest(http.MethodGet, "/bucket?object-lock", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET ?object-lock unconfigured = %d, want 404; body=%s", rec.Code, rec.Body)
	}

	enableObjectLock(t, h, "bucket",
		"<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>30</Days></DefaultRetention></Rule></ObjectLockConfiguration>")

	req = httptest.NewRequest(http.MethodGet, "/bucket?object-lock", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?object-lock = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var doc objectLockConfiguration
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body)
	}
	if doc.ObjectLockEnabled != "Enabled" || doc.Rule == nil || doc.Rule.DefaultRetention == nil {
		t.Fatalf("config = %+v, want enabled with default rule", doc)
	}
	if doc.Rule.DefaultRetention.Mode != "GOVERNANCE" || doc.Rule.DefaultRetention.Days != 30 {
		t.Fatalf("default rule = %+v", doc.Rule.DefaultRetention)
	}
}

func TestPutObjectRetention_RoundTripAndWeakenGuard(t *testing.T) {
	h, _ := newVersioningTestHandler()
	setVersioning(t, h, "bucket", "Enabled")
	enableObjectLock(t, h, "bucket", objectLockEnabledNoRule)
	putTestObject(t, h, "/bucket/obj")

	putRet := func(mode, until string, bypass bool) int {
		body := "<Retention><Mode>" + mode + "</Mode><RetainUntilDate>" + until + "</RetainUntilDate></Retention>"
		req := httptest.NewRequest(http.MethodPut, "/bucket/obj?retention", strings.NewReader(body))
		if bypass {
			req.Header.Set("x-amz-bypass-governance-retention", "true")
		}
		rec := httptest.NewRecorder()
		h.dispatch(rec, req)
		return rec.Code
	}

	if code := putRet("GOVERNANCE", "2099-01-01T00:00:00Z", false); code != http.StatusOK {
		t.Fatalf("set GOVERNANCE retention = %d, want 200", code)
	}

	// Read it back.
	req := httptest.NewRequest(http.MethodGet, "/bucket/obj?retention", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?retention = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var rd retentionDocument
	if err := xml.Unmarshal(rec.Body.Bytes(), &rd); err != nil {
		t.Fatalf("unmarshal retention: %v", err)
	}
	if rd.Mode != "GOVERNANCE" || rd.RetainUntilDate == "" {
		t.Fatalf("retention = %+v, want GOVERNANCE with date", rd)
	}

	// Shortening an in-force GOVERNANCE retention without the bypass
	// header is refused.
	if code := putRet("GOVERNANCE", "2098-01-01T00:00:00Z", false); code != http.StatusForbidden {
		t.Fatalf("shorten without bypass = %d, want 403", code)
	}
	// Extending is always allowed.
	if code := putRet("GOVERNANCE", "2100-01-01T00:00:00Z", false); code != http.StatusOK {
		t.Fatalf("extend = %d, want 200", code)
	}
	// Shortening WITH the bypass header is allowed for GOVERNANCE.
	if code := putRet("GOVERNANCE", "2098-01-01T00:00:00Z", true); code != http.StatusOK {
		t.Fatalf("shorten with bypass = %d, want 200", code)
	}
}

func TestPutObjectRetention_ComplianceCannotBeWeakened(t *testing.T) {
	h, _ := newVersioningTestHandler()
	setVersioning(t, h, "bucket", "Enabled")
	enableObjectLock(t, h, "bucket", objectLockEnabledNoRule)
	putTestObject(t, h, "/bucket/obj")

	putRet := func(mode, until string, bypass bool) int {
		body := "<Retention><Mode>" + mode + "</Mode><RetainUntilDate>" + until + "</RetainUntilDate></Retention>"
		req := httptest.NewRequest(http.MethodPut, "/bucket/obj?retention", strings.NewReader(body))
		if bypass {
			req.Header.Set("x-amz-bypass-governance-retention", "true")
		}
		rec := httptest.NewRecorder()
		h.dispatch(rec, req)
		return rec.Code
	}
	if code := putRet("COMPLIANCE", "2099-01-01T00:00:00Z", false); code != http.StatusOK {
		t.Fatalf("set COMPLIANCE = %d, want 200", code)
	}
	// Even with the bypass header, COMPLIANCE retention is absolute.
	if code := putRet("COMPLIANCE", "2098-01-01T00:00:00Z", true); code != http.StatusForbidden {
		t.Fatalf("shorten COMPLIANCE with bypass = %d, want 403", code)
	}
	// Downgrading COMPLIANCE to the bypassable GOVERNANCE mode is
	// refused even with an unchanged retain-until date and the bypass
	// header — it would silently weaken absolute protection.
	if code := putRet("GOVERNANCE", "2099-01-01T00:00:00Z", true); code != http.StatusForbidden {
		t.Fatalf("downgrade COMPLIANCE->GOVERNANCE = %d, want 403", code)
	}
}

func TestObjectLegalHold_RoundTrip(t *testing.T) {
	h, _ := newVersioningTestHandler()
	setVersioning(t, h, "bucket", "Enabled")
	enableObjectLock(t, h, "bucket", objectLockEnabledNoRule)
	putTestObject(t, h, "/bucket/obj")

	setHold := func(status string) int {
		body := "<LegalHold><Status>" + status + "</Status></LegalHold>"
		req := httptest.NewRequest(http.MethodPut, "/bucket/obj?legal-hold", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.dispatch(rec, req)
		return rec.Code
	}
	// Default (unset) reads OFF.
	req := httptest.NewRequest(http.MethodGet, "/bucket/obj?legal-hold", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	var ld legalHoldDocument
	_ = xml.Unmarshal(rec.Body.Bytes(), &ld)
	if ld.Status != "OFF" {
		t.Fatalf("default legal-hold = %q, want OFF", ld.Status)
	}

	if code := setHold("ON"); code != http.StatusOK {
		t.Fatalf("legal-hold ON = %d", code)
	}
	req = httptest.NewRequest(http.MethodGet, "/bucket/obj?legal-hold", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	ld = legalHoldDocument{}
	_ = xml.Unmarshal(rec.Body.Bytes(), &ld)
	if ld.Status != "ON" {
		t.Fatalf("legal-hold after ON = %q, want ON", ld.Status)
	}
	if code := setHold("OFF"); code != http.StatusOK {
		t.Fatalf("legal-hold OFF = %d", code)
	}
}

func TestObjectLock_RetentionBlocksPermanentDelete(t *testing.T) {
	h, _ := newVersioningTestHandler()
	setVersioning(t, h, "bucket", "Enabled")
	enableObjectLock(t, h, "bucket", objectLockEnabledNoRule)
	versionID := putTestObject(t, h, "/bucket/obj")

	// GOVERNANCE retention far in the future on the latest version.
	body := "<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>2099-01-01T00:00:00Z</RetainUntilDate></Retention>"
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj?retention", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set retention = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	del := func(bypass bool) int {
		req := httptest.NewRequest(http.MethodDelete, "/bucket/obj?versionId="+versionID, nil)
		if bypass {
			req.Header.Set("x-amz-bypass-governance-retention", "true")
		}
		rec := httptest.NewRecorder()
		h.dispatch(rec, req)
		return rec.Code
	}
	// Permanent delete of the locked version is refused without bypass.
	if code := del(false); code != http.StatusForbidden {
		t.Fatalf("delete locked version = %d, want 403", code)
	}
	// GOVERNANCE retention yields to the bypass header.
	if code := del(true); code != http.StatusNoContent {
		t.Fatalf("delete with bypass = %d, want 204", code)
	}
}

func TestObjectLock_ComplianceAndLegalHoldBlockDeleteUnconditionally(t *testing.T) {
	// COMPLIANCE retention is not bypassable.
	h, _ := newVersioningTestHandler()
	setVersioning(t, h, "bucket", "Enabled")
	enableObjectLock(t, h, "bucket", objectLockEnabledNoRule)
	v := putTestObject(t, h, "/bucket/obj")
	body := "<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>2099-01-01T00:00:00Z</RetainUntilDate></Retention>"
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj?retention", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set COMPLIANCE = %d; body=%s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest(http.MethodDelete, "/bucket/obj?versionId="+v, nil)
	req.Header.Set("x-amz-bypass-governance-retention", "true")
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delete COMPLIANCE-locked w/ bypass = %d, want 403; body=%s", rec.Code, rec.Body)
	}

	// Legal hold blocks even with the bypass header and no retention.
	h2, _ := newVersioningTestHandler()
	setVersioning(t, h2, "bucket", "Enabled")
	enableObjectLock(t, h2, "bucket", objectLockEnabledNoRule)
	v2 := putTestObject(t, h2, "/bucket/obj")
	req = httptest.NewRequest(http.MethodPut, "/bucket/obj?legal-hold", strings.NewReader("<LegalHold><Status>ON</Status></LegalHold>"))
	rec = httptest.NewRecorder()
	h2.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set legal-hold = %d; body=%s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest(http.MethodDelete, "/bucket/obj?versionId="+v2, nil)
	req.Header.Set("x-amz-bypass-governance-retention", "true")
	rec = httptest.NewRecorder()
	h2.dispatch(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delete legal-held w/ bypass = %d, want 403; body=%s", rec.Code, rec.Body)
	}
}

func TestObjectLock_DefaultRetentionInheritedAtPut(t *testing.T) {
	h, _ := newVersioningTestHandler()
	setVersioning(t, h, "bucket", "Enabled")
	enableObjectLock(t, h, "bucket",
		"<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>30</Days></DefaultRetention></Rule></ObjectLockConfiguration>")

	// A freshly-PUT object inherits the bucket default retention.
	versionID := putTestObject(t, h, "/bucket/obj")
	req := httptest.NewRequest(http.MethodGet, "/bucket/obj?retention", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET inherited retention = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var rd retentionDocument
	if err := xml.Unmarshal(rec.Body.Bytes(), &rd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rd.Mode != "GOVERNANCE" || rd.RetainUntilDate == "" {
		t.Fatalf("inherited retention = %+v, want GOVERNANCE with date", rd)
	}

	// And it is enforced: the inherited lock blocks a permanent delete.
	req = httptest.NewRequest(http.MethodDelete, "/bucket/obj?versionId="+versionID, nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delete inherited-locked version = %d, want 403; body=%s", rec.Code, rec.Body)
	}
}

func TestObjectLock_OverwriteBlockedWhenVersioningNotEnabled(t *testing.T) {
	h, cfg := newVersioningTestHandler()
	setVersioning(t, h, "bucket", "Enabled")
	enableObjectLock(t, h, "bucket", objectLockEnabledNoRule)
	putTestObject(t, h, "/bucket/obj")

	// Lock the current version under COMPLIANCE.
	body := "<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>2099-01-01T00:00:00Z</RetainUntilDate></Retention>"
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj?retention", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set retention = %d; body=%s", rec.Code, rec.Body)
	}

	// Force versioning to Suspended directly in the store so a PUT
	// would overwrite the locked version in place instead of creating
	// a new one. (The handler now refuses this transition while Object
	// Lock is enabled, so we bypass it to recreate the legacy/
	// inconsistent state the pre-flight guard defends against.)
	if err := cfg.SetVersioning(context.Background(), AnonymousTenant, "bucket", bucket_config.VersioningSuspended); err != nil {
		t.Fatalf("force-suspend versioning: %v", err)
	}
	ow := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader([]byte("new")))
	ow.ContentLength = 3
	owRec := httptest.NewRecorder()
	h.Put(owRec, ow)
	if owRec.Code != http.StatusForbidden {
		t.Fatalf("overwrite of locked version = %d, want 403; body=%s", owRec.Code, owRec.Body)
	}
}

// newObjectLockMultipartHandler builds a handler with BucketConfig and
// a multipart store wired plus an advancing clock (so successive
// writes of a key get distinct version ids).
func newObjectLockMultipartHandler() (*Handler, bucket_config.Store) {
	cfg := bucket_config.NewMemoryStore()
	now := time.Unix(1700000000, 0)
	h := New(Config{
		Manifests:    memory.New(),
		Providers:    map[string]providers.StorageProvider{"test": newFakeProvider("test")},
		Placement:    fixedPlacement{backend: "test"},
		Billing:      &recordingBilling{},
		BucketConfig: cfg,
		Multipart:    multipart.NewMemoryStore(),
		Now: func() time.Time {
			t := now
			now = now.Add(time.Second)
			return t
		},
	})
	return h, cfg
}

// TestObjectLock_MultipartCompleteBlockedWhenVersioningNotEnabled
// proves CompleteMultipartUpload mirrors the Put/Copy overwrite guard:
// completing a multipart upload that would overwrite a locked current
// version on an Object-Lock bucket that is not versioning-Enabled is
// refused with 403, and the locked version is left intact.
func TestObjectLock_MultipartCompleteBlockedWhenVersioningNotEnabled(t *testing.T) {
	h, cfg := newObjectLockMultipartHandler()
	setVersioning(t, h, "bucket", "Enabled")
	enableObjectLock(t, h, "bucket", objectLockEnabledNoRule)
	putTestObject(t, h, "/bucket/mp-obj")

	// Lock the current version under COMPLIANCE.
	ret := "<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>2099-01-01T00:00:00Z</RetainUntilDate></Retention>"
	req := httptest.NewRequest(http.MethodPut, "/bucket/mp-obj?retention", strings.NewReader(ret))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set retention = %d; body=%s", rec.Code, rec.Body)
	}

	// Force versioning Suspended directly (the handler refuses this
	// transition while Object Lock is enabled).
	if err := cfg.SetVersioning(context.Background(), AnonymousTenant, "bucket", bucket_config.VersioningSuspended); err != nil {
		t.Fatalf("force-suspend versioning: %v", err)
	}

	// Start a multipart upload and stage one part.
	req = httptest.NewRequest(http.MethodPost, "/bucket/mp-obj?uploads", nil)
	rec = httptest.NewRecorder()
	h.CreateMultipartUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateMultipartUpload = %d; body=%s", rec.Code, rec.Body)
	}
	var initRes initiateMultipartUploadResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &initRes); err != nil {
		t.Fatalf("decode initiate: %v", err)
	}
	part := bytes.Repeat([]byte("x"), 16)
	url := fmt.Sprintf("/bucket/mp-obj?uploadId=%s&partNumber=1", initRes.UploadID)
	preq := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(part))
	preq.ContentLength = int64(len(part))
	prec := httptest.NewRecorder()
	h.UploadPart(prec, preq)
	if prec.Code != http.StatusOK {
		t.Fatalf("UploadPart = %d; body=%s", prec.Code, prec.Body)
	}
	etag := strings.Trim(prec.Header().Get("ETag"), `"`)

	// Completing would overwrite the locked current version → 403.
	completeXML, _ := xml.Marshal(completeMultipartUploadRequest{Parts: []completeUploadEntry{{PartNumber: 1, ETag: etag}}})
	creq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/bucket/mp-obj?uploadId=%s", initRes.UploadID), bytes.NewReader(completeXML))
	crec := httptest.NewRecorder()
	h.CompleteMultipartUpload(crec, creq)
	if crec.Code != http.StatusForbidden {
		t.Fatalf("CompleteMultipartUpload overwriting locked version = %d, want 403; body=%s", crec.Code, crec.Body)
	}

	// The locked version must still be readable unchanged.
	greq := httptest.NewRequest(http.MethodGet, "/bucket/mp-obj", nil)
	grec := httptest.NewRecorder()
	h.dispatch(grec, greq)
	if grec.Code != http.StatusOK || grec.Body.String() != "body" {
		t.Fatalf("GET after blocked complete = %d body=%q, want 200 'body'", grec.Code, grec.Body)
	}
}

// getFailingManifestStore wraps a real ManifestStore but forces Get to
// return a chosen (non-ErrNotFound) error, simulating a transient
// backend read failure.
type getFailingManifestStore struct {
	manifest_store.ManifestStore
	getErr error
}

func (s getFailingManifestStore) Get(context.Context, manifest_store.ManifestKey) (*metadata.ObjectManifest, error) {
	return nil, s.getErr
}

// TestObjectLock_OverwriteFailsClosedOnReadError verifies the WORM
// guarantee that a transient manifest read error during the overwrite
// pre-flight does NOT fail open: rather than silently proceeding (and
// risking destruction of a locked version), the write is refused with
// 500. ErrNotFound is the only "allow" case, exercised elsewhere.
func TestObjectLock_OverwriteFailsClosedOnReadError(t *testing.T) {
	cfg := bucket_config.NewMemoryStore()
	now := time.Unix(1700000000, 0)
	h := New(Config{
		Manifests:    getFailingManifestStore{ManifestStore: memory.New(), getErr: errors.New("manifest backend unavailable")},
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
	setVersioning(t, h, "bucket", "Enabled")
	enableObjectLock(t, h, "bucket", objectLockEnabledNoRule)
	// Force the inconsistent Suspended state so the overwrite guard
	// reaches the manifest Get (which fails).
	if err := cfg.SetVersioning(context.Background(), AnonymousTenant, "bucket", bucket_config.VersioningSuspended); err != nil {
		t.Fatalf("force-suspend versioning: %v", err)
	}

	ow := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader([]byte("new")))
	ow.ContentLength = 3
	rec := httptest.NewRecorder()
	h.Put(rec, ow)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("overwrite with failing manifest read = %d, want 500 (fail-closed); body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "ObjectLockGetFailed") {
		t.Fatalf("expected ObjectLockGetFailed error code, got body=%s", rec.Body)
	}
}
