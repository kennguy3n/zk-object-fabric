package auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// fakeSink is an AlertSink that records every emission for later
// inspection. It is concurrency-safe so test helpers can call it
// from inside the limiter's lock-release path.
type fakeSink struct {
	mu     sync.Mutex
	events []billing.UsageEvent
}

func (s *fakeSink) Emit(e billing.UsageEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *fakeSink) drain() []billing.UsageEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]billing.UsageEvent, len(s.events))
	copy(out, s.events)
	s.events = s.events[:0]
	return out
}

func (s *fakeSink) dimensionCount(dim billing.Dimension) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.Dimension == dim {
			n++
		}
	}
	return n
}

// fixedLookup returns a RateLimitLookup that always returns the same
// (rps, burst).
func fixedLookup(rps, burst int) RateLimitLookup {
	return func(string) (int, int, bool) { return rps, burst, true }
}

func TestRateLimiter_AllowTokenBucket(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	l := NewRateLimiter(fixedLookup(2, 2), func(*http.Request) (string, bool) { return "t1", true })
	l.Clock = clock

	if !l.Allow("t1") {
		t.Fatal("first request should consume a token")
	}
	if !l.Allow("t1") {
		t.Fatal("second request should consume the remaining token")
	}
	if l.Allow("t1") {
		t.Fatal("third request should be throttled, bucket empty")
	}
	now = now.Add(time.Second)
	if !l.Allow("t1") {
		t.Fatal("token should be refilled after 1 second")
	}
}

func TestRateLimiter_BudgetExhaustionRejects(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	clock := func() time.Time { return now }
	sink := &fakeSink{}

	const budget = int64(1024)
	l := NewRateLimiter(
		fixedLookup(100, 100),
		func(*http.Request) (string, bool) { return "t1", true },
	)
	l.Clock = clock
	l.AlertSink = sink
	l.EgressLookup = func(string) (int64, bool) { return budget, true }

	// First response: within budget.
	l.Observe("t1", 512)
	if !l.AllowEgress("t1") {
		t.Fatal("tenant should still have budget after first observation")
	}
	// Second response: cross the budget threshold.
	l.Observe("t1", 512)
	if l.AllowEgress("t1") {
		t.Fatal("tenant should be rejected when budget is exhausted")
	}
	if got := sink.dimensionCount(billing.AbuseBudgetExhausted); got != 1 {
		t.Fatalf("expected 1 AbuseBudgetExhausted alert, got %d", got)
	}
	// Each subsequent Rejected call should also alert.
	if l.AllowEgress("t1") {
		t.Fatal("rejection must persist once exhausted")
	}
	if got := sink.dimensionCount(billing.AbuseBudgetExhausted); got != 2 {
		t.Fatalf("expected 2 AbuseBudgetExhausted alerts after second check, got %d", got)
	}
}

func TestRateLimiter_BudgetResetsOnMonthRollover(t *testing.T) {
	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l := NewRateLimiter(
		fixedLookup(100, 100),
		func(*http.Request) (string, bool) { return "t1", true },
	)
	l.Clock = clock
	l.EgressLookup = func(string) (int64, bool) { return 1024, true }

	l.Observe("t1", 1024)
	if l.AllowEgress("t1") {
		t.Fatal("tenant should be exhausted at end of March")
	}
	now = time.Date(2026, 4, 1, 0, 0, 1, 0, time.UTC)
	if !l.AllowEgress("t1") {
		t.Fatal("tenant budget should reset at the start of April")
	}
	snap, ok := l.Snapshot("t1")
	if !ok {
		t.Fatal("expected snapshot after reset")
	}
	if snap.EgressBytesThisMonth != 0 {
		t.Fatalf("expected EgressBytesThisMonth=0 after rollover, got %d", snap.EgressBytesThisMonth)
	}
}

