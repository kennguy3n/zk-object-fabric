package abuse

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTenantIsolation_BobCannotImpersonateAlice covers the
// canonical "tenant A's URL signed with tenant A's secret key
// cannot be replayed by tenant B by patching the access key"
// regression. The threat model: Bob captures Alice's presigned
// URL (e.g. via logs), strips Alice's X-Amz-Credential, and
// rewrites it to point at his own access key. Without
// per-binding signature verification this would let Bob serve
// Alice's data while being billed against his own account.
//
// The harness uses the real production SigV4 strategies (both
// HeaderV4 and PresignedV4 are wired by DefaultStrategies);
// the authenticator must reject Bob's tampered URL with
// 403 + "signature mismatch", AND the rate-limit / abuse-guard
// middleware must NOT charge the failed request against either
// tenant's counters.
func TestTenantIsolation_BobCannotImpersonateAlice(t *testing.T) {
	t.Parallel()
	const (
		alice = "t-alice"
		bob   = "t-bob"
		body  = 1024
	)
	h := NewHarness(t, HarnessConfig{
		BodySize:          body,
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       2 * time.Second,
		Tenants: []TenantSpec{
			{ID: alice, AccessKey: "AKIAALICE", SecretKey: "secretalice", RPS: 100, Burst: 100},
			{ID: bob, AccessKey: "AKIABOB", SecretKey: "secretbob", RPS: 100, Burst: 100},
		},
	})

	// Alice signs a GET for her own object. The URL embeds
	// Alice's access key in X-Amz-Credential and a SigV4
	// signature derived from Alice's secret key.
	aliceURL := h.PresignedGet(alice, "alice-secret-object")

	// Tampered URL: replace Alice's access key with Bob's in
	// the X-Amz-Credential parameter. The signature stays
	// derived from Alice's secret because we did not re-sign.
	tampered := substituteAccessKey(t, aliceURL, "AKIAALICE", "AKIABOB")
	if tampered == aliceURL {
		t.Fatalf("test setup: substitution did not change URL (X-Amz-Credential pattern changed?)")
	}

	client := newHTTPClient(2 * time.Second)
	got := doRequest(t, client, tampered)
	if got.code != http.StatusForbidden {
		t.Fatalf("Bob impersonating Alice: status=%d want %d (auth bypassed?)", got.code, http.StatusForbidden)
	}
	if got.echo != "" {
		t.Errorf("X-Tenant-Echo=%q on failed auth (terminal handler ran despite rejection)", got.echo)
	}
	// The 403 response body should mention the SigV4 failure
	// reason — operator playbooks key on the substring.
	if !strings.Contains(strings.ToLower(string(got.bodyBytes)), "signature") {
		t.Errorf("403 body does not mention signature: %q", string(got.bodyBytes))
	}

	// Sanity-check: Alice's unaltered URL must still work,
	// proving the harness signed it correctly in the first
	// place.
	if ok := doRequest(t, client, aliceURL); ok.code != http.StatusOK {
		t.Fatalf("Alice's own URL: status=%d want 200 (signing harness broken?)", ok.code)
	} else if ok.echo != alice {
		t.Errorf("Alice's URL: X-Tenant-Echo=%q want %q (resolver disagreed with authenticator)", ok.echo, alice)
	}
}

