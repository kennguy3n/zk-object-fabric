// Package auth abuse.go implements the per-tenant abuse guard that
// sits alongside the request-rate limiter in rate_limit.go.
//
// Where the rate limiter owns token-bucket request rate enforcement
// plus the per-tenant monthly egress ceiling and the general anomaly
// signal, the abuse guard focuses on abuse-shaped enforcement that
// reads directly off the tenant record:
//
//  1. Egress bandwidth budget — the guard reads
//     tenant.Budgets.EgressTBMonth, converts it to bytes, and rejects
//     tenants that have exhausted their monthly quota with HTTP 429.
//     The counter resets at the top of each calendar month (UTC),
//     which matches the billing pipeline's invoice window.
//
//  2. Egress-rate anomaly detection — a sliding-window EWMA baseline
//     flags tenants whose current egress rate exceeds
//     AnomalyMultiplier (default 2x) of the rolling average. Detected
//     anomalies emit a billing.AbuseAnomalyAlert event on the
//     configured AlertSink; when ThrottleOnAnomaly is true the guard
//     additionally returns HTTP 429 for the cooldown window.
//
//  3. CDN shielding gate — when a tenant's
//     AbuseConfig.CDNShielding == "enabled", the guard requires at
//     least one of the configured CDN-identifying headers to be
//     present on the request. Requests that arrive directly (without
//     a CDN hop) are rejected with HTTP 403. Tenants with shielding
//     disabled pass through untouched.
//
// The guard is wired into the gateway middleware chain in
// cmd/gateway/main.go in front of the S3-compatible handler so these
// controls apply before the request touches the manifest store or
// backend providers. The existing RateLimiter in rate_limit.go
// remains in the chain and retains its own budget counter; the two
// counters are intentionally independent so the guards can be
// toggled on and off separately during Phase 3 rollout.
package auth

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// TenantLookup resolves a tenant ID to its tenant record. The abuse
// guard uses it to read Budgets.EgressTBMonth and Abuse.CDNShielding
// off the canonical tenant schema without depending on the
// Authenticator's key-level view.
type TenantLookup func(tenantID string) (tenant.Tenant, bool)

// CDNShieldingEnabled is the AbuseConfig.CDNShielding value that
// activates the CDN gate. Declared as a constant so operator typos
// surface as tenant-config failures rather than silently disabling
// the guard.
const CDNShieldingEnabled = "enabled"

// DefaultCDNHeaders is the header set the guard accepts as proof
// that a request traversed a CDN. The list is limited to headers
// that CDN edges synthesize from their own signed state — Cloudflare
// (Cf-Connecting-Ip / Cf-Ray), AWS CloudFront (X-Amz-Cf-Id), and
// Fastly (Fastly-Client-Ip) — so a direct client cannot trivially
// forge them to bypass the shielding gate.
//
// X-Forwarded-For is deliberately excluded: any HTTP client can
// attach the header, so accepting it as CDN proof would make the
// gate bypassable. Operators fronted by a reverse proxy that adds
// X-Forwarded-For should override CDNHeaders with the specific
// header their edge attaches (typically a signed or shared-secret
// header) rather than the generic hop indicator.
var DefaultCDNHeaders = []string{
	"Cf-Connecting-Ip",
	"Cf-Ray",
	"X-Amz-Cf-Id",
	"Fastly-Client-Ip",
}

