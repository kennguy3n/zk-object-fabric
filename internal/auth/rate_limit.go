// Package auth rate_limit.go implements the per-tenant rate limiter,
// egress-budget gate, and anomaly-detection alerting used by the
// S3-compatible gateway.
//
// The design draws on two prior-art references:
//
//   - Storj's satellite/accounting package
//     (https://github.com/storj/storj) for per-project bandwidth
//     limits: a monthly "budget" is the authoritative cap, and a
//     project that exhausts it is rejected at the edge rather than
//     downstream at the storage nodes.
//
//   - Ceph RGW's radosgw-admin ratelimit
//     (https://docs.ceph.com/en/latest/radosgw/admin/#rate-limits)
//     for how per-user rate limiting works at the storage layer:
//     RGW enforces a token bucket sized by "max-read-ops" /
//     "max-write-ops" / "max-read-bytes" / "max-write-bytes" on
//     every request. ZK Object Fabric mirrors that shape but moves
//     the enforcement point one hop earlier: the gateway rejects
//     abusive traffic before it reaches Ceph (or Wasabi, or the
//     BYOC backend), so back-end quotas never become the first line
//     of defence.
package auth

import (
	"net/http"
	"sync"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
)

// RateLimitLookup resolves a tenant ID to the token-bucket steady-
// state rate (requests per second) and burst size. The gateway wires
// this to its tenant store so per-tenant budgets drive the limiter.
type RateLimitLookup func(tenantID string) (rps int, burst int, ok bool)

// EgressBudgetLookup resolves a tenant ID to its monthly egress
// budget in bytes. A zero return means the tenant has no configured
// budget and egress enforcement is skipped for them. ok=false means
// the tenant is unknown to the directory; callers treat that as "no
// enforcement" as well so misconfiguration cannot lock users out.
type EgressBudgetLookup func(tenantID string) (bytesPerMonth int64, ok bool)

// TenantResolver is the function the middleware uses to identify the
// tenant for a request. It typically wraps an Authenticator so
// rate-limiting can short-circuit unauthenticated requests before
// they hit the upstream handler.
type TenantResolver func(r *http.Request) (tenantID string, ok bool)

// AlertSink is the subset of billing.BillingSink the rate limiter
// uses to publish anomaly and budget-exhaustion alerts. It is kept
// narrow so tests can supply an in-memory collector without pulling
// in the full billing pipeline.
type AlertSink interface {
	Emit(event billing.UsageEvent)
}

// RateLimiter is the gateway's edge enforcement engine. It combines
// three guardrails:
//
//  1. Token-bucket rate limiting per tenant keyed by
//     tenant.Budgets.RequestsPerSec (the Phase 2 behaviour).
//  2. Monthly egress-budget enforcement keyed by
//     tenant.Budgets.EgressTBMonth: once a tenant has served their
//     quota of bytes for the current calendar month the limiter
//     returns HTTP 429 and emits a billing.AbuseBudgetExhausted
//     event on the configured AlertSink.
//  3. Anomaly detection over a sliding window: when a tenant's
//     request rate or egress rate exceeds AnomalyMultiplier times
//     the historical baseline the limiter emits a
//     billing.AbuseAnomalyAlert event and, if ThrottleOnAnomaly is
//     true, returns HTTP 429 for the duration of the spike.
//
// The zero value is not useful. Construct with NewRateLimiter and
// optionally set AlertSink, AnomalyMultiplier, AnomalyWindow,
// AnomalyCooldown, ThrottleOnAnomaly, and EgressLookup before
// installing the middleware.
type RateLimiter struct {
	Lookup       RateLimitLookup
	EgressLookup EgressBudgetLookup
	Resolver     TenantResolver
	Clock        func() time.Time

	// AlertSink receives anomaly and budget-exhaustion events. Nil
	// disables alerting (the limiter still enforces).
	AlertSink AlertSink

	// AnomalyMultiplier is the ratio of current rate to baseline
	// that triggers an anomaly alert. Defaults to 10.0.
	AnomalyMultiplier float64

	// AnomalyWindow is the sliding window used to measure the
	// current rate. Defaults to 1 minute.
	AnomalyWindow time.Duration

	// AnomalyCooldown debounces repeated alerts for the same
	// tenant. Defaults to AnomalyWindow.
	AnomalyCooldown time.Duration

	// BaselineAlpha is the exponential-moving-average weight
	// applied to each completed window when updating the baseline.
	// Must be in (0, 1]; defaults to 0.3.
	BaselineAlpha float64

	// MinBaselineReqs / MinBaselineEgressBytes are the floor values
	// used when comparing current rate to baseline, so a cold-start
	// tenant's first burst doesn't trivially exceed a near-zero
	// baseline.
	MinBaselineReqs        float64
	MinBaselineEgressBytes float64

	// ThrottleOnAnomaly, when true, causes the middleware to return
	// HTTP 429 for requests that land inside an active anomaly
	// cooldown window. When false the limiter only alerts.
	ThrottleOnAnomaly bool

	mu      sync.Mutex
	buckets map[string]*bucket
	egress  map[string]*egressTracker
	anomaly map[string]*anomalyTracker
}

