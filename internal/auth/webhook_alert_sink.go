package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
)

// WebhookAlertSink fans abuse / anomaly events out to a generic
// HTTP webhook. It satisfies AlertSink (and by extension
// billing.BillingSink semantics for the AbuseGuard / RateLimiter)
// without touching the durable billing pipeline. Production
// gateways install this alongside the ClickHouse billing sink via
// MultiAlertSink so PagerDuty / Slack / a generic webhook receives
// abuse signals in real time even if the billing sink batches.
//
// The sink intentionally fires-and-forgets: a slow webhook target
// must not stall the request that triggered the alert. Each Emit
// dispatches a goroutine bounded by a semaphore so a stuck endpoint
// can only consume MaxInFlight goroutines before new events fall
// through with a logged warning.
type WebhookAlertSink struct {
	// URL is the webhook endpoint the sink POSTs to. Required.
	URL string
	// Method overrides the HTTP method. Defaults to POST.
	Method string
	// Headers overrides the request headers. Content-Type
	// defaults to application/json when not provided.
	Headers map[string]string
	// HTTPClient overrides the default client (10s timeout).
	HTTPClient *http.Client
	// Logger receives transport / decode failures. Nil disables
	// logging.
	Logger *log.Logger
	// MaxInFlight bounds concurrent webhook deliveries. Zero
	// defaults to 16 — enough headroom that a single guarded
	// tenant cannot saturate the sink, low enough that a stuck
	// endpoint cannot leak unbounded goroutines.
	MaxInFlight int
	// Timeout overrides the per-request timeout when HTTPClient
	// is not supplied. Defaults to 10 seconds.
	Timeout time.Duration

	once sync.Once
	sem  chan struct{}
}

// NewWebhookAlertSink builds a webhook sink bound to url. The
// returned sink is safe for concurrent use; Emit dispatches each
// event on its own goroutine bounded by MaxInFlight.
func NewWebhookAlertSink(url string) *WebhookAlertSink {
	return &WebhookAlertSink{URL: url}
}

func (s *WebhookAlertSink) init() {
	s.once.Do(func() {
		max := s.MaxInFlight
		if max <= 0 {
			max = 16
		}
		s.sem = make(chan struct{}, max)
	})
}

func (s *WebhookAlertSink) client() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	t := s.Timeout
	if t <= 0 {
		t = 10 * time.Second
	}
	return &http.Client{Timeout: t}
}

// Emit serializes ev as JSON and POSTs it to the configured URL.
// The call is non-blocking: it returns as soon as the goroutine is
// scheduled (or immediately, dropping the event with a warning, if
// the in-flight semaphore is saturated).
func (s *WebhookAlertSink) Emit(ev billing.UsageEvent) {
	if s == nil || s.URL == "" {
		return
	}
	s.init()
	select {
	case s.sem <- struct{}{}:
	default:
		s.logf("auth: webhook alert sink saturated; dropping event tenant=%s dim=%s", ev.TenantID, ev.Dimension)
		return
	}
	go func() {
		defer func() { <-s.sem }()
		if err := s.deliver(ev); err != nil {
			s.logf("auth: webhook alert delivery failed: %v", err)
		}
	}()
}

// deliver issues the actual webhook request. It is exposed (lower-
// case) so tests can synchronously exercise the JSON envelope and
// HTTP semantics without racing against the goroutine pool.
func (s *WebhookAlertSink) deliver(ev billing.UsageEvent) error {
	body, err := json.Marshal(webhookPayload{
		TenantID:     ev.TenantID,
		Bucket:       ev.Bucket,
		Dimension:    string(ev.Dimension),
		Delta:        ev.Delta,
		ObservedAt:   ev.ObservedAt,
		SourceNodeID: ev.SourceNodeID,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	method := s.Method
	if method == "" {
		method = http.MethodPost
	}
	t := s.Timeout
	if t <= 0 {
		t = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	hasContentType := false
	for k, v := range s.Headers {
		req.Header.Set(k, v)
		if http.CanonicalHeaderKey(k) == "Content-Type" {
			hasContentType = true
		}
	}
	if !hasContentType {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webhook status %d: %s", resp.StatusCode, string(preview))
	}
	return nil
}

func (s *WebhookAlertSink) logf(format string, args ...interface{}) {
	if s.Logger == nil {
		return
	}
	s.Logger.Printf(format, args...)
}

// webhookPayload is the JSON shape the sink emits. It mirrors
// billing.UsageEvent but uses string-typed fields so consumers
// (PagerDuty, Slack, generic ingest) do not need to know about
// internal types.
type webhookPayload struct {
	TenantID     string    `json:"tenant_id"`
	Bucket       string    `json:"bucket,omitempty"`
	Dimension    string    `json:"dimension"`
	Delta        uint64    `json:"delta"`
	ObservedAt   time.Time `json:"observed_at"`
	SourceNodeID string    `json:"source_node_id,omitempty"`
}

// MultiAlertSink fans an event out to every wrapped sink. Sinks
// are invoked sequentially in the order supplied; production wires
// the billing sink first (durable metering) followed by any
// auxiliary alert sinks (webhook, paging integrations).
type MultiAlertSink struct {
	Sinks []AlertSink
}

// NewMultiAlertSink returns a MultiAlertSink that emits to every
// non-nil sink. Passing zero non-nil sinks returns nil so callers
// can use the empty fan-out interchangeably with a missing sink.
func NewMultiAlertSink(sinks ...AlertSink) AlertSink {
	cleaned := make([]AlertSink, 0, len(sinks))
	for _, s := range sinks {
		if s == nil {
			continue
		}
		cleaned = append(cleaned, s)
	}
	switch len(cleaned) {
	case 0:
		return nil
	case 1:
		return cleaned[0]
	default:
		return &MultiAlertSink{Sinks: cleaned}
	}
}

// Emit forwards ev to every wrapped sink.
func (m *MultiAlertSink) Emit(ev billing.UsageEvent) {
	if m == nil {
		return
	}
	for _, s := range m.Sinks {
		s.Emit(ev)
	}
}
