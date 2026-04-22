package auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// baseTenant returns a tenant.Tenant pre-populated with the minimum
// fields the abuse guard inspects, so tests can set one knob at a
// time without re-authoring the full schema.
func baseTenant(id string) tenant.Tenant {
	return tenant.Tenant{
		ID:               id,
		Name:             id,
		ContractType:     tenant.ContractB2CPooled,
		LicenseTier:      tenant.LicenseStandard,
		Keys:             tenant.Keys{RootKeyRef: "cmk://" + id, DEKPolicy: tenant.DEKPerObject},
		PlacementDefault: tenant.PlacementDefault{PolicyRef: "p"},
		Billing:          tenant.Billing{Currency: "USD"},
	}
}

// staticTenant returns a TenantLookup that always resolves tenantID
// to t.
func staticTenant(t tenant.Tenant) TenantLookup {
	return func(id string) (tenant.Tenant, bool) {
		if id != t.ID {
			return tenant.Tenant{}, false
		}
		return t, true
	}
}

// alwaysTenant returns a TenantResolver that binds every request to
// tenantID.
func alwaysTenant(tenantID string) TenantResolver {
	return func(*http.Request) (string, bool) { return tenantID, true }
}

func TestAbuseGuard_CDNShieldingRejectsWithoutHeader(t *testing.T) {
	tn := baseTenant("t1")
	tn.Abuse.CDNShielding = CDNShieldingEnabled
	g := NewAbuseGuard(staticTenant(tn), alwaysTenant("t1"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not run when CDN gate rejects")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("X-Abuse-Reject"); got != "cdn-shielding-required" {
		t.Fatalf("X-Abuse-Reject = %q, want cdn-shielding-required", got)
	}
}

func TestAbuseGuard_CDNShieldingAllowsWithHeader(t *testing.T) {
	tn := baseTenant("t1")
	tn.Abuse.CDNShielding = CDNShieldingEnabled
	g := NewAbuseGuard(staticTenant(tn), alwaysTenant("t1"))

	for _, header := range DefaultCDNHeaders {
		t.Run(header, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
			req.Header.Set(header, "203.0.113.1")
			var reached bool
			g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(rec, req)
			if !reached {
				t.Fatalf("inner handler did not run with header %q (status=%d)", header, rec.Code)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status with header %q = %d, want 200", header, rec.Code)
			}
		})
	}
}

