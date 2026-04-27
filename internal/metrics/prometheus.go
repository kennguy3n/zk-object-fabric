// Package metrics exposes a small Prometheus-text-format metrics
// surface for the gateway. The implementation is intentionally
// self-contained: it does not pull in github.com/prometheus/client_golang
// to keep the build's transitive dependency footprint small. The
// exposition format follows the Prometheus 0.0.4 plain-text spec
// (https://prometheus.io/docs/instrumenting/exposition_formats/).
//
// The set of metrics defined here mirrors the observability brief:
//
//   - zkof_request_duration_seconds (histogram, by method, status)
//   - zkof_cache_hit_total          (counter)
//   - zkof_cache_miss_total         (counter)
//   - zkof_dedup_hit_total          (counter)
//   - zkof_dedup_bytes_saved_total  (counter)
//   - zkof_provider_errors_total    (counter, by provider, operation)
//   - zkof_active_requests          (gauge)
//
// The exporter is goroutine-safe.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// DefaultBuckets is the histogram bucket layout used for request
// duration when callers do not override it. The values are seconds
// and roughly cover sub-millisecond reads through multi-second
// uploads.
var DefaultBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// Registry holds the gateway's metric set. Callers obtain a
// concrete reference via NewRegistry, register it with an HTTP
// server via Handler, and increment counters / observe histograms
// via the typed helpers.
type Registry struct {
	mu sync.Mutex

	// Per-(method,status) request duration histogram.
	requestDuration *Histogram

	// Counters keyed by sorted-label string for fast incrementing.
	cacheHits         atomic.Int64
	cacheMisses       atomic.Int64
	dedupHits         atomic.Int64
	dedupBytesSaved   atomic.Int64
	activeRequests    atomic.Int64
	providerErrors    *labeledCounter
}

// NewRegistry returns a registry initialised with the default
// gateway metric set.
func NewRegistry() *Registry {
	return &Registry{
		requestDuration: newHistogram("zkof_request_duration_seconds",
			"S3 request duration in seconds.",
			[]string{"method", "status"}, DefaultBuckets),
		providerErrors: newLabeledCounter("zkof_provider_errors_total",
			"Number of provider-side errors observed by the gateway.",
			[]string{"provider", "operation"}),
	}
}

// ObserveRequest records a single S3 request's duration with the
// given method and HTTP status code label.
func (r *Registry) ObserveRequest(method, status string, durationSeconds float64) {
	r.requestDuration.Observe(durationSeconds, []string{method, status})
}

// IncCacheHit / IncCacheMiss bump the cache outcome counters.
func (r *Registry) IncCacheHit()  { r.cacheHits.Add(1) }
func (r *Registry) IncCacheMiss() { r.cacheMisses.Add(1) }

// IncDedupHit and AddDedupBytesSaved update the dedup counters.
// Dedup hits count refcount-bumped PUTs; bytes saved is the total
// size that did not have to be uploaded a second time.
func (r *Registry) IncDedupHit()                    { r.dedupHits.Add(1) }
func (r *Registry) AddDedupBytesSaved(delta uint64) { r.dedupBytesSaved.Add(int64(delta)) }

// IncProviderError increments the per-provider/operation error
// counter.
func (r *Registry) IncProviderError(provider, operation string) {
	r.providerErrors.Inc([]string{provider, operation})
}

// IncActive / DecActive update the active-requests gauge. Pair
// each Inc with exactly one Dec on the request goroutine.
func (r *Registry) IncActive() { r.activeRequests.Add(1) }
func (r *Registry) DecActive() { r.activeRequests.Add(-1) }

// Handler returns an http.Handler that serves the metrics in the
// Prometheus text exposition format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = r.write(w)
	})
}

// write emits the full registry to w in Prometheus text format.
func (r *Registry) write(w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	emitCounter(w, "zkof_cache_hit_total", "Cache hits served from the gateway local cache.", r.cacheHits.Load())
	emitCounter(w, "zkof_cache_miss_total", "Cache misses that fell through to the origin.", r.cacheMisses.Load())
	emitCounter(w, "zkof_dedup_hit_total", "Dedup-aware PUTs that reused an existing piece.", r.dedupHits.Load())
	emitCounter(w, "zkof_dedup_bytes_saved_total", "Bytes that did not have to be uploaded thanks to dedup.", r.dedupBytesSaved.Load())
	emitGauge(w, "zkof_active_requests", "Currently in-flight S3 requests.", r.activeRequests.Load())
	r.providerErrors.write(w)
	r.requestDuration.write(w)
	return nil
}

