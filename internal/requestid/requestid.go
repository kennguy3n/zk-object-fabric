// Package requestid generates a 128-bit request identifier for
// every incoming HTTP request, attaches it to the request context,
// and echoes it back as the x-amz-request-id response header.
//
// The identifier is rendered as 32 lowercase hex characters from
// 16 random bytes drawn via crypto/rand. That keeps the wire
// representation small (32 bytes vs UUID's 36 bytes with hyphens)
// while still being a 2^128 namespace that won't collide under
// normal request volumes.
//
// Downstream code retrieves the identifier with FromContext for
// inclusion in structured log lines, audit records, or traces.
// When called on a request that did not go through Middleware
// (e.g. unit tests, internal goroutines), FromContext returns
// an empty string and the caller is expected to treat that as
// "no request id available" rather than failing.
package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// HeaderName is the response header the middleware writes. The
// name matches the S3 protocol's response header so that S3
// clients which already log it for support tickets pick up the
// gateway-generated identifier automatically.
const HeaderName = "x-amz-request-id"

// ctxKey is the unexported context key used to stash the
// generated identifier. It is intentionally an empty struct so
// it cannot collide with another package's int-typed key.
type ctxKey struct{}

// Middleware returns an http.Handler that runs next with a
// freshly-generated request id attached to the context. The id
// is also written to the response under HeaderName before next
// runs so downstream handlers cannot accidentally drop it by
// calling w.WriteHeader before they look at the context.
//
// If the incoming request already carries an x-amz-request-id
// header (e.g. an upstream proxy generated one), the middleware
// reuses that value so cross-service traces stay correlated.
// Empty / whitespace values are ignored; the middleware
// generates a fresh id in that case.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Trim before the empty-check so a misbehaving upstream
		// proxy that sends an all-whitespace header value (e.g.
		// "   " from a sloppy header rewrite) does not propagate
		// a whitespace-only id into our logs and response
		// header. The doc above promises this behaviour.
		id := strings.TrimSpace(r.Header.Get(HeaderName))
		if id == "" {
			id = newID()
		}
		w.Header().Set(HeaderName, id)
		next.ServeHTTP(w, r.WithContext(WithID(r.Context(), id)))
	})
}

// WithID returns a copy of parent with the given id attached.
// This is exported so call sites that build their own context
// (e.g. background jobs that inherit a request id from an
// upstream call) can attach the same id without going through
// the HTTP middleware.
func WithID(parent context.Context, id string) context.Context {
	return context.WithValue(parent, ctxKey{}, id)
}

// FromContext returns the request id attached to ctx, or the
// empty string if no id is attached.
func FromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// newID returns 32 hex characters drawn from 16 random bytes.
// crypto/rand.Read on Linux is backed by getrandom(2) which is
// non-blocking after the kernel's entropy pool is initialised at
// boot; an error from Read means the kernel itself is broken,
// at which point the gateway cannot serve traffic anyway, so we
// return an empty string and let the middleware fall through —
// the response header is still set (to "") and downstream
// handlers see ctx.Value() == "". This is deliberate: failing
// the request because the kernel cannot generate randomness
// would be a strictly worse outcome than logging the request
// without an id.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
