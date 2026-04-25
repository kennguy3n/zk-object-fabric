package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyendpoints "github.com/aws/smithy-go/endpoints"

	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// signAndBuild produces a SigV4-signed *http.Request for a given
// access key / secret key pair, using a fixed timestamp so tests are
// deterministic.
func signAndBuild(t *testing.T, method, path, body, accessKey, secretKey string, ts time.Time) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "example.s3.amazonaws.com"
	stamp := ts.UTC().Format("20060102T150405Z")
	date := ts.UTC().Format("20060102")
	r.Header.Set("x-amz-date", stamp)
	payloadSum := sha256.Sum256([]byte(body))
	r.Header.Set("x-amz-content-sha256", hex.EncodeToString(payloadSum[:]))

	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	p := parsedAuthHeader{
		AccessKey:     accessKey,
		Date:          date,
		Region:        "us-east-1",
		Service:       "s3",
		SignedHeaders: signed,
	}
	sig, err := signRequest(r, p, secretKey)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	authz := "AWS4-HMAC-SHA256 " +
		"Credential=" + accessKey + "/" + date + "/us-east-1/s3/aws4_request, " +
		"SignedHeaders=" + strings.Join(signed, ";") + ", " +
		"Signature=" + sig
	r.Header.Set("Authorization", authz)
	return r
}

func newStoreWithTenant(t *testing.T) (*MemoryTenantStore, string, string, string) {
	t.Helper()
	store := NewMemoryTenantStore()
	tid := "tenant-a"
	ak := "AKIDTEST"
	sk := "supersecret"
	if err := store.AddBinding(TenantBinding{
		AccessKey: ak,
		SecretKey: sk,
		Tenant:    tenant.Tenant{ID: tid, Name: "tenant-a"},
	}); err != nil {
		t.Fatalf("AddBinding: %v", err)
	}
	return store, tid, ak, sk
}

func TestHMACAuthenticator_RoundTrip(t *testing.T) {
	store, tid, ak, sk := newStoreWithTenant(t)
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	auth := &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        func() time.Time { return now },
		MaxClockSkew: time.Hour,
	}

	req := signAndBuild(t, "GET", "/bucket/key", "", ak, sk, now)
	got, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got != tid {
		t.Fatalf("tenantID = %q, want %q", got, tid)
	}
}

func TestHMACAuthenticator_WrongSecret(t *testing.T) {
	store, _, ak, _ := newStoreWithTenant(t)
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	auth := &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        func() time.Time { return now },
		MaxClockSkew: time.Hour,
	}
	req := signAndBuild(t, "GET", "/bucket/key", "", ak, "wrong-secret", now)
	if _, err := auth.Authenticate(req); err == nil {
		t.Fatal("Authenticate with wrong secret: want error, got nil")
	}
}

func TestHMACAuthenticator_UnknownAccessKey(t *testing.T) {
	store, _, _, _ := newStoreWithTenant(t)
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	auth := &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        func() time.Time { return now },
		MaxClockSkew: time.Hour,
	}
	req := signAndBuild(t, "GET", "/bucket/key", "", "UNKNOWN", "anything", now)
	if _, err := auth.Authenticate(req); err == nil {
		t.Fatal("Authenticate with unknown key: want error, got nil")
	}
}

func TestHMACAuthenticator_ClockSkew(t *testing.T) {
	store, _, ak, sk := newStoreWithTenant(t)
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-time.Hour)
	auth := &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        func() time.Time { return now },
		MaxClockSkew: 15 * time.Minute,
	}
	req := signAndBuild(t, "GET", "/bucket/key", "", ak, sk, stale)
	if _, err := auth.Authenticate(req); err == nil {
		t.Fatal("Authenticate on stale request: want error, got nil")
	}
}

// buildPresignedURL manually constructs a SigV4 query-string
// presigned URL using this package's signRequest helper so the
// presigned tests have a self-contained way to produce valid (and
// deliberately invalid) URLs.
func buildPresignedURL(t *testing.T, method, path, accessKey, secretKey string, signedAt time.Time, expiresSec int) string {
	t.Helper()
	const host = "example.s3.amazonaws.com"
	stamp := signedAt.UTC().Format("20060102T150405Z")
	date := signedAt.UTC().Format("20060102")
	cred := accessKey + "/" + date + "/us-east-1/s3/aws4_request"
	signed := []string{"host"}
	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", cred)
	q.Set("X-Amz-Date", stamp)
	q.Set("X-Amz-Expires", strconv.Itoa(expiresSec))
	q.Set("X-Amz-SignedHeaders", strings.Join(signed, ";"))
	rawQuery := q.Encode()

	signingReq := httptest.NewRequest(method, "http://"+host+path+"?"+rawQuery, nil)
	signingReq.Host = host
	signingReq.Header.Set("x-amz-date", stamp)
	p := parsedAuthHeader{
		AccessKey:     accessKey,
		Date:          date,
		Region:        "us-east-1",
		Service:       "s3",
		SignedHeaders: signed,
	}
	sig, err := signRequest(signingReq, p, secretKey)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	sort.Strings(signed)
	q.Set("X-Amz-Signature", sig)
	return "http://" + host + path + "?" + q.Encode()
}

