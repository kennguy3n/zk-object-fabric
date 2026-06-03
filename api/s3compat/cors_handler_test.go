package s3compat

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// putCORS issues PUT /{bucket}?cors with the given body and asserts 200.
func putCORS(t *testing.T, h *Handler, bucket, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/"+bucket+"?cors", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT ?cors = %d, want 200; body=%s", rec.Code, rec.Body)
	}
}

const sampleCORS = `<CORSConfiguration>
  <CORSRule>
    <ID>rule-1</ID>
    <AllowedOrigin>https://app.example.com</AllowedOrigin>
    <AllowedOrigin>https://*.cdn.example.com</AllowedOrigin>
    <AllowedMethod>GET</AllowedMethod>
    <AllowedMethod>PUT</AllowedMethod>
    <AllowedHeader>x-amz-*</AllowedHeader>
    <ExposeHeader>ETag</ExposeHeader>
    <MaxAgeSeconds>3000</MaxAgeSeconds>
  </CORSRule>
</CORSConfiguration>`

func TestPutGetDeleteBucketCors_RoundTrip(t *testing.T) {
	h, _ := newVersioningTestHandler()

	// Unconfigured → 404 NoSuchCORSConfiguration.
	req := httptest.NewRequest(http.MethodGet, "/bucket?cors", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET ?cors unconfigured = %d, want 404; body=%s", rec.Code, rec.Body)
	}
	if code := errCode(t, rec.Body.Bytes()); code != "NoSuchCORSConfiguration" {
		t.Fatalf("error code = %q, want NoSuchCORSConfiguration", code)
	}

	putCORS(t, h, "bucket", sampleCORS)

	// Read back and verify the rule survived the round-trip.
	req = httptest.NewRequest(http.MethodGet, "/bucket?cors", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?cors = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var doc corsConfiguration
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(doc.Rules))
	}
	r := doc.Rules[0]
	if r.ID != "rule-1" || r.MaxAgeSeconds != 3000 ||
		len(r.AllowedOrigins) != 2 || r.AllowedOrigins[1] != "https://*.cdn.example.com" ||
		strings.Join(r.AllowedMethods, ",") != "GET,PUT" ||
		strings.Join(r.AllowedHeaders, ",") != "x-amz-*" ||
		strings.Join(r.ExposeHeaders, ",") != "ETag" {
		t.Fatalf("round-trip mismatch: %+v", r)
	}

	// Delete → 204, then GET → 404 again.
	req = httptest.NewRequest(http.MethodDelete, "/bucket?cors", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE ?cors = %d, want 204; body=%s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest(http.MethodGet, "/bucket?cors", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET ?cors after delete = %d, want 404", rec.Code)
	}

	// Delete of an unconfigured bucket is an idempotent success.
	req = httptest.NewRequest(http.MethodDelete, "/bucket?cors", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE ?cors (no-op) = %d, want 204", rec.Code)
	}
}

func TestPutBucketCors_Rejections(t *testing.T) {
	h, _ := newVersioningTestHandler()

	// Malformed XML → 400 MalformedXML.
	req := httptest.NewRequest(http.MethodPut, "/bucket?cors", strings.NewReader("<CORSConfiguration><CORSRule>"))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed XML = %d, want 400; body=%s", rec.Code, rec.Body)
	}

	// Valid XML but invalid config (a rule with no method) → 400.
	req = httptest.NewRequest(http.MethodPut, "/bucket?cors",
		strings.NewReader("<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin></CORSRule></CORSConfiguration>"))
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rule with no method = %d, want 400; body=%s", rec.Code, rec.Body)
	}

	// Object-level path → 400 (cors is bucket-level only).
	req = httptest.NewRequest(http.MethodPut, "/bucket/key?cors", strings.NewReader(sampleCORS))
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("object-level ?cors = %d, want 400; body=%s", rec.Code, rec.Body)
	}
}

