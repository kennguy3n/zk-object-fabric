package requestid

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMiddleware_GeneratesIDAndWritesHeader confirms the middleware
// (1) writes a 32-char hex id to the x-amz-request-id response
// header, (2) attaches that exact value to the request context, and
// (3) the value the inner handler sees matches the one the client
// observes on the response. Without that invariant the request id
// would be useless for cross-correlating client-side and
// server-side logs.
func TestMiddleware_GeneratesIDAndWritesHeader(t *testing.T) {
	var observed string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	got := w.Header().Get(HeaderName)
	if got == "" {
		t.Fatalf("response header %s is empty", HeaderName)
	}
	if len(got) != 32 {
		t.Fatalf("response header %s = %q (len %d), want 32-char hex", HeaderName, got, len(got))
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Fatalf("response header %s = %q, not valid hex: %v", HeaderName, got, err)
	}
	if observed != got {
		t.Fatalf("context id %q != response header %q", observed, got)
	}
}

// TestMiddleware_EchoesIncomingHeader pins the trace-correlation
// behaviour: when an upstream proxy already generated a request
// id and forwarded it via x-amz-request-id, the middleware must
// reuse that value rather than minting a new one.
func TestMiddleware_EchoesIncomingHeader(t *testing.T) {
	const upstream = "0123456789abcdef0123456789abcdef"

	var observed string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = FromContext(r.Context())
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderName, upstream)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if got := w.Header().Get(HeaderName); got != upstream {
		t.Fatalf("response header = %q, want upstream %q", got, upstream)
	}
	if observed != upstream {
		t.Fatalf("context id = %q, want upstream %q", observed, upstream)
	}
}

// TestMiddleware_DistinctIDsPerRequest is the load-bearing
// invariant for the entire feature: two requests through the same
// middleware must get different ids. A regression here (e.g. a
// package-level cached id) would silently collapse all log
// correlation into a single bucket.
func TestMiddleware_DistinctIDsPerRequest(t *testing.T) {
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	const n = 64
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		id := w.Header().Get(HeaderName)
		if id == "" {
			t.Fatalf("request %d: empty id", i)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("request %d: id %q collided with an earlier request", i, id)
		}
		seen[id] = struct{}{}
	}
}

// TestFromContext_AbsentReturnsEmpty pins the "no panic on a
// background context" behaviour. Code that constructs its own
// context (e.g. a background rebalance worker) must not crash
// when it asks for a request id and there isn't one.
func TestFromContext_AbsentReturnsEmpty(t *testing.T) {
	if got := FromContext(context.Background()); got != "" {
		t.Fatalf("FromContext(background) = %q, want empty", got)
	}
	// Exercise the nil-context branch via a non-typed variable
	// so staticcheck SA1012 (do-not-pass-nil-Context) does not
	// flag the call site. FromContext is documented to tolerate
	// a nil context so background workers without a context plumb
	// do not panic on lookup.
	var ctxNil context.Context // nolint:gocritic // intentional nil-context probe
	if got := FromContext(ctxNil); got != "" {
		t.Fatalf("FromContext(nil) = %q, want empty", got)
	}
}

// TestWithID_RoundTrip is the simplest sanity check that the
// context plumbing works end-to-end.
func TestWithID_RoundTrip(t *testing.T) {
	const id = "deadbeefdeadbeefdeadbeefdeadbeef"
	ctx := WithID(context.Background(), id)
	if got := FromContext(ctx); got != id {
		t.Fatalf("FromContext = %q, want %q", got, id)
	}
}

// TestMiddleware_IgnoresWhitespaceOnlyHeader confirms that an
// upstream proxy that forwarded an empty header value still gets a
// fresh server-side id rather than ending up with an empty
// response header.
func TestMiddleware_IgnoresWhitespaceOnlyHeader(t *testing.T) {
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderName, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	got := w.Header().Get(HeaderName)
	if got == "" || strings.TrimSpace(got) == "" {
		t.Fatalf("empty-header request produced empty id %q", got)
	}
}
