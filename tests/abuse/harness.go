// Package abuse contains adversarial end-to-end tests for the
// gateway's edge defences: the rate limiter, the abuse guard, the
// connection-exhaustion timeouts, and the tenant-isolation
// guarantees of the authenticator.
//
// The harness is deliberately built against a real net.Listener
// and the actual production middleware chain rather than calling
// handler methods directly. Slowloris, in particular, cannot be
// reproduced against an in-memory transport because it exercises
// the http.Server's ReadHeaderTimeout / IdleTimeout knobs, which
// only fire under raw TCP socket I/O. Treating these as
// production-pathway integration tests keeps regressions
// from sneaking in via a refactor that bypasses a middleware (or
// drops a timeout) without changing the unit-test surface.
//
// The terminal handler at the end of the chain echoes the
// resolved tenant ID back as the X-Tenant-Echo header and writes
// a fixed-size body. The body size is the lever the egress-budget
// test uses to overflow the abuse guard's monthly counter without
// having to drive thousands of requests.
package abuse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	neturl "net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyendpoints "github.com/aws/smithy-go/endpoints"

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/internal/auth"
	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// echoHandler is the terminal http.Handler the harness installs
// behind the middleware chain. It writes the resolved tenant ID
// (as identified by the request's authenticator round-trip) into
// the X-Tenant-Echo header, then streams cfg.BodySize bytes back
// to the client. Tests rely on the echo to assert tenant
// isolation, and rely on cfg.BodySize to drive the egress
// counter without standing up a content-addressable store.
type echoHandler struct {
	auth     *auth.HMACAuthenticator
	bodySize int
}

func (h *echoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Run authentication so the echo reflects the SAME tenant
	// the middleware chain resolved. If the middleware resolver
	// disagrees with the authenticator (a tenant-isolation
	// regression), this header makes the divergence observable
	// in the test instead of silently passing.
	tid, err := h.auth.Authenticate(r)
	if err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusForbidden)
		return
	}
	w.Header().Set("X-Tenant-Echo", tid)
	w.Header().Set("Content-Length", strconv.Itoa(h.bodySize))
	w.WriteHeader(http.StatusOK)
	if h.bodySize > 0 {
		// Stream the response body in fixed chunks so write
		// buffering does not let a partial flush short-circuit
		// the egress counter.
		buf := make([]byte, 1024)
		for written := 0; written < h.bodySize; {
			n := len(buf)
			if remaining := h.bodySize - written; remaining < n {
				n = remaining
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			written += n
		}
	}
}

// TenantSpec is the harness's view of one tenant. It carries
// both the auth binding (so the SDK can sign requests with this
// tenant's credentials) and the rate-limit / budget knobs that
// drive the middleware decisions.
type TenantSpec struct {
	ID            string
	AccessKey     string
	SecretKey     string
	RPS           int
	Burst         int
	EgressTBMonth float64 // 0 means "no monthly cap"
}

// HarnessConfig parameterises the test harness. All fields are
// optional; sensible defaults are filled in by NewHarness.
type HarnessConfig struct {
	Tenants []TenantSpec

	// BodySize is the number of bytes the echo handler streams
	// back on every successful response. Defaults to 0 (header
	// only) so tests that do not exercise egress-budget logic
	// run in microseconds.
	BodySize int

	// ReadHeaderTimeout is the deadline applied to the test
	// server's http.Server. Tests that exercise Slowloris pin
	// this very low (e.g. 100ms) to keep the test fast.
	ReadHeaderTimeout time.Duration

	// IdleTimeout mirrors the production knob. Defaults to
	// ReadHeaderTimeout if zero.
	IdleTimeout time.Duration

	// MaxHeaderBytes caps the request-header size. Zero uses
	// the Go default (1 MiB) which is fine for every test
	// except the oversized-header scenario.
	MaxHeaderBytes int

	// AnomalyMultiplier overrides the abuse guard's default
	// 2.0 multiplier. Lower values make the anomaly trip on
	// smaller traffic deltas; tests that drive anomaly events
	// set this to 1.1 so a modest burst suffices.
	AnomalyMultiplier float64

	// AnomalyWindow overrides the default 1-minute sliding
	// window so the anomaly accumulator can drain inside a
	// short test.
	AnomalyWindow time.Duration

	// AnomalyCooldown is the duration the throttle stays on
	// after the anomaly fires. Defaults to AnomalyWindow.
	AnomalyCooldown time.Duration

	// ThrottleOnAnomaly toggles the AbuseGuard.ThrottleOnAnomaly
	// behaviour. When false (default) the guard only alerts.
	ThrottleOnAnomaly bool
}