// AbuseGuard is the edge abuse guard. Construct with NewAbuseGuard
// and configure the anomaly knobs, CDN header list, and alert sink
// before installing the middleware.
type AbuseGuard struct {
	// Tenants resolves tenant records by ID. Required.
	Tenants TenantLookup

	// Resolver identifies the tenant for a request (typically
	// shared with the rate limiter so both guards see the same
	// tenant view).
	Resolver TenantResolver

	// Clock returns the current time. Tests override it to drive
	// the budget and anomaly counters deterministically.
	Clock func() time.Time

	// AlertSink receives AbuseBudgetExhausted and AbuseAnomalyAlert
	// events. Nil disables alerting (the guard still enforces).
	AlertSink AlertSink

	// AnomalyMultiplier is the ratio of current egress rate to
	// baseline that triggers an anomaly alert. Defaults to 2.0.
	AnomalyMultiplier float64

	// AnomalyWindow is the sliding window over which the current
	// egress rate is measured. Defaults to 1 minute.
	AnomalyWindow time.Duration

	// AnomalyCooldown debounces repeated alerts and, when
	// ThrottleOnAnomaly is true, is also the duration for which
	// follow-up requests are returned as HTTP 429. Defaults to
	// AnomalyWindow.
	AnomalyCooldown time.Duration

	// BaselineAlpha is the EWMA weight applied to each completed
	// window when folding it into the baseline. Must be in (0, 1];
	// defaults to 0.3.
	BaselineAlpha float64

	// MinBaselineEgressBytes is the floor applied when comparing
	// current rate to baseline, so a cold-start tenant's first
	// burst does not trivially exceed a near-zero baseline.
	// Defaults to 1 KiB/s.
	MinBaselineEgressBytes float64

	// ThrottleOnAnomaly returns HTTP 429 for follow-up requests
	// inside the cooldown window after an anomaly fires. When
	// false the guard only alerts and does not reject traffic.
	ThrottleOnAnomaly bool

	// CDNHeaders overrides DefaultCDNHeaders. Leave nil to use the
	// default set.
	CDNHeaders []string

	mu      sync.Mutex
	egress  map[string]*abuseEgressTracker
	anomaly map[string]*abuseAnomalyTracker
}

// abuseEgressTracker is the monthly running total for one tenant.
// The counter resets at the start of each calendar month UTC.
type abuseEgressTracker struct {
	monthStart time.Time
	bytes      int64
}

// abuseAnomalyTracker holds the sliding-window byte counter plus the
// EWMA baseline for one tenant. Folding the window into the baseline
// happens in rollWindow.
type abuseAnomalyTracker struct {
	windowStart   time.Time
	windowBytes   int64
	baseline      float64
	lastAlertAt   time.Time
	throttleUntil time.Time
}

// NewAbuseGuard returns a guard wired to tenants and resolver. The
// zero-value anomaly knobs are filled in from sensible defaults on
// first use so a bare NewAbuseGuard is usable in tests.
func NewAbuseGuard(tenants TenantLookup, resolver TenantResolver) *AbuseGuard {
	return &AbuseGuard{
		Tenants:  tenants,
		Resolver: resolver,
		Clock:    time.Now,
		egress:   map[string]*abuseEgressTracker{},
		anomaly:  map[string]*abuseAnomalyTracker{},
	}
}

// Middleware returns an http.Handler that runs every request through
// the guard. Unknown tenants (no Resolver match, or no record in
// Tenants) pass through without enforcement so misconfiguration
// cannot lock users out; the downstream authenticator is responsible
// for rejecting them if needed.
//
// The response is wrapped in a counting writer so the guard can
// measure egress without cooperation from the inner handler.
func (g *AbuseGuard) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID, ok := g.Resolver(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		t, known := g.Tenants(tenantID)
		if !known {
			next.ServeHTTP(w, r)
			return
		}
		if !g.allowCDN(t, r) {
			w.Header().Set("X-Abuse-Reject", "cdn-shielding-required")
			http.Error(w, "cdn shielding required", http.StatusForbidden)
			return
		}
		// The budget check is deliberately soft: we reject further
		// traffic only once the running monthly counter already
		// meets or exceeds tenant.Budgets.EgressTBMonth. The
		// current request itself is allowed through and its bytes
		// are added to the counter after Observe below, so a
		// single oversized request can overshoot the budget on
		// the window it crosses the threshold. Enforcing a hard
		// pre-request cap would require buffering the backend
		// response body and making a double-HEAD call to size it
		// — an explicit non-goal for Phase 3. The billing pipeline
		// backfills the exhaustion event when it lands, and every
		// subsequent request is rejected with 429.
		if !g.allowBudget(tenantID, t) {
			w.Header().Set("Retry-After", "3600")
			http.Error(w, "monthly egress budget exhausted", http.StatusTooManyRequests)
			return
		}
		if g.ThrottleOnAnomaly && g.throttling(tenantID) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "abuse anomaly throttle active", http.StatusTooManyRequests)
			return
		}
		// Reuse an upstream countingWriter if the rate limiter (or
		// any other outer middleware) already wrapped the response.
		// Each guard still calls Observe independently so the
		// per-tenant budget and anomaly counters stay accurate,
		// but we avoid layering N counting writers on every byte
		// write down the chain.
		counter, ok := w.(*countingWriter)
		if !ok {
			counter = &countingWriter{ResponseWriter: w}
		}
		startBytes := counter.bytes
		next.ServeHTTP(counter, r)
		g.Observe(tenantID, t, counter.bytes-startBytes)
	})
}