// labeledCounter is a thread-safe map of label-value tuples to an
// integer counter.
type labeledCounter struct {
	name   string
	help   string
	labels []string

	mu     sync.Mutex
	values map[string]*atomic.Int64
}

func newLabeledCounter(name, help string, labels []string) *labeledCounter {
	return &labeledCounter{name: name, help: help, labels: labels, values: map[string]*atomic.Int64{}}
}

func (c *labeledCounter) Inc(labelValues []string) {
	key := joinLabelValues(labelValues)
	c.mu.Lock()
	v, ok := c.values[key]
	if !ok {
		v = new(atomic.Int64)
		c.values[key] = v
	}
	c.mu.Unlock()
	v.Add(1)
}

func (c *labeledCounter) write(w io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n", c.name, c.help)
	fmt.Fprintf(w, "# TYPE %s counter\n", c.name)
	keys := make([]string, 0, len(c.values))
	for k := range c.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		labelValues := splitLabelValues(k)
		fmt.Fprintf(w, "%s{%s} %d\n", c.name, formatLabels(c.labels, labelValues), c.values[k].Load())
	}
}

// Histogram is a fixed-bucket histogram. Bucket counts and the
// sum/count are tracked per label tuple.
type Histogram struct {
	name    string
	help    string
	labels  []string
	buckets []float64

	mu      sync.Mutex
	series  map[string]*histSeries
}

type histSeries struct {
	counts []uint64
	sum    float64
	count  uint64
}

func newHistogram(name, help string, labels []string, buckets []float64) *Histogram {
	cp := make([]float64, len(buckets))
	copy(cp, buckets)
	sort.Float64s(cp)
	return &Histogram{name: name, help: help, labels: labels, buckets: cp, series: map[string]*histSeries{}}
}

// Observe records a single sample.
func (h *Histogram) Observe(v float64, labelValues []string) {
	key := joinLabelValues(labelValues)
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.series[key]
	if !ok {
		s = &histSeries{counts: make([]uint64, len(h.buckets))}
		h.series[key] = s
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
		}
	}
	s.sum += v
	s.count++
}

func (h *Histogram) write(w io.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n", h.name, h.help)
	fmt.Fprintf(w, "# TYPE %s histogram\n", h.name)
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := h.series[k]
		labelValues := splitLabelValues(k)
		base := formatLabels(h.labels, labelValues)
		var cumulative uint64
		for i, b := range h.buckets {
			cumulative = s.counts[i]
			fmt.Fprintf(w, "%s_bucket{%s,le=\"%s\"} %d\n", h.name, base, formatFloat(b), cumulative)
		}
		fmt.Fprintf(w, "%s_bucket{%s,le=\"+Inf\"} %d\n", h.name, base, s.count)
		fmt.Fprintf(w, "%s_sum{%s} %s\n", h.name, base, formatFloat(s.sum))
		fmt.Fprintf(w, "%s_count{%s} %d\n", h.name, base, s.count)
	}
}

func emitCounter(w io.Writer, name, help string, v int64) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s counter\n", name)
	fmt.Fprintf(w, "%s %d\n", name, v)
}

func emitGauge(w io.Writer, name, help string, v int64) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s gauge\n", name)
	fmt.Fprintf(w, "%s %d\n", name, v)
}

// joinLabelValues serialises a label value tuple to a single
// string usable as a map key. Each value is escaped with the
// classic CSV-of-pipes convention so component values containing
// pipes round-trip safely.
func joinLabelValues(values []string) string {
	if len(values) == 0 {
		return ""
	}
	escaped := make([]string, len(values))
	for i, v := range values {
		escaped[i] = strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `|`, `\|`)
	}
	return strings.Join(escaped, "|")
}

func splitLabelValues(key string) []string {
	if key == "" {
		return nil
	}
	// Split on unescaped pipes.
	var out []string
	var cur strings.Builder
	for i := 0; i < len(key); i++ {
		if key[i] == '\\' && i+1 < len(key) {
			cur.WriteByte(key[i+1])
			i++
			continue
		}
		if key[i] == '|' {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(key[i])
	}
	out = append(out, cur.String())
	return out
}

func formatLabels(names, values []string) string {
	pairs := make([]string, 0, len(names))
	for i, n := range names {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		pairs = append(pairs, fmt.Sprintf(`%s="%s"`, n, escapeLabelValue(v)))
	}
	return strings.Join(pairs, ",")
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return v
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