// fixedEndpointResolver pins the AWS SDK v2 presigner to a specific
// host so the test can turn the resulting URL into an
// *http.Request without relying on the default virtual-host
// resolver.
type fixedEndpointResolver struct {
	host string
}

func (r fixedEndpointResolver) ResolveEndpoint(_ context.Context, _ s3.EndpointParameters) (smithyendpoints.Endpoint, error) {
	u, err := url.Parse("http://" + r.host)
	if err != nil {
		return smithyendpoints.Endpoint{}, err
	}
	return smithyendpoints.Endpoint{URI: *u}, nil
}

func TestHMACAuthenticator_PresignedRoundTrip_AWSSDKv2(t *testing.T) {
	store, tid, ak, sk := newStoreWithTenant(t)
	// AWS SDK v2 presigner uses time.Now internally; peg the
	// authenticator clock to wall time with a generous skew so the
	// test stays deterministic on slow machines.
	auth := &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        time.Now,
		MaxClockSkew: time.Hour,
	}

	const host = "example.s3.amazonaws.com"
	s3Client := s3.New(s3.Options{
		Region:             "us-east-1",
		Credentials:        credentials.NewStaticCredentialsProvider(ak, sk, ""),
		UsePathStyle:       true,
		EndpointResolverV2: fixedEndpointResolver{host: host},
	})
	presigner := s3.NewPresignClient(s3Client, func(o *s3.PresignOptions) {
		o.Expires = 15 * time.Minute
	})

	cases := []struct {
		name string
		call func() (string, string, error)
	}{
		{
			name: "PutObject",
			call: func() (string, string, error) {
				req, err := presigner.PresignPutObject(context.Background(), &s3.PutObjectInput{
					Bucket: aws.String("bucket"),
					Key:    aws.String("tenant-a/file/version"),
				})
				if err != nil {
					return "", "", err
				}
				return req.Method, req.URL, nil
			},
		},
		{
			name: "GetObject",
			call: func() (string, string, error) {
				req, err := presigner.PresignGetObject(context.Background(), &s3.GetObjectInput{
					Bucket: aws.String("bucket"),
					Key:    aws.String("tenant-a/file/version"),
				})
				if err != nil {
					return "", "", err
				}
				return req.Method, req.URL, nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method, reqURL, err := tc.call()
			if err != nil {
				t.Fatalf("presign: %v", err)
			}
			httpReq := httptest.NewRequest(method, reqURL, nil)
			got, err := auth.Authenticate(httpReq)
			if err != nil {
				t.Fatalf("Authenticate(%s): %v (url=%s)", tc.name, err, reqURL)
			}
			if got != tid {
				t.Fatalf("tenantID = %q, want %q", got, tid)
			}
		})
	}
}

func TestHMACAuthenticator_PresignedExpired(t *testing.T) {
	store, _, ak, sk := newStoreWithTenant(t)
	signedAt := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	later := signedAt.Add(2 * time.Hour)
	auth := &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        func() time.Time { return later },
		MaxClockSkew: 15 * time.Minute,
	}
	u := buildPresignedURL(t, "GET", "/bucket/key", ak, sk, signedAt, 15*60)
	req := httptest.NewRequest("GET", u, nil)
	if _, err := auth.Authenticate(req); err == nil {
		t.Fatal("Authenticate expired presigned: want error, got nil")
	}
}

func TestHMACAuthenticator_PresignedWrongSecret(t *testing.T) {
	store, _, ak, _ := newStoreWithTenant(t)
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	auth := &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        func() time.Time { return now },
		MaxClockSkew: time.Hour,
	}
	u := buildPresignedURL(t, "GET", "/bucket/key", ak, "wrong-secret", now, 900)
	req := httptest.NewRequest("GET", u, nil)
	if _, err := auth.Authenticate(req); err == nil {
		t.Fatal("Authenticate presigned wrong secret: want error, got nil")
	}
}

func TestHMACAuthenticator_PresignedTamperedQuery(t *testing.T) {
	store, _, ak, sk := newStoreWithTenant(t)
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	auth := &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        func() time.Time { return now },
		MaxClockSkew: time.Hour,
	}
	u := buildPresignedURL(t, "GET", "/bucket/key", ak, sk, now, 900)
	tampered := u + "&extra=evil"
	req := httptest.NewRequest("GET", tampered, nil)
	if _, err := auth.Authenticate(req); err == nil {
		t.Fatal("Authenticate tampered presigned: want error, got nil")
	}
}