// allowCDN reports whether the request satisfies the tenant's CDN
// shielding configuration. Tenants whose AbuseConfig.CDNShielding is
// not "enabled" always pass.
func (g *AbuseGuard) allowCDN(t tenant.Tenant, r *http.Request) bool {
	if t.Abuse.CDNShielding != CDNShieldingEnabled {
		return true
	}
	headers := g.CDNHeaders
	if len(headers) == 0 {
		headers = DefaultCDNHeaders
	}
	for _, h := range headers {
		if strings.TrimSpace(r.Header.Get(h)) != "" {
			return true
		}
	}
	return false
}

// allowBudget reports whether tenantID still has monthly egress
// budget left. A zero/negative tenant.Budgets.EgressTBMonth means
// "no configured budget" and the guard passes the request. An
// exhausted budget also emits an AbuseBudgetExhausted alert per
// rejected request so operators can see sustained abuse rather
// than a single edge event.
func (g *AbuseGuard) allowBudget(tenantID string, t tenant.Tenant) bool {
	budget := tenantEgressBudgetBytes(t)
	if budget <= 0 {
		return true
	}
	now := g.now()

	g.mu.Lock()
	tr, ok := g.egress[tenantID]
	if !ok {
		tr = &abuseEgressTracker{monthStart: monthStart(now)}
		g.egress[tenantID] = tr
	}
	if !sameMonth(tr.monthStart, now) {
		tr.monthStart = monthStart(now)
		tr.bytes = 0
	}
	exhausted := tr.bytes >= budget
	g.mu.Unlock()

	if exhausted {
		g.alert(tenantID, billing.AbuseBudgetExhausted, 1)
		return false
	}
	return true
}

// Observe records that tenantID just served egressBytes. The
// observation updates both the monthly budget counter and the
// anomaly-detection window. It is called by Middleware after the
// inner handler finishes, so the counters reflect bytes actually
// written to the wire. External callers may also invoke Observe
// directly when a non-HTTP path (e.g. a presigned redirect) served
// bytes on behalf of the tenant.
func (g *AbuseGuard) Observe(tenantID string, t tenant.Tenant, egressBytes int64) {
	if tenantID == "" || egressBytes <= 0 {
		return
	}
	now := g.now()

	g.mu.Lock()
	tr, ok := g.egress[tenantID]
	if !ok {
		tr = &abuseEgressTracker{monthStart: monthStart(now)}
		g.egress[tenantID] = tr
	}
	if !sameMonth(tr.monthStart, now) {
		tr.monthStart = monthStart(now)
		tr.bytes = 0
	}
	tr.bytes += egressBytes

	a, ok := g.anomaly[tenantID]
	if !ok {
		a = &abuseAnomalyTracker{windowStart: now}
		g.anomaly[tenantID] = a
	}
	window := g.window()
	if now.Sub(a.windowStart) >= window {
		g.rollWindow(a, now, window)
	}
	a.windowBytes += egressBytes
	decision := g.maybeAlert(a, now, window)
	g.mu.Unlock()

	if decision.fire {
		g.alert(tenantID, billing.AbuseAnomalyAlert, decision.ratio)
	}
	_ = t // t is kept on the signature so future policy hooks (e.g.
	// license-tier-specific multipliers) can read tenant state
	// without a second Tenants lookup.
}

// AbuseSnapshot is a read-only projection of the guard's per-tenant
// counters, exposed for tests and operational debug endpoints.
type AbuseSnapshot struct {
	EgressBytesThisMonth int64
	MonthStart           time.Time
	WindowBytes          int64
	Baseline             float64
	LastAlertAt          time.Time
}

// Snapshot returns the guard's counters for tenantID.
func (g *AbuseGuard) Snapshot(tenantID string) (AbuseSnapshot, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	snap := AbuseSnapshot{}
	found := false
	if tr, ok := g.egress[tenantID]; ok {
		snap.EgressBytesThisMonth = tr.bytes
		snap.MonthStart = tr.monthStart
		found = true
	}
	if a, ok := g.anomaly[tenantID]; ok {
		snap.WindowBytes = a.windowBytes
		snap.Baseline = a.baseline
		snap.LastAlertAt = a.lastAlertAt
		found = true
	}
	return snap, found
}