func TestRateLimiter_AnomalyDetectionFiresAlert(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	clock := func() time.Time { return now }
	sink := &fakeSink{}

	l := NewRateLimiter(
		fixedLookup(10_000, 10_000),
		func(*http.Request) (string, bool) { return "t1", true },
	)
	l.Clock = clock
	l.AlertSink = sink
	l.AnomalyMultiplier = 5.0
	l.AnomalyWindow = time.Minute
	l.BaselineAlpha = 1.0 // One-for-one baseline so the test is deterministic.
	l.MinBaselineReqs = 1.0
	l.MinBaselineEgressBytes = 1024

	// Baseline: 60 requests over 60 seconds ≈ 1 req/s. The window
	// is closed when time advances past AnomalyWindow, so the
	// baseline folds in via rollWindow.
	for i := 0; i < 60; i++ {
		l.Observe("t1", 2048)
		now = now.Add(time.Second)
	}
	// Advance past the window so the next Observe rolls the
	// baseline in.
	now = now.Add(2 * time.Second)
	l.Observe("t1", 2048)

	snap, _ := l.Snapshot("t1")
	if snap.BaselineReqs < 0.5 || snap.BaselineReqs > 2 {
		t.Fatalf("expected baseline ~1 req/s, got %.3f", snap.BaselineReqs)
	}

	// Spike: 600 requests spread over 6 seconds — that's 100 req/s
	// vs baseline ~1 req/s, well above the 5x multiplier. We have
	// to advance the clock so the detector sees non-zero elapsed
	// time in the current window.
	baseSink := sink.dimensionCount(billing.AbuseAnomalyAlert)
	for i := 0; i < 600; i++ {
		l.Observe("t1", 2048)
		now = now.Add(10 * time.Millisecond)
	}
	if got := sink.dimensionCount(billing.AbuseAnomalyAlert); got <= baseSink {
		t.Fatalf("expected anomaly alert after spike, got %d (baseline %d)", got, baseSink)
	}
}

func TestRateLimiter_AnomalyCooldownDebounce(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	clock := func() time.Time { return now }
	sink := &fakeSink{}

	l := NewRateLimiter(
		fixedLookup(100_000, 100_000),
		func(*http.Request) (string, bool) { return "t1", true },
	)
	l.Clock = clock
	l.AlertSink = sink
	l.AnomalyMultiplier = 10.0
	l.AnomalyWindow = time.Minute
	l.AnomalyCooldown = 5 * time.Minute
	l.BaselineAlpha = 1.0

	// Establish a baseline of ~1 req/s over a full window. That
	// stays well under the 10x multiplier so the warmup itself
	// does not alert and consume the cooldown.
	for i := 0; i < 60; i++ {
		l.Observe("t1", 1024)
		now = now.Add(time.Second)
	}
	now = now.Add(2 * time.Second)
	l.Observe("t1", 1024) // rolls the window and folds the baseline in

	// Spike: 500 observations over 5 seconds ~= 100 req/s against
	// baseline ~1 req/s. The first observation in the spike trips
	// the detector; every subsequent observation is debounced by
	// AnomalyCooldown so we expect exactly one emitted alert.
	baseline := sink.dimensionCount(billing.AbuseAnomalyAlert)
	for i := 0; i < 500; i++ {
		l.Observe("t1", 1024)
		now = now.Add(10 * time.Millisecond)
	}
	alerts := sink.dimensionCount(billing.AbuseAnomalyAlert) - baseline
	if alerts != 1 {
		t.Fatalf("expected exactly 1 alert inside cooldown, got %d", alerts)
	}
}