// signAndBuildWithDateHeader produces a SigV4-signed request that
// carries the timestamp on the standard RFC1123 Date header instead
// of x-amz-date. The signing path normalises the timestamp and
// places x-amz-date on its signing clone, so the on-the-wire
// canonical request must match what the server reconstructs.
func signAndBuildWithDateHeader(t *testing.T, method, path, body, accessKey, secretKey string, ts time.Time) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "example.s3.amazonaws.com"
	stamp := ts.UTC().Format("20060102T150405Z")
	date := ts.UTC().Format("20060102")
	r.Header.Set("Date", ts.UTC().Format(time.RFC1123))
	payloadSum := sha256.Sum256([]byte(body))
	r.Header.Set("x-amz-content-sha256", hex.EncodeToString(payloadSum[:]))

	signed := []string{"host", "x-amz-content-sha256"}
	sort.Strings(signed)

	// Build the signing clone the same way the server will: place
	// the normalised x-amz-date header on a copy of r so the
	// canonical request hashes identically.
	signingClone := r.Clone(r.Context())
	signingClone.Header.Set("x-amz-date", stamp)

	p := parsedAuthHeader{
		AccessKey:     accessKey,
		Date:          date,
		Region:        "us-east-1",
		Service:       "s3",
		SignedHeaders: signed,
	}
	sig, err := signRequest(signingClone, p, secretKey)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	authz := "AWS4-HMAC-SHA256 " +
		"Credential=" + accessKey + "/" + date + "/us-east-1/s3/aws4_request, " +
		"SignedHeaders=" + strings.Join(signed, ";") + ", " +
		"Signature=" + sig
	r.Header.Set("Authorization", authz)
	return r
}

func TestHMACAuthenticator_DateHeaderFallback(t *testing.T) {
	store, tid, ak, sk := newStoreWithTenant(t)
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	auth := &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        func() time.Time { return now },
		MaxClockSkew: time.Hour,
	}

	req := signAndBuildWithDateHeader(t, "GET", "/bucket/key", "", ak, sk, now)
	got, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate (Date header fallback): %v", err)
	}
	if got != tid {
		t.Fatalf("tenantID = %q, want %q", got, tid)
	}
}

func TestHMACAuthenticator_NoAuthHeaders(t *testing.T) {
	store, _, _, _ := newStoreWithTenant(t)
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	auth := &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        func() time.Time { return now },
		MaxClockSkew: time.Hour,
	}
	// Request with neither an Authorization header nor an
	// X-Amz-Signature query parameter must be rejected with a
	// stable, descriptive error so operators can grep for it.
	req := httptest.NewRequest("GET", "/bucket/key", nil)
	req.Host = "example.s3.amazonaws.com"
	_, err := auth.Authenticate(req)
	if err == nil {
		t.Fatal("Authenticate without credentials: want error, got nil")
	}
	if !strings.Contains(err.Error(), "no SigV4 credential presented") {
		t.Fatalf("unexpected error %q, want a 'no SigV4 credential presented' message", err)
	}
}

func TestHMACAuthenticator_HeaderMissingDate(t *testing.T) {
	store, _, ak, sk := newStoreWithTenant(t)
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	auth := &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        func() time.Time { return now },
		MaxClockSkew: time.Hour,
	}
	// signAndBuild attaches x-amz-date; strip it (and Date) so the
	// strategy hits the new "missing x-amz-date or Date header"
	// error message that replaces the old "missing x-amz-date".
	req := signAndBuild(t, "GET", "/bucket/key", "", ak, sk, now)
	req.Header.Del("x-amz-date")
	req.Header.Del("Date")
	_, err := auth.Authenticate(req)
	if err == nil {
		t.Fatal("Authenticate without date header: want error, got nil")
	}
	if !strings.Contains(err.Error(), "missing x-amz-date or Date header") {
		t.Fatalf("unexpected error %q, want 'missing x-amz-date or Date header'", err)
	}
}