type bucket struct {
	tokens    float64
	capacity  float64
	refillPS  float64
	lastEvent time.Time
}

// egressTracker holds the running monthly egress-bytes counter for a
// single tenant. The counter resets at the top of each calendar
// month (UTC) so the budget semantics match the billing pipeline's
// invoice period.
type egressTracker struct {
	monthStart time.Time
	bytes      int64
}

// anomalyTracker keeps a sliding-window counter plus an EWMA
// baseline for one tenant. When a window closes its counts are
// folded into the baseline, then compared against the pre-fold
// baseline to decide whether an alert should fire.
type anomalyTracker struct {
	windowStart      time.Time
	windowReqs       int64
	windowEgressByte int64
	baselineReqs     float64
	baselineEgress   float64
	lastAlertAt      time.Time
	throttleUntil    time.Time
}

// NewRateLimiter builds a RateLimiter. Callers typically wire
// EgressLookup, AlertSink, and the anomaly-detection knobs after
// construction.
func NewRateLimiter(lookup RateLimitLookup, resolver TenantResolver) *RateLimiter {
	return &RateLimiter{
		Lookup:   lookup,
		Resolver: resolver,
		Clock:    time.Now,
		buckets:  map[string]*bucket{},
		egress:   map[string]*egressTracker{},
		anomaly:  map[string]*anomalyTracker{},
	}
}

// Middleware returns an http.Handler that rate-limits per tenant,
// gates on egress budget, runs anomaly detection, and otherwise
// delegates to next. Bytes sent downstream are measured by wrapping
// the ResponseWriter so egress tracking does not depend on the
// upstream handler voluntarily reporting.
func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID, ok := l.Resolver(r)
		if !ok {
			// Cannot bill the request to a tenant: let the handler
			// decide (typically 403). Rate limiting only applies
			// when the caller is identified.
			next.ServeHTTP(w, r)
			return
		}
		if !l.Allow(tenantID) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		if !l.AllowEgress(tenantID) {
			w.Header().Set("Retry-After", "3600")
			http.Error(w, "monthly egress budget exhausted", http.StatusTooManyRequests)
			return
		}
		if l.ThrottleOnAnomaly && l.throttling(tenantID) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "anomaly throttle active", http.StatusTooManyRequests)
			return
		}
		// Reuse an upstream countingWriter if an outer middleware
		// (e.g. AbuseGuard) already wrapped the response. Each
		// guard's Observe call uses the delta of bytes written
		// during its own serve window, so re-wrapping would only
		// add a second pass-through layer without changing the
		// measured bytes.
		counter, ok := w.(*countingWriter)
		if !ok {
			counter = &countingWriter{ResponseWriter: w}
		}
		startBytes := counter.bytes
		next.ServeHTTP(counter, r)
		l.Observe(tenantID, counter.bytes-startBytes)
	})
}