// TestBucketCors_NotImplementedWithoutStore verifies the handlers
// return 501 when the gateway has no BucketConfig store wired.
func TestBucketCors_NotImplementedWithoutStore(t *testing.T) {
	h := New(Config{Manifests: nil})
	for _, tc := range []struct {
		method, path string
		body         string
	}{
		{http.MethodPut, "/bucket?cors", sampleCORS},
		{http.MethodGet, "/bucket?cors", ""},
		{http.MethodDelete, "/bucket?cors", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		h.dispatch(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s without store = %d, want 501; body=%s", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
}

func TestApplyCORS_ActualRequest(t *testing.T) {
	h, _ := newVersioningTestHandler()
	putCORS(t, h, "bucket", sampleCORS)

	// A cross-origin GET with a matching Origin gets the CORS headers,
	// even though the object itself does not exist (headers attach
	// regardless of operation outcome).
	req := httptest.NewRequest(http.MethodGet, "/bucket/missing", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("Allow-Origin = %q, want echoed origin", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "GET, PUT" {
		t.Fatalf("Allow-Methods = %q, want 'GET, PUT'", got)
	}
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "ETag" {
		t.Fatalf("Expose-Headers = %q, want ETag", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("Vary = %q, want to contain Origin", got)
	}
}

func TestApplyCORS_NoHeadersWhenNoMatch(t *testing.T) {
	h, _ := newVersioningTestHandler()
	putCORS(t, h, "bucket", sampleCORS)

	cases := []struct {
		name   string
		origin string
		method string
	}{
		{"no origin (non-browser client)", "", http.MethodGet},
		{"origin not in any rule", "https://evil.example.com", http.MethodGet},
		{"method not allowed for origin", "https://app.example.com", http.MethodDelete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/bucket/obj", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.dispatch(rec, req)
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Fatalf("Allow-Origin = %q, want empty (no CORS headers)", got)
			}
		})
	}
}

func TestCORSPreflight(t *testing.T) {
	h, _ := newVersioningTestHandler()
	putCORS(t, h, "bucket", sampleCORS)

	// Matching preflight → 200 with allow headers echoed and max-age.
	req := httptest.NewRequest(http.MethodOptions, "/bucket/obj", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	req.Header.Set("Access-Control-Request-Headers", "x-amz-meta-foo, x-amz-acl")
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preflight match = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("Allow-Origin = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "x-amz-meta-foo, x-amz-acl" {
		t.Fatalf("Allow-Headers = %q, want echoed request headers", got)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "3000" {
		t.Fatalf("Max-Age = %q, want 3000", got)
	}

	// Missing Origin / Request-Method → 400.
	req = httptest.NewRequest(http.MethodOptions, "/bucket/obj", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("preflight missing headers = %d, want 400", rec.Code)
	}

	// Origin not allowed → 403.
	req = httptest.NewRequest(http.MethodOptions, "/bucket/obj", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("preflight bad origin = %d, want 403", rec.Code)
	}

	// Method not allowed → 403.
	req = httptest.NewRequest(http.MethodOptions, "/bucket/obj", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "DELETE")
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("preflight bad method = %d, want 403", rec.Code)
	}

	// Requested header not whitelisted → 403 (sample only allows x-amz-*).
	req = httptest.NewRequest(http.MethodOptions, "/bucket/obj", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	req.Header.Set("Access-Control-Request-Headers", "x-goog-meta-foo")
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("preflight disallowed header = %d, want 403", rec.Code)
	}
}

func TestCORSPreflight_NoConfig(t *testing.T) {
	h, _ := newVersioningTestHandler()
	// No CORS configured on the bucket → preflight rejected with 403.
	req := httptest.NewRequest(http.MethodOptions, "/bucket/obj", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("preflight no config = %d, want 403; body=%s", rec.Code, rec.Body)
	}
}

// errCode extracts the S3 error <Code> from a response body.
func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var e s3ErrorResponse
	if err := xml.Unmarshal(body, &e); err != nil {
		t.Fatalf("unmarshal error body: %v; body=%s", err, body)
	}
	return e.Code
}