func TestHMACAuthenticator_ChunkedSeedSignature(t *testing.T) {
	store, tid, ak, sk := newStoreWithTenant(t)
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	auth := &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        func() time.Time { return now },
		MaxClockSkew: time.Hour,
	}

	// signAndBuild already produces a header-signed SigV4 request
	// with x-amz-content-sha256 set to the body's hash. For an
	// aws-chunked request the convention is that
	// x-amz-content-sha256 is the streaming sentinel and
	// Content-Encoding advertises aws-chunked. The seed signature
	// is reused as parsed.Signature.
	stamp := now.UTC().Format("20060102T150405Z")
	date := now.UTC().Format("20060102")
	r := httptest.NewRequest("PUT", "/bucket/key", strings.NewReader(""))
	r.Host = "example.s3.amazonaws.com"
	r.Header.Set("x-amz-date", stamp)
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("x-amz-content-sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD")
	r.Header.Set("x-amz-decoded-content-length", "16")

	signed := []string{"content-encoding", "host", "x-amz-content-sha256", "x-amz-date", "x-amz-decoded-content-length"}
	sort.Strings(signed)
	p := parsedAuthHeader{
		AccessKey:     ak,
		Date:          date,
		Region:        "us-east-1",
		Service:       "s3",
		SignedHeaders: signed,
	}
	seedSig, err := signRequest(r, p, sk)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	authz := "AWS4-HMAC-SHA256 " +
		"Credential=" + ak + "/" + date + "/us-east-1/s3/aws4_request, " +
		"SignedHeaders=" + strings.Join(signed, ";") + ", " +
		"Signature=" + seedSig
	r.Header.Set("Authorization", authz)

	res, err := auth.AuthenticateEx(r)
	if err != nil {
		t.Fatalf("AuthenticateEx: %v", err)
	}
	if res.TenantID != tid {
		t.Fatalf("tenantID = %q, want %q", res.TenantID, tid)
	}
	if !res.IsChunked {
		t.Fatal("expected IsChunked=true on aws-chunked request")
	}
	if res.SeedSig != seedSig {
		t.Fatalf("SeedSig = %q, want %q", res.SeedSig, seedSig)
	}
	if len(res.SigningKey) == 0 {
		t.Fatal("expected non-empty SigningKey on chunked result")
	}
	wantScope := date + "/us-east-1/s3/aws4_request"
	if res.Scope != wantScope {
		t.Fatalf("Scope = %q, want %q", res.Scope, wantScope)
	}
	if res.Timestamp != stamp {
		t.Fatalf("Timestamp = %q, want %q", res.Timestamp, stamp)
	}

	// VerifyChunkSignature should be deterministic given the seed
	// signature, signing key, and chunk bytes; running it twice on
	// the same input must produce the same output.
	chunk := []byte("hello, chunked!\n")
	first, err := VerifyChunkSignature(res.SeedSig, chunk, res.SigningKey, res.Timestamp, res.Scope)
	if err != nil {
		t.Fatalf("VerifyChunkSignature: %v", err)
	}
	second, err := VerifyChunkSignature(res.SeedSig, chunk, res.SigningKey, res.Timestamp, res.Scope)
	if err != nil {
		t.Fatalf("VerifyChunkSignature (second call): %v", err)
	}
	if first != second {
		t.Fatalf("VerifyChunkSignature is not deterministic: %q vs %q", first, second)
	}
	if len(first) != 64 {
		t.Fatalf("VerifyChunkSignature returned %d hex chars, want 64", len(first))
	}
}

func TestVerifyChunkSignature_RequiresKeyAndScope(t *testing.T) {
	if _, err := VerifyChunkSignature("seed", []byte("data"), nil, "ts", "scope"); err == nil {
		t.Fatal("VerifyChunkSignature with nil key: want error, got nil")
	}
	if _, err := VerifyChunkSignature("seed", []byte("data"), []byte("k"), "", "scope"); err == nil {
		t.Fatal("VerifyChunkSignature with empty timestamp: want error, got nil")
	}
	if _, err := VerifyChunkSignature("seed", []byte("data"), []byte("k"), "ts", ""); err == nil {
		t.Fatal("VerifyChunkSignature with empty scope: want error, got nil")
	}
}

func TestRateLimiter_AllowsBurstThenThrottles(t *testing.T) {
	now := time.Unix(0, 0)
	rl := NewRateLimiter(
		func(tid string) (int, int, bool) { return 1, 2, true },
		func(r *http.Request) (string, bool) { return "t", true },
	)
	rl.Clock = func() time.Time { return now }

	if !rl.Allow("t") {
		t.Fatal("first call: want allow")
	}
	if !rl.Allow("t") {
		t.Fatal("second call within burst: want allow")
	}
	if rl.Allow("t") {
		t.Fatal("third call over burst: want deny")
	}
	// Advance time to refill.
	now = now.Add(2 * time.Second)
	if !rl.Allow("t") {
		t.Fatal("after refill: want allow")
	}
}

func TestRateLimiter_UnknownTenantPassesThrough(t *testing.T) {
	rl := NewRateLimiter(
		func(string) (int, int, bool) { return 0, 0, false },
		func(r *http.Request) (string, bool) { return "", false },
	)
	if !rl.Allow("anything") {
		t.Fatal("unknown tenant: want allow")
	}
}