// Allow reserves one request for tenantID. It returns false when the
// bucket is empty.
func (l *RateLimiter) Allow(tenantID string) bool {
	rps, burst, ok := l.Lookup(tenantID)
	if !ok || rps <= 0 {
		// Unknown tenant or no budget configured: allow through so
		// the Authenticator can reject the request if needed.
		return true
	}
	if burst <= 0 {
		burst = rps
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[tenantID]
	if !ok {
		b = &bucket{
			tokens:    float64(burst),
			capacity:  float64(burst),
			refillPS:  float64(rps),
			lastEvent: now,
		}
		l.buckets[tenantID] = b
	} else {
		// Refresh capacity in case the tenant's budget changed.
		b.capacity = float64(burst)
		b.refillPS = float64(rps)
	}

	elapsed := now.Sub(b.lastEvent).Seconds()
	if elapsed > 0 {
		b.tokens = minFloat(b.capacity, b.tokens+elapsed*b.refillPS)
	}
	b.lastEvent = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// AllowEgress reports whether tenantID has any monthly egress budget
// left. It returns true when the tenant has no configured budget (so
// misconfigured tenants fail open) and emits an AbuseBudgetExhausted
// alert each time an already-exhausted tenant is rejected.
func (l *RateLimiter) AllowEgress(tenantID string) bool {
	if l.EgressLookup == nil {
		return true
	}
	budget, ok := l.EgressLookup(tenantID)
	if !ok || budget <= 0 {
		return true
	}
	now := l.now()

	l.mu.Lock()
	t, seen := l.egress[tenantID]
	if !seen {
		t = &egressTracker{monthStart: monthStart(now)}
		l.egress[tenantID] = t
	}
	if !sameMonth(t.monthStart, now) {
		t.monthStart = monthStart(now)
		t.bytes = 0
	}
	exhausted := t.bytes >= budget
	l.mu.Unlock()

	if exhausted {
		l.alert(tenantID, billing.AbuseBudgetExhausted, 1)
		return false
	}
	return true
}

// Observe records that tenantID just served egressBytes back to its
// caller. The observation feeds both the monthly egress counter and
// the sliding-window anomaly detector.
func (l *RateLimiter) Observe(tenantID string, egressBytes int64) {
	if tenantID == "" {
		return
	}
	if egressBytes < 0 {
		egressBytes = 0
	}
	now := l.now()

	l.mu.Lock()
	t, ok := l.egress[tenantID]
	if !ok {
		t = &egressTracker{monthStart: monthStart(now)}
		l.egress[tenantID] = t
	}
	if !sameMonth(t.monthStart, now) {
		t.monthStart = monthStart(now)
		t.bytes = 0
	}
	t.bytes += egressBytes

	// Anomaly tracking runs even for tenants without an egress
	// budget: request-rate spikes are actionable regardless of the
	// byte budget.
	a, ok := l.anomaly[tenantID]
	if !ok {
		a = &anomalyTracker{windowStart: now}
		l.anomaly[tenantID] = a
	}
	window := l.anomalyWindow()
	if now.Sub(a.windowStart) >= window {
		l.rollWindow(a, now, window)
	}
	a.windowReqs++
	a.windowEgressByte += egressBytes
	decision := l.maybeAlert(a, now, window)
	l.mu.Unlock()

	if decision.fire {
		l.alert(tenantID, billing.AbuseAnomalyAlert, decision.ratio)
	}
}

// Snapshot returns a point-in-time view of the tenant's counters.
// Exposed for tests and operational debug endpoints; callers must
// not hold a reference to the returned structs across Observe calls.
func (l *RateLimiter) Snapshot(tenantID string) (RateLimiterSnapshot, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	snap := RateLimiterSnapshot{}
	found := false
	if e, ok := l.egress[tenantID]; ok {
		snap.EgressBytesThisMonth = e.bytes
		snap.MonthStart = e.monthStart
		found = true
	}
	if a, ok := l.anomaly[tenantID]; ok {
		snap.WindowReqs = a.windowReqs
		snap.WindowEgressBytes = a.windowEgressByte
		snap.BaselineReqs = a.baselineReqs
		snap.BaselineEgressBytes = a.baselineEgress
		snap.LastAlertAt = a.lastAlertAt
		found = true
	}
	return snap, found
}

// RateLimiterSnapshot is a read-only projection of the rate
// limiter's per-tenant counters. See Snapshot.
type RateLimiterSnapshot struct {
	EgressBytesThisMonth int64
	MonthStart           time.Time
	WindowReqs           int64
	WindowEgressBytes    int64
	BaselineReqs         float64
	BaselineEgressBytes  float64
	LastAlertAt          time.Time
}

// throttling reports whether tenantID is currently inside an active
// anomaly-throttle cooldown.
func (l *RateLimiter) throttling(tenantID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.anomaly[tenantID]
	if !ok {
		return false
	}
	return l.now().Before(a.throttleUntil)
}

type alertDecision struct {
	fire  bool
	ratio uint64
}

// maybeAlert evaluates the current window against the baseline. It
// must be called with l.mu held. Returns the decision to fire an
// alert without touching the sink so the caller can release the
// lock before emitting.
func (l *RateLimiter) maybeAlert(a *anomalyTracker, now time.Time, window time.Duration) alertDecision {
	multiplier := l.AnomalyMultiplier
	if multiplier <= 0 {
		multiplier = 10.0
	}
	elapsed := now.Sub(a.windowStart).Seconds()
	if elapsed <= 0 {
		return alertDecision{}
	}
	currentReqsPerSec := float64(a.windowReqs) / elapsed
	currentEgressBps := float64(a.windowEgressByte) / elapsed
	minReqs := l.MinBaselineReqs
	if minReqs <= 0 {
		minReqs = 1.0
	}
	minEgress := l.MinBaselineEgressBytes
	if minEgress <= 0 {
		minEgress = 1024.0
	}
	baselineReqs := a.baselineReqs
	if baselineReqs < minReqs {
		baselineReqs = minReqs
	}
	baselineEgress := a.baselineEgress
	if baselineEgress < minEgress {
		baselineEgress = minEgress
	}

	reqsRatio := currentReqsPerSec / baselineReqs
	egressRatio := currentEgressBps / baselineEgress
	ratio := reqsRatio
	if egressRatio > ratio {
		ratio = egressRatio
	}
	if ratio < multiplier {
		return alertDecision{}
	}

	cooldown := l.AnomalyCooldown
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

// rollWindow folds the current window into the EWMA baseline and
// resets the counters. Must be called with l.mu held.
func (l *RateLimiter) rollWindow(a *anomalyTracker, now time.Time, window time.Duration) {
	elapsed := now.Sub(a.windowStart).Seconds()
	if elapsed <= 0 {
		elapsed = window.Seconds()
	}
	reqsPerSec := float64(a.windowReqs) / elapsed
	egressBps := float64(a.windowEgressByte) / elapsed
	alpha := l.BaselineAlpha
	if alpha <= 0 || alpha > 1 {
		alpha = 0.3
	}
	if a.baselineReqs == 0 && a.baselineEgress == 0 {
		a.baselineReqs = reqsPerSec
		a.baselineEgress = egressBps
	} else {
		a.baselineReqs = alpha*reqsPerSec + (1-alpha)*a.baselineReqs
		a.baselineEgress = alpha*egressBps + (1-alpha)*a.baselineEgress
	}
	a.windowStart = now
	a.windowReqs = 0
	a.windowEgressByte = 0
}

func (l *RateLimiter) alert(tenantID string, dim billing.Dimension, delta uint64) {
	if l.AlertSink == nil {
		return
	}
	l.AlertSink.Emit(billing.UsageEvent{
		TenantID:   tenantID,
		Dimension:  dim,
		Delta:      delta,
		ObservedAt: l.now(),
	})
}

func (l *RateLimiter) anomalyWindow() time.Duration {
	if l.AnomalyWindow <= 0 {
		return time.Minute
	}
	return l.AnomalyWindow
}

func (l *RateLimiter) now() time.Time {
	if l.Clock == nil {
		return time.Now()
	}
	return l.Clock()
}

// countingWriter wraps http.ResponseWriter to count bytes passed
// through Write so the rate limiter can tally egress without
// cooperation from the upstream handler.
type countingWriter struct {
	http.ResponseWriter
	bytes int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// monthStart returns the 00:00:00 UTC timestamp of the first day of
// t's month. It is the reset anchor for the monthly egress budget.
func monthStart(t time.Time) time.Time {
	y, m, _ := t.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

func sameMonth(a, b time.Time) bool {
	ay, am, _ := a.UTC().Date()
	by, bm, _ := b.UTC().Date()
	return ay == by && am == bm
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// TenantBudgetsLookup adapts a *MemoryTenantStore to the
// RateLimitLookup signature. The burst is the same as rps when no
// explicit burst is configured: the Phase 2 tenant record carries
// only a single RequestsPerSec knob.
func TenantBudgetsLookup(store TenantStore) RateLimitLookup {
	return func(tenantID string) (int, int, bool) {
		b, ok := store.LookupByTenantID(tenantID)
		if !ok {
			return 0, 0, false
		}
		rps := b.Tenant.Budgets.RequestsPerSec
		if rps <= 0 {
			return 0, 0, false
		}
		return rps, rps, true
	}
}

// TenantEgressBudgetLookup adapts a *MemoryTenantStore to the
// EgressBudgetLookup signature by converting the tenant record's
// Budgets.EgressTBMonth field (a TB/month float for human
// authoring) into a bytes-per-month int64 for the limiter.
func TenantEgressBudgetLookup(store *MemoryTenantStore) EgressBudgetLookup {
	return func(tenantID string) (int64, bool) {
		b, ok := store.LookupByTenantID(tenantID)
		if !ok {
			return 0, false
		}
		tb := b.Tenant.Budgets.EgressTBMonth
		if tb <= 0 {
			return 0, true
		}
		const bytesPerTB = int64(1) << 40 // 1 TiB
		return int64(tb * float64(bytesPerTB)), true
	}
}

// TenantResolverFromAuth wraps an Authenticator into a TenantResolver
// so the rate limiter can identify the tenant using the same HMAC
// signature verification the S3 handler runs. Unauthenticated
// requests return ok=false, which causes the limiter to skip them.
type requestAuthenticator interface {
	Authenticate(r *http.Request) (string, error)
}

func TenantResolverFromAuth(a requestAuthenticator) TenantResolver {
	return func(r *http.Request) (string, bool) {
		tenantID, err := a.Authenticate(r)
		if err != nil {
			return "", false
		}
		return tenantID, true
	}
}