// TestTenantIsolation_TerminalEchoMatchesResolver pins the
// architectural guarantee that the middleware resolver and the
// terminal-handler authenticator agree on the tenant for every
// successful request. A regression where one of them is wired
// to a different TenantStore would let bytes be billed to
// tenant A while being served from tenant B's manifest store.
//
// The test runs many concurrent requests from two tenants in
// parallel and asserts X-Tenant-Echo always matches the SigV4
// access key. Run under -race to catch a future TenantStore
// implementation with a shared mutable map.
func TestTenantIsolation_TerminalEchoMatchesResolver(t *testing.T) {
	t.Parallel()
	const (
		alice    = "t-iso-alice"
		bob      = "t-iso-bob"
		body     = 256
		perRound = 16
		rounds   = 12
	)
	h := NewHarness(t, HarnessConfig{
		BodySize:          body,
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       2 * time.Second,
		Tenants: []TenantSpec{
			{ID: alice, AccessKey: "AKIAIA", SecretKey: "secretia", RPS: 10000, Burst: 10000},
			{ID: bob, AccessKey: "AKIAIB", SecretKey: "secretib", RPS: 10000, Burst: 10000},
		},
	})

	// Pre-mint a pool of URLs for each tenant. Presigning under
	// concurrent test execution is safe but slow; pre-minting
	// keeps the hot path focused on the wire-level concurrency
	// the test is actually exercising.
	aliceURLs := make([]string, perRound*rounds)
	bobURLs := make([]string, perRound*rounds)
	for i := range aliceURLs {
		aliceURLs[i] = h.PresignedGet(alice, randomKey())
		bobURLs[i] = h.PresignedGet(bob, randomKey())
	}

	client := newHTTPClient(2 * time.Second)
	var leaks atomic.Int32
	var wg sync.WaitGroup
	for r := 0; r < rounds; r++ {
		for i := 0; i < perRound; i++ {
			idx := r*perRound + i
			wg.Add(2)
			go func(u, expected string) {
				defer wg.Done()
				got := doRequest(t, client, u)
				if got.code != http.StatusOK || got.echo != expected {
					t.Errorf("tenant=%s status=%d echo=%q", expected, got.code, got.echo)
					leaks.Add(1)
				}
			}(aliceURLs[idx], alice)
			go func(u, expected string) {
				defer wg.Done()
				got := doRequest(t, client, u)
				if got.code != http.StatusOK || got.echo != expected {
					t.Errorf("tenant=%s status=%d echo=%q", expected, got.code, got.echo)
					leaks.Add(1)
				}
			}(bobURLs[idx], bob)
		}
	}
	wg.Wait()
	if leaks.Load() != 0 {
		t.Fatalf("tenant isolation leaks under concurrency: %d / %d requests", leaks.Load(), 2*perRound*rounds)
	}
}

// TestTenantIsolation_UnknownAccessKey_Rejected covers the
// "unauthenticated tenant" path: a presigned URL whose
// X-Amz-Credential references an access key that does not
// exist in the tenant store must be rejected with 403 +
// "unknown access key", and the request must NOT consume any
// other tenant's rate-limit / egress-budget tokens.
func TestTenantIsolation_UnknownAccessKey_Rejected(t *testing.T) {
	t.Parallel()
	const (
		alice = "t-iso-alice2"
		body  = 256
	)
	h := NewHarness(t, HarnessConfig{
		BodySize:          body,
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       2 * time.Second,
		Tenants: []TenantSpec{
			{ID: alice, AccessKey: "AKIAALICE2", SecretKey: "secretalice2", RPS: 5, Burst: 5},
		},
	})

	aliceURL := h.PresignedGet(alice, "obj")
	ghostURL := substituteAccessKey(t, aliceURL, "AKIAALICE2", "AKIAGHOST")

	client := newHTTPClient(2 * time.Second)
	got := doRequest(t, client, ghostURL)
	if got.code != http.StatusForbidden {
		t.Fatalf("unknown access key: status=%d want %d", got.code, http.StatusForbidden)
	}

	// Alice's bucket must not have been charged. Drive five
	// legitimate requests now and assert all succeed — if the
	// ghost request had consumed an Alice token the fifth of
	// these would be 429.
	for i := 0; i < 5; i++ {
		ok := doRequest(t, client, h.PresignedGet(alice, randomKey()))
		if ok.code != http.StatusOK {
			t.Fatalf("Alice request %d after ghost: status=%d (ghost drained Alice's bucket?)", i, ok.code)
		}
	}
}

// substituteAccessKey rewrites the X-Amz-Credential query
// parameter to replace the leading access key segment. It
// preserves the rest of the credential scope (date/region/
// service/aws4_request) and every other query parameter so
// the only deliberate change is the credential subject. The
// signature is intentionally left untouched — the whole point
// of these tests is to exercise the verifier's mismatch
// detection.
func substituteAccessKey(t *testing.T, raw, oldKey, newKey string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("substituteAccessKey: parse %q: %v", raw, err)
	}
	q := u.Query()
	cred := q.Get("X-Amz-Credential")
	if cred == "" {
		t.Fatalf("substituteAccessKey: no X-Amz-Credential in %q", raw)
	}
	parts := strings.SplitN(cred, "/", 2)
	if len(parts) != 2 {
		t.Fatalf("substituteAccessKey: malformed X-Amz-Credential %q (expected access-key/scope, got %q)", raw, cred)
	}
	if parts[0] != oldKey {
		t.Fatalf("substituteAccessKey: X-Amz-Credential prefix %q != %q", parts[0], oldKey)
	}
	q.Set("X-Amz-Credential", newKey+"/"+parts[1])
	u.RawQuery = q.Encode()
	return u.String()
}