func TestAbuseGuard_CDNShieldingDisabledAllows(t *testing.T) {
	tn := baseTenant("t1")
	// CDNShielding left empty — gate is disabled.
	g := NewAbuseGuard(staticTenant(tn), alwaysTenant("t1"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	var reached bool
	g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if !reached {
		t.Fatalf("inner handler did not run with shielding disabled (status=%d)", rec.Code)
	}
}

func TestAbuseGuard_CDNHeadersOverride(t *testing.T) {
	tn := baseTenant("t1")
	tn.Abuse.CDNShielding = CDNShieldingEnabled
	g := NewAbuseGuard(staticTenant(tn), alwaysTenant("t1"))
	g.CDNHeaders = []string{"X-Custom-CDN"}

	// Default header should no longer satisfy the gate when the
	// override is set.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	req.Header.Set("Cf-Connecting-Ip", "203.0.113.1")
	g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not run: override excludes Cf-Connecting-Ip")
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 with override", rec.Code)
	}

	// The custom header should open the gate.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	req.Header.Set("X-Custom-CDN", "yes")
	var reached bool
	g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if !reached {
		t.Fatalf("inner handler did not run with custom CDN header (status=%d)", rec.Code)
	}
}

func TestAbuseGuard_BudgetExhaustionRejects(t *testing.T) {
	// Budget of 1 TB, so the 1 KiB floor never crosses until we
	// pre-seed the counter.
	tn := baseTenant("t1")
	tn.Budgets.EgressTBMonth = 1.0 / float64(int64(1)<<40) * 2048 // exactly 2048 bytes budget
	sink := &fakeSink{}

	now := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	g := NewAbuseGuard(staticTenant(tn), alwaysTenant("t1"))
	g.Clock = func() time.Time { return now }
	g.AlertSink = sink

	// Observe just under the budget.
	g.Observe("t1", tn, 1024)
	if !g.allowBudget("t1", tn) {
		t.Fatal("tenant should still have budget after first observation")
	}
	// Cross the threshold.
	g.Observe("t1", tn, 1024)
	if g.allowBudget("t1", tn) {
		t.Fatal("tenant should be rejected once budget is exhausted")
	}
	if got := sink.dimensionCount(billing.AbuseBudgetExhausted); got != 1 {
		t.Fatalf("expected 1 AbuseBudgetExhausted alert, got %d", got)
	}
	// Each subsequent check should also alert so operators can see
	// sustained abuse.
	if g.allowBudget("t1", tn) {
		t.Fatal("rejection must persist once exhausted")
	}
	if got := sink.dimensionCount(billing.AbuseBudgetExhausted); got != 2 {
		t.Fatalf("expected 2 AbuseBudgetExhausted alerts, got %d", got)
	}
}

func TestAbuseGuard_BudgetResetsOnMonthRollover(t *testing.T) {
	tn := baseTenant("t1")
	tn.Budgets.EgressTBMonth = 1.0 / float64(int64(1)<<40) * 2048
	now := time.Date(2026, 3, 31, 23, 30, 0, 0, time.UTC)
	g := NewAbuseGuard(staticTenant(tn), alwaysTenant("t1"))
	g.Clock = func() time.Time { return now }

	g.Observe("t1", tn, 2048)
	if g.allowBudget("t1", tn) {
		t.Fatal("tenant should be exhausted at end of March")
	}
	now = time.Date(2026, 4, 1, 0, 0, 1, 0, time.UTC)
	if !g.allowBudget("t1", tn) {
		t.Fatal("tenant budget should reset at the start of April")
	}
	snap, ok := g.Snapshot("t1")
	if !ok {
		t.Fatal("expected snapshot after reset")
	}
	if snap.EgressBytesThisMonth != 0 {
		t.Fatalf("EgressBytesThisMonth = %d, want 0 after rollover", snap.EgressBytesThisMonth)
	}
}

func TestAbuseGuard_UnconfiguredBudgetSkipsEnforcement(t *testing.T) {
	tn := baseTenant("t1") // EgressTBMonth = 0
	g := NewAbuseGuard(staticTenant(tn), alwaysTenant("t1"))

	// Observe vastly more bytes than any budget; AllowBudget should
	// stay true because no budget is configured.
	g.Observe("t1", tn, 1<<30)
	if !g.allowBudget("t1", tn) {
		t.Fatal("tenants without a configured budget must not be enforced")
	}
}

func TestAbuseGuard_AnomalyDetection2xRolling(t *testing.T) {
	tn := baseTenant("t1")
	sink := &fakeSink{}

	now := time.Unix(0, 0).UTC()
	g := NewAbuseGuard(staticTenant(tn), alwaysTenant("t1"))
	g.Clock = func() time.Time { return now }
	g.AlertSink = sink
	g.AnomalyMultiplier = 2.0
	g.AnomalyWindow = time.Minute
	g.BaselineAlpha = 1.0 // one-for-one fold so the baseline is deterministic
	g.MinBaselineEgressBytes = 1

	// Establish a baseline of ~1024 B/s over a full window.
	for i := 0; i < 60; i++ {
		g.Observe("t1", tn, 1024)
		now = now.Add(time.Second)
	}
	// Advance past the window so the next Observe rolls the
	// window into the baseline.
	now = now.Add(2 * time.Second)
	g.Observe("t1", tn, 1024)

	snap, _ := g.Snapshot("t1")
	if snap.Baseline < 900 || snap.Baseline > 1200 {
		t.Fatalf("expected baseline ~1024 B/s, got %.1f", snap.Baseline)
	}

	// Spike: ~3x baseline for 5 seconds should trip the 2x gate.
	baseAlerts := sink.dimensionCount(billing.AbuseAnomalyAlert)
	for i := 0; i < 500; i++ {
		g.Observe("t1", tn, 3200)
		now = now.Add(10 * time.Millisecond)
	}
	if got := sink.dimensionCount(billing.AbuseAnomalyAlert); got <= baseAlerts {
		t.Fatalf("expected anomaly alert after 3x spike, got %d (baseline %d)", got, baseAlerts)
	}
}

func TestAbuseGuard_ThrottleOnAnomalyGates429(t *testing.T) {
	tn := baseTenant("t1")
	sink := &fakeSink{}

	now := time.Unix(0, 0).UTC()
	g := NewAbuseGuard(staticTenant(tn), alwaysTenant("t1"))
	g.Clock = func() time.Time { return now }
	g.AlertSink = sink
	g.AnomalyMultiplier = 2.0
	g.AnomalyWindow = time.Minute
	g.AnomalyCooldown = 5 * time.Minute
	g.BaselineAlpha = 1.0
	g.MinBaselineEgressBytes = 1
	g.ThrottleOnAnomaly = true

	// Warm up a modest baseline.
	for i := 0; i < 30; i++ {
		g.Observe("t1", tn, 1024)
		now = now.Add(time.Second)
	}
	now = now.Add(2 * time.Second)
	g.Observe("t1", tn, 1024)

	// Trigger the spike to arm the throttle.
	for i := 0; i < 300; i++ {
		g.Observe("t1", tn, 4096)
	}
	if !g.throttling("t1") {
		t.Fatal("expected throttle to be active after spike")
	}

	// Middleware should return 429 while the throttle is armed.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not run while throttle is active")
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "anomaly throttle") {
		t.Fatalf("body = %q, want anomaly message", rec.Body.String())
	}

	// After the cooldown expires the throttle clears.
	now = now.Add(10 * time.Minute)
	if g.throttling("t1") {
		t.Fatal("throttle should clear after cooldown")
	}
}

