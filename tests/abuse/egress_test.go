package abuse

import (
	"net/http"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
)

// TestEgress_BudgetExhausted_Trips429AndEmitsAlert is the
// canonical "egress-budget exhaustion" abuse scenario. The
// tenant is configured with a deliberately tiny monthly budget
// (1 byte) so a single successful response — whose body is
// far larger than 1 byte — must exhaust it. Every subsequent
// request then has to be rejected with HTTP 429 AND emit an
// AbuseBudgetExhausted UsageEvent on the configured AlertSink.
//
// The harness wires the production middleware chain end to end
// (limiter -> abuse guard -> SigV4 authenticator echo). A
// regression that drops the AllowEgress gate (or stops emitting
// the alert) would be visible here as either an OK response on
// the second request or a missing dimension on the sink.
//
// The test relies on the conversion in NewHarness:
//
//	budget_bytes := int64(EgressTBMonth * 1e12)
//
// With EgressTBMonth = 1e-12, budget_bytes = 1. The first
// request still passes the gate (initial counter is zero, so
// 0 >= 1 is false) and the body bytes get folded into the
// monthly counter via Observe. Every subsequent request sees
// counter >= 1 and is rejected with the alert emitted.
func TestEgress_BudgetExhausted_Trips429AndEmitsAlert(t *testing.T) {
	t.Parallel()
	const (
		tenantID = "t-egress"
		bodySize = 64 * 1024 // 64 KiB per response, plenty to overshoot a 1-byte budget
		// 1e-12 TB == 1 byte. Pinning the budget to 1 byte makes
		// the exhaustion event happen on the *second* request,
		// independent of CI scheduling jitter.
		egressBudgetTB = 1e-12
	)
	h := NewHarness(t, HarnessConfig{
		BodySize:          bodySize,
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       2 * time.Second,
		Tenants: []TenantSpec{{
			ID:            tenantID,
			AccessKey:     "AKIAEGRESS",
			SecretKey:     "secretegress",
			RPS:           1000,
			Burst:         1000,
			EgressTBMonth: egressBudgetTB,
		}},
	})

	client := newHTTPClient(2 * time.Second)

	// Request 1: passes the gate (counter is 0 < 1 byte budget)
	// and serves bodySize bytes. After the handler returns, the
	// Observe call folds bodySize into the monthly counter.
	first := doRequest(t, client, h.PresignedGet(tenantID, randomKey()))
	if first.code != http.StatusOK {
		t.Fatalf("first request: status=%d want 200 (initial counter is 0)", first.code)
	}
	if first.echo != tenantID {
		t.Fatalf("first request: X-Tenant-Echo=%q want %q", first.echo, tenantID)
	}
	if first.bodyLen() != bodySize {
		t.Fatalf("first request: body=%d bytes want %d (the harness echo handler is the egress counter source)", first.bodyLen(), bodySize)
	}

	// Request 2..N: counter is now >= budget; every subsequent
	// request must be rejected with 429 + Retry-After and emit
	// an AbuseBudgetExhausted event per call.
	const rejected = 5
	for i := 0; i < rejected; i++ {
		got := doRequest(t, client, h.PresignedGet(tenantID, randomKey()))
		if got.code != http.StatusTooManyRequests {
			t.Fatalf("rejection request %d: status=%d want %d (budget should be exhausted)", i, got.code, http.StatusTooManyRequests)
		}
		if got.retryAfter == "" {
			t.Errorf("rejection request %d: empty Retry-After header (operator playbooks rely on this)", i)
		}
		// Once exhausted, the terminal handler must not run.
		// The echo header is set by the handler so its
		// absence proves the limiter short-circuited before
		// the echo handler. The 429 response body is the
		// limiter's own "monthly egress budget exhausted"
		// string (32 bytes), not the configured 64 KiB
		// payload — so assert bodyLen << bodySize rather
		// than bodyLen == 0 (http.Error always writes a
		// short text body).
		if got.echo != "" {
			t.Errorf("rejection request %d: X-Tenant-Echo=%q expected empty (terminal handler ran despite rejection)", i, got.echo)
		}
		if got.bodyLen() >= bodySize {
			t.Errorf("rejection request %d: body=%d bytes >= configured payload %d (terminal handler ran despite rejection)", i, got.bodyLen(), bodySize)
		}
	}

	// One alert per rejected request — the limiter explicitly
	// re-emits on every rejection so operators get a non-flat
	// signal during sustained abuse.
	if got := h.Sink.CountByDimension(billing.AbuseBudgetExhausted); got != rejected {
		t.Fatalf("AbuseBudgetExhausted alerts: got %d, want %d (one per rejected request)", got, rejected)
	}

	// Sanity-check the limiter's exposed snapshot — the
	// counter must be at least bodySize bytes after the first
	// successful request. Catches a regression where Observe
	// stops counting or the counter is reset on every request.
	snap, ok := h.Limiter.Snapshot(tenantID)
	if !ok {
		t.Fatalf("no rate-limiter snapshot for tenant %q after request burst", tenantID)
	}
	if snap.EgressBytesThisMonth < int64(bodySize) {
		t.Errorf("egress counter only %d bytes after %d-byte body request (Observe stopped counting?)", snap.EgressBytesThisMonth, bodySize)
	}
}

