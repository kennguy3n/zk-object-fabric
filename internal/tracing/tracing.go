// Package tracing exposes a tiny request-scoped tracing surface
// that the gateway uses to annotate S3 requests with structured
// span information.
//
// The implementation is deliberately minimal so the build does
// not pull in the OpenTelemetry SDK by default. Operators who
// want to export to Jaeger / OTLP can supply an Exporter from a
// thin adapter that converts our SpanRecord into otlp protobufs;
// the default NoopExporter is a non-allocating drop.
//
// Phase 4 wires this package into cmd/gateway via Middleware so
// every S3 request is wrapped in a Span. Subsequent phases will
// thread Span objects through the manifest store and provider
// calls; for now span attributes are limited to the HTTP request
// surface.
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// Exporter receives finished spans. NoopExporter is the default;
// a real OTLP / Jaeger exporter implements ExportSpan.
type Exporter interface {
	ExportSpan(span SpanRecord)
}

// SpanRecord is the immutable record of a finished span.
type SpanRecord struct {
	TraceID    string
	SpanID     string
	ParentID   string
	Name       string
	StartTime  time.Time
	EndTime    time.Time
	Attributes map[string]string
	Events     []SpanEvent
}

// SpanEvent is a timestamped annotation within a span (errors,
// log lines, retries, etc.).
type SpanEvent struct {
	Timestamp time.Time
	Name      string
	Fields    map[string]string
}

// Span is the live, mutable handle returned by Tracer.Start. Call
// AddEvent / SetAttribute during the request and End once the
// request is finished. End is idempotent.
type Span struct {
	mu        sync.Mutex
	record    SpanRecord
	tracer    *Tracer
	finished  bool
}

// SetAttribute attaches a key/value attribute to the span.
func (s *Span) SetAttribute(key, value string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record.Attributes == nil {
		s.record.Attributes = map[string]string{}
	}
	s.record.Attributes[key] = value
}

// AddEvent adds a timestamped event with optional fields.
func (s *Span) AddEvent(name string, fields map[string]string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record.Events = append(s.record.Events, SpanEvent{
		Timestamp: time.Now(),
		Name:      name,
		Fields:    fields,
	})
}

// End closes the span and ships it to the configured Exporter.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	s.record.EndTime = time.Now()
	if s.tracer != nil && s.tracer.Exporter != nil {
		s.tracer.Exporter.ExportSpan(s.record)
	}
}

// Record returns a snapshot of the span. Useful for tests.
func (s *Span) Record() SpanRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.record
	if s.record.Attributes != nil {
		cp.Attributes = map[string]string{}
		for k, v := range s.record.Attributes {
			cp.Attributes[k] = v
		}
	}
	return cp
}

// Tracer is the entry point for creating spans. A zero-value
// Tracer is usable: it produces no-op spans.
type Tracer struct {
	ServiceName string
	Exporter    Exporter
}

// New returns a Tracer with the given service name and exporter.
func New(serviceName string, exporter Exporter) *Tracer {
	if exporter == nil {
		exporter = NoopExporter{}
	}
	return &Tracer{ServiceName: serviceName, Exporter: exporter}
}

// Start opens a new span. Use the returned context to propagate
// the span through call sites.
func (t *Tracer) Start(ctx context.Context, name string) (context.Context, *Span) {
	if t == nil {
		return ctx, nil
	}
	parentID := ""
	traceID := ""
	if existing := SpanFromContext(ctx); existing != nil {
		parentID = existing.record.SpanID
		traceID = existing.record.TraceID
	}
	if traceID == "" {
		traceID = randomID(16)
	}
	span := &Span{
		tracer: t,
		record: SpanRecord{
			TraceID:   traceID,
			SpanID:    randomID(8),
			ParentID:  parentID,
			Name:      name,
			StartTime: time.Now(),
		},
	}
	return context.WithValue(ctx, spanKey{}, span), span
}

// Middleware wraps an http.Handler so every request is enclosed
// in a Span named "http.request" with method/path/status
// attributes.
func (t *Tracer) Middleware(next http.Handler) http.Handler {
	if t == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := t.Start(r.Context(), "http.request")
		span.SetAttribute("http.method", r.Method)
		span.SetAttribute("http.path", r.URL.Path)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			span.SetAttribute("http.status", statusToString(sw.status))
			span.End()
		}()
		next.ServeHTTP(sw, r.WithContext(ctx))
	})
}

// statusWriter captures the HTTP status code for span attribution.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func statusToString(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}

// SpanFromContext returns the active span, or nil when no span is
// in flight.
func SpanFromContext(ctx context.Context) *Span {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(spanKey{}).(*Span)
	return v
}

type spanKey struct{}

// NoopExporter discards finished spans. The default exporter when
// tracing is configured but no real backend is wired.
type NoopExporter struct{}

// ExportSpan implements Exporter.
func (NoopExporter) ExportSpan(SpanRecord) {}

// MemoryExporter records finished spans in memory. Used by tests
// to assert on span structure.
type MemoryExporter struct {
	mu    sync.Mutex
	Spans []SpanRecord
}

// ExportSpan appends the span to the in-memory buffer.
func (m *MemoryExporter) ExportSpan(span SpanRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Spans = append(m.Spans, span)
}

func randomID(byteLen int) string {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand never fails in practice; fall back to
		// a deterministic timestamp so we never panic.
		ts := time.Now().UnixNano()
		for i := range buf {
			buf[i] = byte(ts >> (i * 8))
		}
	}
	return hex.EncodeToString(buf)
}
