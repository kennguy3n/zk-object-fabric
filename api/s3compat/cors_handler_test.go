package s3compat

import (
	"encoding/xml"
	"errors"
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
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Allow-Credentials = %q, want true", got)
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
			// AWS still sets Vary: Origin when an Origin is present but no
			// rule matches, so caches key on the origin. With no Origin
			// (non-browser client) applyCORS returns before touching Vary.
			gotVary := rec.Header().Get("Vary")
			if tc.origin == "" && gotVary != "" {
				t.Fatalf("Vary = %q, want empty for no-Origin request", gotVary)
			}
			if tc.origin != "" && !strings.Contains(gotVary, "Origin") {
				t.Fatalf("Vary = %q, want to contain Origin on no-match", gotVary)
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
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("preflight Allow-Credentials = %q, want true", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("preflight Vary = %q, want to contain Origin", got)
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

	// Origin present but Request-Method absent → 400, and it still
	// carries Vary: Origin (the response varies by origin even though
	// the preflight is malformed).
	req = httptest.NewRequest(http.MethodOptions, "/bucket/obj", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("preflight Origin-only = %d, want 400", rec.Code)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("400 preflight Vary = %q, want to contain Origin", got)
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
	// Even a rejected (403) preflight carries Vary: Origin so a cache
	// never reuses it across origins (matches AWS).
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("403 preflight Vary = %q, want to contain Origin", got)
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

// fakeAuth is a minimal Authenticator + TenantResolver for the
// production-path CORS tests. It maps a presigned access key to a
// tenant. Authenticate (the signature-verifying path) succeeds only
// for a fully-signed request (one carrying X-Amz-Signature) — a
// browser preflight, which carries no signature, always errors, just
// like the real authenticator. ResolveTenantUnverified resolves the
// tenant from the X-Amz-Credential alone, no signature required,
// which is what makes a presigned-URL preflight work.
type fakeAuth struct {
	keys      map[string]string // accessKey -> tenantID
	authCalls *int              // optional Authenticate call counter
}

func (f fakeAuth) tenantFor(r *http.Request) (string, bool) {
	cred := r.URL.Query().Get("X-Amz-Credential")
	if cred == "" {
		return "", false
	}
	segs := strings.Split(cred, "/")
	tid, ok := f.keys[segs[0]]
	return tid, ok
}

func (f fakeAuth) Authenticate(r *http.Request) (string, error) {
	if f.authCalls != nil {
		*f.authCalls++
	}
	if r.URL.Query().Get("X-Amz-Signature") == "" {
		return "", errors.New("auth: no signature presented")
	}
	tid, ok := f.tenantFor(r)
	if !ok {
		return "", errors.New("auth: unknown access key")
	}
	return tid, nil
}

func (f fakeAuth) ResolveTenantUnverified(r *http.Request) (string, bool) {
	return f.tenantFor(r)
}

// presignedURL builds a path carrying a presigned X-Amz-Credential for
// accessKey (and, when signed, an X-Amz-Signature) so fakeAuth can
// resolve / authenticate it.
func presignedURL(path, accessKey string, signed bool) string {
	q := "X-Amz-Credential=" + accessKey + "%2F20260101%2Fus-east-1%2Fs3%2Faws4_request"
	if signed {
		q += "&X-Amz-Signature=deadbeef"
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + q
}

// TestBucketCors_AuthenticatedTenant exercises the production path
// (Auth != nil): CORS rules stored by an authenticated tenant must be
// found by that tenant's unauthenticated, presigned-URL preflight —
// the scenario BUG-0001 broke when the preflight fell back to the
// anonymous tenant.
func TestBucketCors_AuthenticatedTenant(t *testing.T) {
	h, _ := newVersioningTestHandler()
	h.cfg.Auth = fakeAuth{keys: map[string]string{"AKIDA": "tenant-a", "AKIDB": "tenant-b"}}
	h.cfg.RequireAuth = true

	// tenant-a stores CORS through the authenticated PUT path.
	req := httptest.NewRequest(http.MethodPut,
		presignedURL("/bucket?cors", "AKIDA", true), strings.NewReader(sampleCORS))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated PUT ?cors = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	// tenant-a's presigned-URL preflight (unsigned, as browsers send it)
	// resolves tenant-a from X-Amz-Credential and finds the rule → 200.
	req = httptest.NewRequest(http.MethodOptions, presignedURL("/bucket/obj", "AKIDA", false), nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authed-tenant preflight = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("Allow-Origin = %q, want echoed origin", got)
	}
	// A preflight must not carry Expose-Headers (AWS omits it there).
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "" {
		t.Fatalf("preflight Expose-Headers = %q, want empty", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("authed-tenant preflight Allow-Credentials = %q, want true", got)
	}

	// A different tenant (tenant-b) has no CORS on this bucket name, so
	// its preflight is rejected — proving tenant isolation holds.
	req = httptest.NewRequest(http.MethodOptions, presignedURL("/bucket/obj", "AKIDB", false), nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("other-tenant preflight = %d, want 403", rec.Code)
	}

	// A preflight with no credential at all cannot resolve a tenant → 403.
	req = httptest.NewRequest(http.MethodOptions, "/bucket/obj", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("credential-less preflight = %d, want 403", rec.Code)
	}
}

// TestApplyCORS_AuthenticatedActualRequest verifies the actual
// (signed) cross-origin request gets CORS headers under a real
// Authenticator, and that the tenant is authenticated only once per
// request (applyCORS + the operation handler share the memo).
func TestApplyCORS_AuthenticatedActualRequest(t *testing.T) {
	h, _ := newVersioningTestHandler()
	calls := 0
	h.cfg.Auth = fakeAuth{keys: map[string]string{"AKIDA": "tenant-a"}, authCalls: &calls}
	h.cfg.RequireAuth = true
	// Store tenant-a's CORS through the authenticated PUT path.
	req := httptest.NewRequest(http.MethodPut,
		presignedURL("/bucket?cors", "AKIDA", true), strings.NewReader(sampleCORS))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated PUT ?cors = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	calls = 0 // count only the cross-origin actual request below

	req = httptest.NewRequest(http.MethodGet, presignedURL("/bucket/missing", "AKIDA", true), nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("Allow-Origin = %q, want echoed origin; body=%s", got, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "ETag" {
		t.Fatalf("actual-request Expose-Headers = %q, want ETag", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("actual-request Allow-Credentials = %q, want true", got)
	}
	// applyCORS and the Get handler both call authenticate; the
	// per-request memo must collapse them into a single resolution.
	if calls != 1 {
		t.Fatalf("Authenticate called %d times, want 1 (per-request memo)", calls)
	}
}

// TestApplyCORS_AuthFailureStillSetsCORSHeaders verifies that a
// cross-origin request that FAILS signature verification (e.g. an
// expired presigned PUT from a browser SPA) still gets CORS headers on
// its error response, resolving the tenant unverified from the
// presigned credential. Without this the browser would surface an
// opaque CORS error instead of the real 403.
func TestApplyCORS_AuthFailureStillSetsCORSHeaders(t *testing.T) {
	h, _ := newVersioningTestHandler()
	h.cfg.Auth = fakeAuth{keys: map[string]string{"AKIDA": "tenant-a"}}
	h.cfg.RequireAuth = true
	// Store tenant-a's CORS through the authenticated (signed) PUT path.
	req := httptest.NewRequest(http.MethodPut,
		presignedURL("/bucket?cors", "AKIDA", true), strings.NewReader(sampleCORS))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated PUT ?cors = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	// An UNSIGNED presigned PUT (signature verification fails) carries
	// X-Amz-Credential naming tenant-a. The operation is rejected, but
	// the CORS headers must still attach so the SPA can read the error.
	req = httptest.NewRequest(http.MethodPut,
		presignedURL("/bucket/obj", "AKIDA", false), strings.NewReader("data"))
	req.Header.Set("Origin", "https://app.example.com")
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("unsigned PUT = %d, want an auth failure (non-200)", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("auth-failure Allow-Origin = %q, want echoed origin; body=%s", got, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("auth-failure Allow-Credentials = %q, want true", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("auth-failure Vary = %q, want to contain Origin", got)
	}

	// A credential we can't resolve to a tenant gets no CORS headers
	// (we can't know which tenant's rules apply).
	req = httptest.NewRequest(http.MethodPut,
		presignedURL("/bucket/obj", "UNKNOWN", false), strings.NewReader("data"))
	req.Header.Set("Origin", "https://app.example.com")
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("unresolvable-tenant Allow-Origin = %q, want empty", got)
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
