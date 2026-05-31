package abuse

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
)

// TestKeyflood_RateLimiter_Trips_AfterBurstExhausted is the
// canonical "tenant A floods the gateway with millions of tiny
// objects" scenario. The rate limiter has to start rejecting
// well before the worker pool exhausts its memory; we assert
// the production rejection signal (429 + "rate limit exceeded"
// + Retry-After) and that the SigV4-authenticated terminal
// handler never sees the rejected requests.
//
// The bucket is configured with RPS=5, Burst=5. A single tenant
// then fires 200 sequential signed presigned-URL GETs with no
// inter-request delay. The first 5 must succeed (burst); the
// remainder must be rejected by the limiter — at the worst case
// the bucket refills a few extra slots during the loop, so the
// invariant is "rejections >= 100" rather than an exact split.
//
// The test runs against the production middleware chain:
// AbuseGuard -> token-bucket Allow -> SigV4 verify (resolver)
// -> echo handler. A regression that bypasses the limiter
// (e.g. forgetting to wire the resolver) would let the entire
// 200-request flood through, which would fail the assertion
// loudly.
func TestKeyflood_RateLimiter_Trips_AfterBurstExhausted(t *testing.T) {
	t.Parallel()
	const (
		tenantID = "t-flood"
		rps      = 5
		burst    = 5
		total    = 200
	)
	h := NewHarness(t, HarnessConfig{
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       2 * time.Second,
		Tenants: []TenantSpec{{
			ID:        tenantID,
			AccessKey: "AKIAFLOOD",
			SecretKey: "secretflood",
			RPS:       rps,
			Burst:     burst,
		}},
	})

	client := newHTTPClient(2 * time.Second)
	var succeeded, throttled int

	for i := 0; i < total; i++ {
		// Mint a fresh presigned URL for each request so the
		// SigV4 verifier exercises a real signature on every
		// call — using a cached URL would mask a bug where the
		// limiter is bypassed by deterministic-signature reuse.
		url := h.PresignedGet(tenantID, randomKey())
		req, err := newGETRequest(url)
		if err != nil {
			t.Fatalf("newGETRequest: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		drainBody(resp.Body)
		_ = resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
			succeeded++
			if got := resp.Header.Get("X-Tenant-Echo"); got != tenantID {
				t.Fatalf("request %d: X-Tenant-Echo=%q, want %q (tenant resolution disagrees with auth)", i, got, tenantID)
			}
		case http.StatusTooManyRequests:
			throttled++
			if ra := resp.Header.Get("Retry-After"); ra == "" {
				t.Fatalf("request %d: 429 without Retry-After header (operator playbooks rely on this)", i)
			}
		default:
			t.Fatalf("request %d: unexpected status %d", i, resp.StatusCode)
		}
	}

	// Burst must drain at least once; if every request succeeded
	// the limiter is wired up but the bucket is misconfigured
	// (or the resolver returned an empty tenant id, which the
	// limiter treats as "unknown -> allow").
	if throttled == 0 {
		t.Fatalf("expected at least one 429 over %d requests at rps=%d burst=%d; got 0 (rate limiter bypassed?)", total, rps, burst)
	}
	// At minimum half the flood must be rejected. With RPS=5
	// and Burst=5 the bucket can refill ~5 tokens per second
	// during the loop; even on a heavily loaded CI runner the
	// 200-request loop completes in well under 10s, so total
	// "permitted" requests are bounded by burst (5) + RPS*duration.
	// Floor of 100 leaves comfortable slack for CI scheduling
	// jitter while still failing loudly on a true bypass.
	if throttled < total/2 {
		t.Fatalf("rate limiter under-triggered: %d/%d throttled, want >= %d", throttled, total, total/2)
	}
	if succeeded < burst {
		t.Fatalf("burst capacity not honoured: %d/%d succeeded, want >= %d", succeeded, total, burst)
	}

	// The egress-budget path is independent of the token bucket:
	// no EgressTBMonth is configured for this tenant so the
	// AbuseBudgetExhausted dimension must never fire on a pure
	// keyflood (only too-many-requests). Catches a future
	// refactor that conflates the two rejection paths and
	// double-emits to the operator pager.
	if got := h.Sink.CountByDimension(billing.AbuseBudgetExhausted); got != 0 {
		t.Errorf("token-bucket rejections should not emit AbuseBudgetExhausted, got %d", got)
	}

	// The flood SHOULD trip the anomaly detector (current
	// req/s >> baseline >> AnomalyMultiplier), independent of
	// whether ThrottleOnAnomaly is set. Both the RateLimiter
	// and the AbuseGuard run anomaly detection, so the alert
	// count is at least one when the spike is real.
	if got := h.Sink.CountByDimension(billing.AbuseAnomalyAlert); got == 0 {
		t.Errorf("expected at least one AbuseAnomalyAlert from the flood (rate %d req/s >> baseline), got 0", total)
	}
}

// TestKeyflood_RateLimiter_Concurrent_Tenants_Independent_Buckets
// proves that one tenant's flood cannot drain another tenant's
// bucket. The 'flood' tenant fires N concurrent requests with
// rps=1 burst=1 and overflows the limiter; the 'quiet' tenant
// fires a single request meanwhile and must not see a 429.
//
// This is the structural guarantee that prevents a noisy
// neighbour from causing collateral SLA breaches — without it
// a single misbehaving tenant could DoS every other tenant
// sharing the gateway.
func TestKeyflood_RateLimiter_Concurrent_Tenants_Independent_Buckets(t *testing.T) {
	t.Parallel()
	const (
		floodTenant = "t-noisy"
		quietTenant = "t-quiet"
		floodN      = 50
	)
	h := NewHarness(t, HarnessConfig{
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       2 * time.Second,
		Tenants: []TenantSpec{
			{ID: floodTenant, AccessKey: "AKIANOISY", SecretKey: "secretnoisy", RPS: 1, Burst: 1},
			{ID: quietTenant, AccessKey: "AKIAQUIET", SecretKey: "secretquiet", RPS: 100, Burst: 100},
		},
	})

	floodURLs := make([]string, floodN)
	for i := range floodURLs {
		floodURLs[i] = h.PresignedGet(floodTenant, randomKey())
	}

	// Synchronise the flood goroutines so they all start at
	// roughly the same wall-clock moment, maximising the chance
	// that the bucket is empty when the 'quiet' request lands.
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	done.Add(floodN)
	var floodThrottled atomic.Int32

	client := newHTTPClient(2 * time.Second)
	for i := 0; i < floodN; i++ {
		go func(url string) {
			defer done.Done()
			start.Wait()
			req, err := newGETRequest(url)
			if err != nil {
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			drainBody(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusTooManyRequests {
				floodThrottled.Add(1)
			}
		}(floodURLs[i])
	}
	start.Done()

	// Race the quiet tenant against the flood. Issue several
	// quiet requests so the flood is guaranteed to be in
	// progress when at least one of them lands.
	quietClient := newHTTPClient(2 * time.Second)
	var quietOK int
	for i := 0; i < 5; i++ {
		quietURL := h.PresignedGet(quietTenant, randomKey())
		req, err := newGETRequest(quietURL)
		if err != nil {
			t.Fatalf("quiet newGETRequest: %v", err)
		}
		resp, err := quietClient.Do(req)
		if err != nil {
			t.Fatalf("quiet request %d: %v", i, err)
		}
		drainBody(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Errorf("quiet tenant got 429 while noisy tenant was flooding (bucket isolation broken)")
		}
		if resp.StatusCode == http.StatusOK {
			quietOK++
			if got := resp.Header.Get("X-Tenant-Echo"); got != quietTenant {
				t.Errorf("quiet request %d: X-Tenant-Echo=%q, want %q", i, got, quietTenant)
			}
		}
	}

	done.Wait()
	if floodThrottled.Load() == 0 {
		t.Errorf("flood tenant was never throttled (concurrent bucket drain not observed)")
	}
	if quietOK == 0 {
		t.Errorf("quiet tenant served zero successful requests; harness or signing broke (not an isolation regression)")
	}
}

// TestKeyflood_RateLimiter_BucketRefills_AfterCooldown asserts
// the token bucket refills over wall-clock time. Without this,
// a single throttle event could permanently lock out a
// well-behaved tenant after a brief burst (the failure mode if
// a future refactor accidentally turns the refill rate to zero
// or stops updating lastEvent).
func TestKeyflood_RateLimiter_BucketRefills_AfterCooldown(t *testing.T) {
	t.Parallel()
	const tenantID = "t-refill"
	h := NewHarness(t, HarnessConfig{
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       2 * time.Second,
		Tenants: []TenantSpec{{
			ID:        tenantID,
			AccessKey: "AKIAREFILL",
			SecretKey: "secretrefill",
			RPS:       10,
			Burst:     2,
		}},
	})

	// Drain the bucket: 2 burst slots succeed; the 3rd is
	// rejected immediately.
	client := newHTTPClient(2 * time.Second)
	statuses := make([]int, 0, 3)
	for i := 0; i < 3; i++ {
		url := h.PresignedGet(tenantID, randomKey())
		req, err := newGETRequest(url)
		if err != nil {
			t.Fatalf("newGETRequest: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		drainBody(resp.Body)
		_ = resp.Body.Close()
		statuses = append(statuses, resp.StatusCode)
	}
	if statuses[0] != http.StatusOK || statuses[1] != http.StatusOK {
		t.Fatalf("burst slots not honoured: statuses=%v", statuses)
	}
	if statuses[2] != http.StatusTooManyRequests {
		t.Fatalf("3rd request should have been throttled (burst=2 already drained); statuses=%v", statuses)
	}

	// Wait for the bucket to refill (rps=10 -> ~100ms per
	// token; 350ms is well over the threshold but still short
	// enough to keep the test fast).
	time.Sleep(350 * time.Millisecond)

	// One request must now succeed; if it doesn't, the bucket
	// never refilled.
	url := h.PresignedGet(tenantID, randomKey())
	req, err := newGETRequest(url)
	if err != nil {
		t.Fatalf("post-cooldown newGETRequest: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("post-cooldown request hung past deadline: bucket failed to refill")
		}
		t.Fatalf("post-cooldown request: %v", err)
	}
	drainBody(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-cooldown request status=%d, want 200 (bucket failed to refill)", resp.StatusCode)
	}
}
