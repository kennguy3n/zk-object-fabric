package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// UsageStreamConfig collects the dependencies the SSE handler needs.
type UsageStreamConfig struct {
	// Usage is the query surface the handler polls each tick.
	Usage UsageQuery

	// Tokens authenticates the caller. The browser's EventSource
	// API cannot set arbitrary headers, so the SPA passes its
	// bearer token via the ?token= query parameter; the handler
	// resolves it here and rejects any request whose token is
	// missing, unknown, or bound to a different tenant than the
	// URL path. A nil Tokens store makes the endpoint refuse all
	// requests rather than falling through to an unauthenticated
	// stream.
	Tokens TokenStore

	// Interval is the poll cadence between SSE frames. Defaults
	// to 5 seconds.
	Interval time.Duration

	// Window is the lookback window reported on each frame.
	// Defaults to 30 days, matching the dashboard's default
	// GET /usage window so the live values line up with the
	// initial snapshot the SPA rendered on page load.
	Window time.Duration

	// Now returns the current time. Defaults to time.Now.
	Now Clock
}

// UsageStreamHandler streams per-tenant usage counters to the SPA
// dashboard over Server-Sent Events.
//
// The frontend opens an EventSource to
// GET /api/v1/usage/stream/{tenantID} and renders each JSON-encoded
// frame as it arrives. The handler writes frames at UsageStreamConfig
// .Interval (default 5 s) and respects r.Context().Done() so a
// client disconnect unwinds the goroutine immediately.
//
// The handler is separate from the REST GET /api/tenants/{id}/usage
// endpoint so a slow SSE subscriber can never block a request coming
// in through the same mux.
type UsageStreamHandler struct {
	cfg UsageStreamConfig
}

// NewUsageStreamHandler returns a handler with defaults filled in.
func NewUsageStreamHandler(cfg UsageStreamConfig) *UsageStreamHandler {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Window <= 0 {
		cfg.Window = 30 * 24 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &UsageStreamHandler{cfg: cfg}
}

// Register mounts the SSE endpoint under
// /api/v1/usage/stream/{tenantID}. The console-mux alias at
// /api/tenants/{tenantID}/usage/stream is dispatched by the
// console Handler (which already owns the /api/tenants/ prefix);
// it forwards matching requests through ServeHTTP via
// parseUsageStreamPath, which accepts either form.
func (h *UsageStreamHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/usage/stream/", h.ServeHTTP)
}

// UsageStreamEvent is the JSON payload emitted on each SSE frame.
// The field names match UsageResponse so a client can reuse the same
// parser for both the REST snapshot and the SSE stream.
type UsageStreamEvent struct {
	TenantID   string            `json:"tenant_id"`
	ObservedAt time.Time         `json:"observed_at"`
	Start      time.Time         `json:"start"`
	End        time.Time         `json:"end"`
	Counters   map[string]uint64 `json:"counters"`
}

// ServeHTTP implements http.Handler. It parses the tenant ID from
// the trailing path segment, sets the SSE headers, and then loops
// until the client disconnects or UsageQuery returns a
// non-retryable error.
func (h *UsageStreamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.cfg.Usage == nil {
		writeError(w, http.StatusServiceUnavailable, "usage query not configured")
		return
	}
	if h.cfg.Tokens == nil {
		// Without a TokenStore the endpoint has no way to
		// authenticate the caller, so refuse rather than leak
		// per-tenant usage counters to anyone who knows a
		// tenant ID.
		writeError(w, http.StatusServiceUnavailable, "usage stream auth not configured")
		return
	}
	tenantID, ok := parseUsageStreamPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "expected /api/v1/usage/stream/{tenantID}")
		return
	}
	// EventSource cannot send an Authorization header, so the SPA
	// mints a short-lived bearer token via /auth/login and passes
	// it on the query string. We accept the Authorization header
	// too so non-browser clients (curl, Go tests) can use the
	// same endpoint without special-casing.
	token := extractUsageStreamToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	boundTenant, ok := h.cfg.Tokens.ResolveToken(token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid bearer token")
		return
	}
	if boundTenant != tenantID {
		// The caller's token is valid but authenticates a
		// different tenant. Return 403 so a probing client
		// cannot distinguish "tenant does not exist" from
		// "tenant exists but I'm not allowed to stream it".
		writeError(w, http.StatusForbidden, "token does not authorize this tenant")
		return
	}
	// SSE requires a ResponseWriter that supports Flush so frames
	// leave the server buffer the instant they are written. Without
	// this, frames accumulate until the kernel's write-buffer
	// threshold fires and the dashboard appears frozen.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// X-Accel-Buffering: no disables nginx response buffering in
	// deployments that front the console with an ingress proxy.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	// Emit the first frame immediately so the client does not have
	// to wait Interval to render its first data point.
	if err := h.emit(ctx, w, flusher, tenantID); err != nil {
		if errors.Is(err, ctx.Err()) {
			return
		}
		h.writeErrorFrame(w, flusher, err)
		return
	}

	ticker := time.NewTicker(h.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.emit(ctx, w, flusher, tenantID); err != nil {
				if errors.Is(err, ctx.Err()) {
					return
				}
				h.writeErrorFrame(w, flusher, err)
				return
			}
		}
	}
}