// throttling reports whether tenantID is currently inside an active
// anomaly-throttle cooldown.
func (g *AbuseGuard) throttling(tenantID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.anomaly[tenantID]
	if !ok {
		return false
	}
	return g.now().Before(a.throttleUntil)
}

// rollWindow folds the current window's bytes-per-second into the
// EWMA baseline and resets the counters. Must be called with g.mu
// held.
func (g *AbuseGuard) rollWindow(a *abuseAnomalyTracker, now time.Time, window time.Duration) {
	elapsed := now.Sub(a.windowStart).Seconds()
	if elapsed <= 0 {
		elapsed = window.Seconds()
	}
	bps := float64(a.windowBytes) / elapsed
	alpha := g.BaselineAlpha
	if alpha <= 0 || alpha > 1 {
		alpha = 0.3
	}
	if a.baseline == 0 {
		a.baseline = bps
	} else {
		a.baseline = alpha*bps + (1-alpha)*a.baseline
	}
	a.windowStart = now
	a.windowBytes = 0
}

// maybeAlert evaluates the current window's rate against the
// baseline. Must be called with g.mu held. It returns the firing
// decision so the caller can release the lock before emitting on
// the sink.
func (g *AbuseGuard) maybeAlert(a *abuseAnomalyTracker, now time.Time, window time.Duration) alertDecision {
	multiplier := g.AnomalyMultiplier
	if multiplier <= 0 {
		multiplier = 2.0
	}
	elapsed := now.Sub(a.windowStart).Seconds()
	if elapsed <= 0 {
		return alertDecision{}
	}
	currentBps := float64(a.windowBytes) / elapsed
	minBaseline := g.MinBaselineEgressBytes
	if minBaseline <= 0 {
		minBaseline = 1024.0
	}
	baseline := a.baseline
	if baseline < minBaseline {
		baseline = minBaseline
	}
	ratio := currentBps / baseline
	if ratio < multiplier {
		return alertDecision{}
	}
	cooldown := g.AnomalyCooldown
	if cooldown <= 0 {
		cooldown = window
	}
	if !a.lastAlertAt.IsZero() && now.Sub(a.lastAlertAt) < cooldown {
		return alertDecision{}
	}
	a.lastAlertAt = now
	a.throttleUntil = now.Add(cooldown)
	return alertDecision{fire: true, ratio: uint64(ratio + 0.5)}
}

func (g *AbuseGuard) alert(tenantID string, dim billing.Dimension, delta uint64) {
	if g.AlertSink == nil {
		return
	}
	g.AlertSink.Emit(billing.UsageEvent{
		TenantID:   tenantID,
		Dimension:  dim,
		Delta:      delta,
		ObservedAt: g.now(),
	})
}

func (g *AbuseGuard) now() time.Time {
	if g.Clock == nil {
		return time.Now()
	}
	return g.Clock()
}

func (g *AbuseGuard) window() time.Duration {
	if g.AnomalyWindow <= 0 {
		return time.Minute
	}
	return g.AnomalyWindow
}

// tenantEgressBudgetBytes converts Budgets.EgressTBMonth (a TB/month
// float authored by operators) into bytes-per-month. A non-positive
// value means "no configured budget" and the caller skips
// enforcement.
func tenantEgressBudgetBytes(t tenant.Tenant) int64 {
	if t.Budgets.EgressTBMonth <= 0 {
		return 0
	}
	const bytesPerTB = int64(1) << 40 // 1 TiB
	return int64(t.Budgets.EgressTBMonth * float64(bytesPerTB))
}

// TenantLookupFromStore adapts a *MemoryTenantStore to TenantLookup.
// Used by the gateway's main() to hand the abuse guard the same
// tenant view the authenticator sees.
func TenantLookupFromStore(store TenantStore) TenantLookup {
	return func(tenantID string) (tenant.Tenant, bool) {
		b, ok := store.LookupByTenantID(tenantID)
		if !ok {
			return tenant.Tenant{}, false
		}
		return b.Tenant, true
	}
}