func TestAbuseGuard_UnknownTenantPassesThrough(t *testing.T) {
	// Resolver returns an ID, but Tenants does not know it.
	g := NewAbuseGuard(
		func(string) (tenant.Tenant, bool) { return tenant.Tenant{}, false },
		alwaysTenant("t-unknown"),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	var reached bool
	g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if !reached {
		t.Fatal("unknown tenants must pass through so misconfig cannot lock users out")
	}
}

func TestAbuseGuard_MiddlewareCountsEgress(t *testing.T) {
	tn := baseTenant("t1")
	g := NewAbuseGuard(staticTenant(tn), alwaysTenant("t1"))
	now := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	g.Clock = func() time.Time { return now }

	body := strings.Repeat("A", 2048)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	g.Middleware(next).ServeHTTP(rec, req)

	snap, ok := g.Snapshot("t1")
	if !ok {
		t.Fatal("expected snapshot after observation")
	}
	if snap.EgressBytesThisMonth != int64(len(body)) {
		t.Fatalf("EgressBytesThisMonth = %d, want %d", snap.EgressBytesThisMonth, len(body))
	}
	if snap.WindowBytes != int64(len(body)) {
		t.Fatalf("WindowBytes = %d, want %d", snap.WindowBytes, len(body))
	}
}

func TestTenantLookupFromStore(t *testing.T) {
	store := NewMemoryTenantStore()
	tn := baseTenant("tenant-1")
	tn.Budgets.EgressTBMonth = 2.0
	tn.Abuse.CDNShielding = CDNShieldingEnabled
	if err := store.AddBinding(TenantBinding{AccessKey: "AK", SecretKey: "SK", Tenant: tn}); err != nil {
		t.Fatalf("AddBinding: %v", err)
	}
	lookup := TenantLookupFromStore(store)
	got, ok := lookup("tenant-1")
	if !ok {
		t.Fatal("lookup returned ok=false for known tenant")
	}
	if got.Abuse.CDNShielding != CDNShieldingEnabled {
		t.Fatalf("CDNShielding = %q, want %q", got.Abuse.CDNShielding, CDNShieldingEnabled)
	}
	if got.Budgets.EgressTBMonth != 2.0 {
		t.Fatalf("EgressTBMonth = %v, want 2.0", got.Budgets.EgressTBMonth)
	}
	if _, ok := lookup("nope"); ok {
		t.Fatal("expected lookup for unknown tenant to return ok=false")
	}
}