// emit polls UsageQuery once and writes a single SSE frame.
func (h *UsageStreamHandler) emit(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	tenantID string,
) error {
	now := h.cfg.Now()
	start := now.Add(-h.cfg.Window)
	counters, err := h.cfg.Usage.TenantUsage(ctx, tenantID, start, now)
	if err != nil {
		return err
	}
	event := UsageStreamEvent{
		TenantID:   tenantID,
		ObservedAt: now,
		Start:      start,
		End:        now,
		Counters:   counters,
	}
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("console: marshal usage frame: %w", err)
	}
	// SSE frame format: "event: usage\ndata: <json>\n\n". The named
	// event type lets the client add a typed listener rather than
	// relying on the default "message" event.
	if _, err := fmt.Fprintf(w, "event: usage\ndata: %s\n\n", body); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writeErrorFrame surfaces a non-retryable error to the client as a
// typed SSE "error" event before the handler exits.
func (h *UsageStreamHandler) writeErrorFrame(w http.ResponseWriter, flusher http.Flusher, err error) {
	payload, _ := json.Marshal(errorResponse{Error: err.Error()})
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	flusher.Flush()
}

// extractUsageStreamToken returns the bearer token the caller
// supplied, preferring the Authorization header when present and
// falling back to the ?token= query parameter EventSource clients
// use. It returns an empty string when the caller supplied neither.
func extractUsageStreamToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if strings.HasPrefix(h, prefix) {
			return strings.TrimSpace(h[len(prefix):])
		}
	}
	return strings.TrimSpace(r.URL.Query().Get("token"))
}

// parseUsageStreamPath extracts the tenant ID from either of the
// two SSE mounts:
//
//   - /api/v1/usage/stream/{id} — the SPA EventSource path.
//   - /api/tenants/{id}/usage/stream — the console-mux alias.
//
// A trailing slash is tolerated on both so the frontend can
// normalise URLs without affecting routing.
func parseUsageStreamPath(p string) (string, bool) {
	if id, ok := parseV1UsageStreamPath(p); ok {
		return id, true
	}
	return parseTenantScopedUsageStreamPath(p)
}

func parseV1UsageStreamPath(p string) (string, bool) {
	const prefix = "/api/v1/usage/stream/"
	if !strings.HasPrefix(p, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(p, prefix)
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

func parseTenantScopedUsageStreamPath(p string) (string, bool) {
	const prefix = "/api/tenants/"
	const suffix = "/usage/stream"
	if !strings.HasPrefix(p, prefix) {
		return "", false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(p, prefix), "/")
	if !strings.HasSuffix(rest, suffix) {
		return "", false
	}
	id := strings.TrimSuffix(rest, suffix)
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}
