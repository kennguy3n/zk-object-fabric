package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
