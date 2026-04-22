package auth

import (
	"net/http"
	"sync"
	"time"
)

// RateLimitLookup resolves a tenant ID to the token-bucket steady-
// state rate (requests per second) and burst size. The gateway wires
// this to its tenant store so per-tenant budgets drive the limiter.
type RateLimitLookup func(tenantID string) (rps int, burst int, ok bool)

// TenantResolver is the function the middleware uses to identify the
// tenant for a request. It typically wraps an Authenticator so
// rate-limiting can short-circuit unauthenticated requests before
// they hit the upstream handler.
type TenantResolver func(r *http.Request) (tenantID string, ok bool)

// RateLimiter is a token-bucket middleware keyed by tenant ID. It
// refills each tenant's bucket continuously at the configured rps
// and caps it at burst. Requests that cannot draw a token receive
// an HTTP 429.
type RateLimiter struct {
	Lookup   RateLimitLookup
	Resolver TenantResolver
	Clock    func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens    float64
	capacity  float64
	refillPS  float64
	lastEvent time.Time
}

// NewRateLimiter builds a RateLimiter. If clock is nil it uses
// time.Now.
func NewRateLimiter(lookup RateLimitLookup, resolver TenantResolver) *RateLimiter {
	return &RateLimiter{
		Lookup:   lookup,
		Resolver: resolver,
		Clock:    time.Now,
		buckets:  map[string]*bucket{},
	}
}

// Middleware returns an http.Handler that rate-limits per tenant and
// otherwise delegates to next.
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
		next.ServeHTTP(w, r)
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
	clock := l.Clock
	if clock == nil {
		clock = time.Now
	}
	now := clock()

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
func TenantBudgetsLookup(store *MemoryTenantStore) RateLimitLookup {
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