// CollectingSink is an in-memory billing.AlertSink that records
// every UsageEvent emitted by the rate limiter or abuse guard.
// Tests read from Events to assert that the right alert fired.
type CollectingSink struct {
	mu     sync.Mutex
	events []billing.UsageEvent
}

// Emit implements auth.AlertSink.
func (c *CollectingSink) Emit(event billing.UsageEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

// Events returns a snapshot of the collected events. The slice is
// a copy so callers can iterate safely while the sink keeps
// receiving new events.
func (c *CollectingSink) Events() []billing.UsageEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]billing.UsageEvent, len(c.events))
	copy(out, c.events)
	return out
}

// CountByDimension counts the events whose Dimension matches d.
// Tests assert on this rather than the raw slice length so a
// single dimension's volume can be checked in isolation.
func (c *CollectingSink) CountByDimension(d billing.Dimension) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e.Dimension == d {
			n++
		}
	}
	return n
}

// Harness is the test fixture exposed to the abuse tests. It
// owns the http.Server, the listener, the middleware chain, and
// helpers to mint signed requests on behalf of any registered
// tenant.
type Harness struct {
	T           *testing.T
	Server      *http.Server
	Listener    net.Listener
	URL         string
	Sink        *CollectingSink
	Limiter     *auth.RateLimiter
	Abuse       *auth.AbuseGuard
	Auth        *auth.HMACAuthenticator
	Tenants     map[string]TenantSpec
	tenantStore *auth.MemoryTenantStore
}

// NewHarness boots an http.Server backed by the real production
// middleware chain (abuse guard -> rate limiter -> SigV4
// authenticator echo) matching cmd/gateway/main.go's
// `ag.Middleware(rl.Middleware(handler))` wiring. With this order
// AbuseGuard.Observe() sees every inbound request (including the
// ones the rate limiter rejects with 429), so its anomaly tracker
// and egress budget reflect actual offered load rather than only
// the slice that passes the token bucket. The server listens on
// an ephemeral port; the URL is exposed as h.URL for tests to dial.
//
// The caller is responsible for calling Close in a t.Cleanup or
// defer so the server's goroutines exit cleanly.
func NewHarness(t *testing.T, cfg HarnessConfig) *Harness {
	t.Helper()

	if cfg.AnomalyMultiplier == 0 {
		cfg.AnomalyMultiplier = 2.0
	}
	if cfg.AnomalyWindow == 0 {
		cfg.AnomalyWindow = time.Minute
	}
	if cfg.AnomalyCooldown == 0 {
		cfg.AnomalyCooldown = cfg.AnomalyWindow
	}

	store := auth.NewMemoryTenantStore()
	tenants := map[string]TenantSpec{}

	for _, ts := range cfg.Tenants {
		// Default-fill the budget knobs so a TenantSpec with
		// only an ID is still wired into the store and the
		// rate-limit lookups.
		if ts.RPS == 0 {
			ts.RPS = 1000
		}
		if ts.Burst == 0 {
			ts.Burst = ts.RPS
		}
		if err := store.AddBinding(auth.TenantBinding{
			AccessKey: ts.AccessKey,
			SecretKey: ts.SecretKey,
			Tenant: tenant.Tenant{
				ID:               ts.ID,
				Name:             ts.ID,
				ContractType:     tenant.ContractB2CPooled,
				LicenseTier:      tenant.LicenseStandard,
				Keys:             tenant.Keys{RootKeyRef: "cmk://test/root", DEKPolicy: tenant.DEKPerObject},
				PlacementDefault: tenant.PlacementDefault{PolicyRef: "p_test"},
				Budgets: tenant.Budgets{
					EgressTBMonth:  ts.EgressTBMonth,
					RequestsPerSec: ts.RPS,
				},
				Billing: tenant.Billing{Currency: "USD"},
			},
		}); err != nil {
			t.Fatalf("AddBinding(%s): %v", ts.ID, err)
		}
		tenants[ts.ID] = ts
	}

	authenticator := &auth.HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        time.Now,
		MaxClockSkew: time.Hour,
		Strategies:   auth.DefaultStrategies(),
	}

	// Resolver runs the SigV4 verifier so the middleware sees
	// the same tenant ID the terminal handler will. This is the
	// production wiring — tenant-isolation regressions surface
	// here, not in a separate parallel resolver path.
	resolver := func(r *http.Request) (string, bool) {
		tid, err := authenticator.Authenticate(r)
		if err != nil {
			return "", false
		}
		return tid, true
	}

	lookup := func(tid string) (int, int, bool) {
		ts, ok := tenants[tid]
		if !ok {
			return 0, 0, false
		}
		return ts.RPS, ts.Burst, true
	}

	egressLookup := func(tid string) (int64, bool) {
		ts, ok := tenants[tid]
		if !ok {
			return 0, false
		}
		// tenant.Budgets stores monthly TB; the limiter expects
		// bytes. The conversion uses powers of 10 to match the
		// billing pipeline.
		return int64(ts.EgressTBMonth * 1e12), true
	}

	tenantLookup := func(tid string) (tenant.Tenant, bool) {
		b, ok := store.LookupByTenantID(tid)
		if !ok {
			return tenant.Tenant{}, false
		}
		return b.Tenant, true
	}

	sink := &CollectingSink{}

	limiter := auth.NewRateLimiter(lookup, resolver)
	limiter.EgressLookup = egressLookup
	limiter.AlertSink = sink
	limiter.AnomalyMultiplier = cfg.AnomalyMultiplier
	limiter.AnomalyWindow = cfg.AnomalyWindow
	limiter.AnomalyCooldown = cfg.AnomalyCooldown

	abuse := auth.NewAbuseGuard(tenantLookup, resolver)
	abuse.AlertSink = sink
	abuse.AnomalyMultiplier = cfg.AnomalyMultiplier
	abuse.AnomalyWindow = cfg.AnomalyWindow
	abuse.AnomalyCooldown = cfg.AnomalyCooldown
	abuse.ThrottleOnAnomaly = cfg.ThrottleOnAnomaly

	terminal := &echoHandler{auth: authenticator, bodySize: cfg.BodySize}
	// Order MUST mirror cmd/gateway/main.go:320
	// (ag.Middleware(rl.Middleware(handler))): AbuseGuard is
	// outermost so its anomaly + budget counters observe every
	// inbound request, including ones the rate limiter throws away
	// with 429. Flipping these around silently weakens both
	// detectors and would make production abuse-control regressions
	// invisible to this suite.
	chain := abuse.Middleware(limiter.Middleware(terminal))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	srv := &http.Server{
		Handler:           chain,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout: func() time.Duration {
			if cfg.IdleTimeout != 0 {
				return cfg.IdleTimeout
			}
			return cfg.ReadHeaderTimeout
		}(),
		MaxHeaderBytes: cfg.MaxHeaderBytes,
	}
	go func() {
		// The harness's listener may close while the server is
		// still mid-handshake; surface anything that isn't the
		// expected "use of closed network connection" so a
		// regression that hangs the listener loop is visible.
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			t.Logf("test server: %v", err)
		}
	}()

	h := &Harness{
		T:           t,
		Server:      srv,
		Listener:    listener,
		URL:         "http://" + listener.Addr().String(),
		Sink:        sink,
		Limiter:     limiter,
		Abuse:       abuse,
		Auth:        authenticator,
		Tenants:     tenants,
		tenantStore: store,
	}
	t.Cleanup(h.Close)
	return h
}