func TestRateLimiter_ThrottleOnAnomalyGates429(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	clock := func() time.Time { return now }
	sink := &fakeSink{}

	l := NewRateLimiter(
		fixedLookup(100_000, 100_000),
		func(*http.Request) (string, bool) { return "t1", true },
	)
	l.Clock = clock
	l.AlertSink = sink
	l.AnomalyMultiplier = 2.0
	l.AnomalyWindow = time.Minute
	l.AnomalyCooldown = 5 * time.Minute
	l.BaselineAlpha = 1.0
	l.ThrottleOnAnomaly = true

	// Warm up a small baseline.
	for i := 0; i < 30; i++ {
		l.Observe("t1", 1024)
		now = now.Add(time.Second)
	}
	now = now.Add(2 * time.Second)
	l.Observe("t1", 1024)

	// Trigger a spike to arm the throttle.
	for i := 0; i < 300; i++ {
		l.Observe("t1", 1024)
	}
	if !l.throttling("t1") {
		t.Fatal("expected throttle to be active after spike")
	}

	// The middleware should now return 429 even though the token
	// bucket has plenty of tokens.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should-not-reach"))
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	l.Middleware(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("middleware status = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "anomaly throttle") {
		t.Fatalf("middleware body = %q, want anomaly message", rec.Body.String())
	}

	// After the cooldown expires the throttle should clear.
	now = now.Add(10 * time.Minute)
	if l.throttling("t1") {
		t.Fatal("throttle should clear after cooldown")
	}
}

func TestRateLimiter_MiddlewareCountsEgress(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	clock := func() time.Time { return now }
	sink := &fakeSink{}

	l := NewRateLimiter(
		fixedLookup(1000, 1000),
		func(*http.Request) (string, bool) { return "t1", true },
	)
	l.Clock = clock
	l.AlertSink = sink

	body := strings.Repeat("A", 2048)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	l.Middleware(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", rec.Code)
	}
	snap, ok := l.Snapshot("t1")
	if !ok {
		t.Fatal("expected snapshot after observation")
	}
	if snap.EgressBytesThisMonth != int64(len(body)) {
		t.Fatalf("EgressBytesThisMonth = %d, want %d", snap.EgressBytesThisMonth, len(body))
	}
	if snap.WindowReqs != 1 {
		t.Fatalf("WindowReqs = %d, want 1", snap.WindowReqs)
	}
}

func TestTenantEgressBudgetLookup_ConvertsTBToBytes(t *testing.T) {
	store := NewMemoryTenantStore()
	err := store.AddBinding(TenantBinding{
		AccessKey: "AK",
		SecretKey: "SK",
		Tenant: tenant.Tenant{
			ID:           "tenant-1",
			Name:         "t",
			ContractType: tenant.ContractB2CPooled,
			LicenseTier:  tenant.LicenseStandard,
			Keys:         tenant.Keys{RootKeyRef: "cmk://t", DEKPolicy: tenant.DEKPerObject},
			PlacementDefault: tenant.PlacementDefault{PolicyRef: "p"},
			Budgets: tenant.Budgets{
				RequestsPerSec: 50,
				EgressTBMonth:  2.0,
			},
			Billing: tenant.Billing{Currency: "USD"},
		},
	})
	if err != nil {
		t.Fatalf("AddBinding: %v", err)
	}
	lookup := TenantEgressBudgetLookup(store)
	bytes, ok := lookup("tenant-1")
	if !ok {
		t.Fatal("lookup returned ok=false for known tenant")
	}
	const wantBytes = int64(2) << 40 // 2 TiB
	if bytes != wantBytes {
		t.Fatalf("bytes = %d, want %d", bytes, wantBytes)
	}
	if _, ok := lookup("nope"); ok {
		t.Fatal("expected lookup for unknown tenant to return ok=false")
	}
}

func TestTenantBudgetsLookup_ZeroRPSSkipsLimiter(t *testing.T) {
	store := NewMemoryTenantStore()
	err := store.AddBinding(TenantBinding{
		AccessKey: "AK",
		SecretKey: "SK",
		Tenant: tenant.Tenant{
			ID:           "tenant-1",
			Name:         "t",
			ContractType: tenant.ContractB2CPooled,
			LicenseTier:  tenant.LicenseStandard,
			Keys:         tenant.Keys{RootKeyRef: "cmk://t", DEKPolicy: tenant.DEKPerObject},
			PlacementDefault: tenant.PlacementDefault{PolicyRef: "p"},
			Budgets:      tenant.Budgets{RequestsPerSec: 0},
			Billing:      tenant.Billing{Currency: "USD"},
		},
	})
	if err != nil {
		t.Fatalf("AddBinding: %v", err)
	}
	lookup := TenantBudgetsLookup(store)
	if _, _, ok := lookup("tenant-1"); ok {
		t.Fatal("expected ok=false when RequestsPerSec=0 so the limiter is a no-op")
	}
}
