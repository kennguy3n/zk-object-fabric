package s3compat

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// recordingEmitter captures emitted ObjectEvents for assertions.
type recordingEmitter struct {
	mu     sync.Mutex
	events []ObjectEvent
}

func (e *recordingEmitter) Emit(evt ObjectEvent) {
	e.mu.Lock()
	e.events = append(e.events, evt)
	e.mu.Unlock()
}

func (e *recordingEmitter) names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.events))
	for i, evt := range e.events {
		out[i] = evt.EventName
	}
	return out
}

// newNotificationTestHandler builds a handler with a BucketConfig store
// and a recording notification emitter, plus an advancing clock.
func newNotificationTestHandler() (*Handler, *recordingEmitter) {
	emit := &recordingEmitter{}
	now := time.Unix(1700000000, 0)
	h := New(Config{
		Manifests:     memory.New(),
		Providers:     map[string]providers.StorageProvider{"test": newFakeProvider("test")},
		Placement:     fixedPlacement{backend: "test"},
		Billing:       &recordingBilling{},
		BucketConfig:  bucket_config.NewMemoryStore(),
		Notifications: emit,
		Now: func() time.Time {
			t := now
			now = now.Add(time.Second)
			return t
		},
	})
	return h, emit
}

const sampleNotification = `<NotificationConfiguration>
  <WebhookConfiguration>
    <Id>on-upload</Id>
    <Endpoint>https://hooks.example.com/s3</Endpoint>
    <Event>s3:ObjectCreated:*</Event>
    <Event>s3:ObjectRemoved:Delete</Event>
    <Filter><S3Key>
      <FilterRule><Name>prefix</Name><Value>logs/</Value></FilterRule>
      <FilterRule><Name>suffix</Name><Value>.json</Value></FilterRule>
    </S3Key></Filter>
  </WebhookConfiguration>
</NotificationConfiguration>`

func putNotification(t *testing.T, h *Handler, bucket, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/"+bucket+"?notification", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT ?notification = %d, want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestPutGetBucketNotification_RoundTrip(t *testing.T) {
	h, _ := newNotificationTestHandler()

	// Unconfigured → empty 200 document (no NoSuch* error).
	req := httptest.NewRequest(http.MethodGet, "/bucket?notification", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?notification unconfigured = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var empty notificationConfiguration
	if err := xml.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if len(empty.Webhooks) != 0 {
		t.Fatalf("unconfigured webhooks = %d, want 0", len(empty.Webhooks))
	}

	putNotification(t, h, "bucket", sampleNotification)

	req = httptest.NewRequest(http.MethodGet, "/bucket?notification", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?notification = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var doc notificationConfiguration
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Webhooks) != 1 {
		t.Fatalf("webhooks = %d, want 1", len(doc.Webhooks))
	}
	w := doc.Webhooks[0]
	if w.ID != "on-upload" || w.Endpoint != "https://hooks.example.com/s3" {
		t.Fatalf("id/endpoint mismatch: %+v", w)
	}
	if strings.Join(w.Events, ",") != "s3:ObjectCreated:*,s3:ObjectRemoved:Delete" {
		t.Fatalf("events mismatch: %v", w.Events)
	}
	if w.Filter == nil || len(w.Filter.S3Key.Rules) != 2 {
		t.Fatalf("filter mismatch: %+v", w.Filter)
	}
	if w.Filter.S3Key.Rules[0].Name != "prefix" || w.Filter.S3Key.Rules[0].Value != "logs/" ||
		w.Filter.S3Key.Rules[1].Name != "suffix" || w.Filter.S3Key.Rules[1].Value != ".json" {
		t.Fatalf("filter rules mismatch: %+v", w.Filter.S3Key.Rules)
	}
}

func TestPutBucketNotification_EmptyBodyClears(t *testing.T) {
	h, _ := newNotificationTestHandler()
	putNotification(t, h, "bucket", sampleNotification)

	// Empty body clears the configuration.
	putNotification(t, h, "bucket", "")

	req := httptest.NewRequest(http.MethodGet, "/bucket?notification", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	var doc notificationConfiguration
	_ = xml.Unmarshal(rec.Body.Bytes(), &doc)
	if len(doc.Webhooks) != 0 {
		t.Fatalf("after clear webhooks = %d, want 0", len(doc.Webhooks))
	}
}

func TestPutBucketNotification_MalformedFilter(t *testing.T) {
	h, _ := newNotificationTestHandler()
	body := `<NotificationConfiguration><WebhookConfiguration>
	  <Endpoint>https://hooks.example.com/s3</Endpoint>
	  <Event>s3:ObjectCreated:*</Event>
	  <Filter><S3Key><FilterRule><Name>bogus</Name><Value>x</Value></FilterRule></S3Key></Filter>
	</WebhookConfiguration></NotificationConfiguration>`
	req := httptest.NewRequest(http.MethodPut, "/bucket?notification", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed filter = %d, want 400; body=%s", rec.Code, rec.Body)
	}
	if code := errCode(t, rec.Body.Bytes()); code != "MalformedXML" {
		t.Fatalf("error code = %q, want MalformedXML", code)
	}
}

func TestPutBucketNotification_InvalidEndpoint(t *testing.T) {
	h, _ := newNotificationTestHandler()
	body := `<NotificationConfiguration><WebhookConfiguration>
	  <Endpoint>ftp://nope.example.com</Endpoint>
	  <Event>s3:ObjectCreated:*</Event>
	</WebhookConfiguration></NotificationConfiguration>`
	req := httptest.NewRequest(http.MethodPut, "/bucket?notification", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid endpoint = %d, want 400; body=%s", rec.Code, rec.Body)
	}
}

func TestPutBucketNotification_RejectsObjectPath(t *testing.T) {
	h, _ := newNotificationTestHandler()
	req := httptest.NewRequest(http.MethodPut, "/bucket/key?notification", strings.NewReader(sampleNotification))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("object-path PUT ?notification = %d, want 400", rec.Code)
	}
}

func TestNotification_NilStoreReturns501(t *testing.T) {
	h, _, _, _ := newTestHandler() // no BucketConfig store
	req := httptest.NewRequest(http.MethodGet, "/bucket?notification", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("GET ?notification (nil store) = %d, want 501; body=%s", rec.Code, rec.Body)
	}
}

// TestObjectEventsEmittedOnMutations verifies the handler emits the
// right leaf events on PUT and DELETE through the notify hook. The
// emitter is synchronous in the test, so no waiting is required.
func TestObjectEventsEmittedOnMutations(t *testing.T) {
	h, emit := newNotificationTestHandler()

	// PUT object → ObjectCreated:Put.
	body := []byte("hello")
	req := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d; body=%s", rec.Code, rec.Body)
	}

	// DELETE object → ObjectRemoved:Delete (non-versioned bucket).
	req = httptest.NewRequest(http.MethodDelete, "/bucket/obj", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d; body=%s", rec.Code, rec.Body)
	}

	got := emit.names()
	if len(got) != 2 || got[0] != "s3:ObjectCreated:Put" || got[1] != "s3:ObjectRemoved:Delete" {
		t.Fatalf("emitted events = %v, want [Put, Delete]", got)
	}
	// The PUT event carries object metadata.
	emit.mu.Lock()
	put := emit.events[0]
	emit.mu.Unlock()
	if put.Bucket != "bucket" || put.ObjectKey != "obj" || put.SizeBytes != int64(len(body)) {
		t.Fatalf("put event = %+v", put)
	}
}