// TestEgress_NoBudgetConfigured_AllowsUnlimited covers the
// inverse case: a tenant with EgressTBMonth = 0 must not be
// rate-limited on the egress path at all. This is the
// "fail-open for misconfigured tenants" branch documented on
// RateLimiter.AllowEgress. A regression that defaults this to
// fail-closed would brown out every tenant whose budget hasn't
// been provisioned yet.
func TestEgress_NoBudgetConfigured_AllowsUnlimited(t *testing.T) {
	t.Parallel()
	const (
		tenantID = "t-unbudgeted"
		bodySize = 8 * 1024
	)
	h := NewHarness(t, HarnessConfig{
		BodySize:          bodySize,
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       2 * time.Second,
		Tenants: []TenantSpec{{
			ID:        tenantID,
			AccessKey: "AKIANOBUDGET",
			SecretKey: "secretnobudget",
			RPS:       1000,
			Burst:     1000,
			// EgressTBMonth deliberately omitted (zero).
		}},
	})

	client := newHTTPClient(2 * time.Second)
	const requests = 8
	for i := 0; i < requests; i++ {
		got := doRequest(t, client, h.PresignedGet(tenantID, randomKey()))
		if got.code != http.StatusOK {
			t.Fatalf("request %d: status=%d, want 200 (unbudgeted tenant must fail-open on egress)", i, got.code)
		}
	}
	if got := h.Sink.CountByDimension(billing.AbuseBudgetExhausted); got != 0 {
		t.Errorf("AbuseBudgetExhausted fired %d times for unbudgeted tenant; expected 0", got)
	}
}

// TestEgress_BudgetIsPerTenant proves that exhausting tenant
// A's budget does not affect tenant B. This is the
// counterpart to the keyflood isolation test, but for the
// egress-budget path rather than the request-rate path.
func TestEgress_BudgetIsPerTenant(t *testing.T) {
	t.Parallel()
	const (
		exhaustTenant = "t-exhaust"
		quietTenant   = "t-quiet"
		bodySize      = 64 * 1024
		budgetTB      = 1e-12 // 1 byte
	)
	h := NewHarness(t, HarnessConfig{
		BodySize:          bodySize,
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       2 * time.Second,
		Tenants: []TenantSpec{
			{ID: exhaustTenant, AccessKey: "AKIAEX", SecretKey: "secretex", RPS: 1000, Burst: 1000, EgressTBMonth: budgetTB},
			{ID: quietTenant, AccessKey: "AKIAQT", SecretKey: "secretqt", RPS: 1000, Burst: 1000, EgressTBMonth: 1.0 /* 1 TB */},
		},
	})

	client := newHTTPClient(2 * time.Second)

	// Exhaust tenant A's budget.
	if got := doRequest(t, client, h.PresignedGet(exhaustTenant, randomKey())); got.code != http.StatusOK {
		t.Fatalf("seeding request: status=%d want 200", got.code)
	}
	if got := doRequest(t, client, h.PresignedGet(exhaustTenant, randomKey())); got.code != http.StatusTooManyRequests {
		t.Fatalf("post-exhaust request: status=%d want 429", got.code)
	}

	// Tenant B must be unaffected.
	for i := 0; i < 5; i++ {
		got := doRequest(t, client, h.PresignedGet(quietTenant, randomKey()))
		if got.code != http.StatusOK {
			t.Fatalf("quiet tenant request %d: status=%d want 200 (egress counter is shared across tenants?!)", i, got.code)
		}
		if got.echo != quietTenant {
			t.Errorf("quiet tenant request %d: X-Tenant-Echo=%q want %q", i, got.echo, quietTenant)
		}
	}
}

// requestResult bundles the fields each abuse test asserts on
// so the test bodies stay focused on the abuse-path contract
// rather than HTTP plumbing.
type requestResult struct {
	code       int
	echo       string
	bodyBytes  []byte
	retryAfter string
}

func (r requestResult) bodyLen() int { return len(r.bodyBytes) }

func doRequest(t *testing.T, client *http.Client, url string) requestResult {
	t.Helper()
	req, err := newGETRequest(url)
	if err != nil {
		t.Fatalf("newGETRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 1<<16)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	return requestResult{
		code:       resp.StatusCode,
		echo:       resp.Header.Get("X-Tenant-Echo"),
		bodyBytes:  body,
		retryAfter: resp.Header.Get("Retry-After"),
	}
}