// Close shuts the http.Server down. Safe to call multiple times.
func (h *Harness) Close() {
	if h.Server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = h.Server.Shutdown(ctx)
	h.Server = nil
}

// PresignedGet returns a SigV4-presigned URL for /bucket/<key>
// signed with tenantID's credentials. Tests use this rather
// than hand-rolling auth headers so the harness's signing path
// matches what an SDK client would emit.
func (h *Harness) PresignedGet(tenantID, key string) string {
	h.T.Helper()
	ts, ok := h.Tenants[tenantID]
	if !ok {
		h.T.Fatalf("unknown tenant %q", tenantID)
	}
	addr := h.Listener.Addr().String()
	client := s3.New(s3.Options{
		Region:             "us-east-1",
		Credentials:        credentials.NewStaticCredentialsProvider(ts.AccessKey, ts.SecretKey, ""),
		UsePathStyle:       true,
		EndpointResolverV2: fixedEndpointResolver{host: addr},
	})
	presigner := s3.NewPresignClient(client, func(o *s3.PresignOptions) {
		o.Expires = 5 * time.Minute
	})
	req, err := presigner.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String(key),
	})
	if err != nil {
		h.T.Fatalf("PresignGetObject(%s): %v", tenantID, err)
	}
	return req.URL
}

// fixedEndpointResolver pins the AWS SDK v2 presigner to the
// harness's ephemeral listener so the resulting URL has the
// host:port the test server actually listens on.
type fixedEndpointResolver struct {
	host string
}

func (r fixedEndpointResolver) ResolveEndpoint(_ context.Context, _ s3.EndpointParameters) (smithyendpoints.Endpoint, error) {
	u, err := neturl.Parse("http://" + r.host)
	if err != nil {
		return smithyendpoints.Endpoint{}, err
	}
	return smithyendpoints.Endpoint{URI: *u}, nil
}

// randomKey returns an unpredictable object key for tests that
// want each request to look unique. A SigV4 signature varies
// with the key, so distinct keys also serve as a regression
// canary if a future refactor accidentally caches signatures.
func randomKey() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return "key/" + hex.EncodeToString(buf[:])
}

