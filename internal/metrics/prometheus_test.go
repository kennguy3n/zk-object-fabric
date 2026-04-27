package metrics

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/billing"
)

func TestRegistry_BasicCounters(t *testing.T) {
	r := NewRegistry()
	r.IncCacheHit()
	r.IncCacheHit()
	r.IncCacheMiss()
	r.IncDedupHit()
	r.AddDedupBytesSaved(1024)
	r.IncProviderError("wasabi", "GetPiece")
	r.IncProviderError("wasabi", "GetPiece")
	r.IncProviderError("aws_s3", "PutPiece")
	r.IncActive()
	r.IncActive()
	r.DecActive()
	r.ObserveRequest("GET", "2xx", 0.012)
	r.ObserveRequest("GET", "2xx", 0.5)
	r.ObserveRequest("PUT", "5xx", 1.2)

	var buf bytes.Buffer
	if err := r.write(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"zkof_cache_hit_total 2",
		"zkof_cache_miss_total 1",
		"zkof_dedup_hit_total 1",
		"zkof_dedup_bytes_saved_total 1024",
		"zkof_active_requests 1",
		`zkof_provider_errors_total{provider="wasabi",operation="GetPiece"} 2`,
		`zkof_provider_errors_total{provider="aws_s3",operation="PutPiece"} 1`,
		`zkof_request_duration_seconds_bucket{method="GET",status="2xx",le="+Inf"} 2`,
		`zkof_request_duration_seconds_count{method="GET",status="2xx"} 2`,
		`zkof_request_duration_seconds_count{method="PUT",status="5xx"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, out)
		}
	}
	// Histogram cumulative property: bucket counts non-decreasing.
	prev := 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "zkof_request_duration_seconds_bucket{method=\"GET\"") {
			continue
		}
		// extract the trailing integer.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var n int
		_, err := fmtSscanf(fields[len(fields)-1], &n)
		if err != nil {
			continue
		}
		if n < prev {
			t.Errorf("histogram bucket counts decreased %d -> %d in %q", prev, n, line)
		}
		prev = n
	}
}

// fmtSscanf is a tiny stub to avoid pulling in fmt in the helper.
func fmtSscanf(s string, n *int) (int, error) {
	var v int
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errParse
		}
		v = v*10 + int(c-'0')
	}
	*n = v
	return 1, nil
}

var errParse = &parseErr{}

type parseErr struct{}

func (*parseErr) Error() string { return "not int" }

func TestRegistry_Handler_ServesPromText(t *testing.T) {
	r := NewRegistry()
	r.IncCacheHit()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/internal/metrics", nil)
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "zkof_cache_hit_total 1") {
		t.Errorf("body missing counter:\n%s", rec.Body.String())
	}
}

type recordingSink struct{ events []billing.UsageEvent }

func (r *recordingSink) Emit(e billing.UsageEvent) { r.events = append(r.events, e) }

func TestMetricsBillingSink_ForwardsAndCounts(t *testing.T) {
	r := NewRegistry()
	inner := &recordingSink{}
	wrap := NewMetricsBillingSink(inner, r)

	wrap.Emit(billing.UsageEvent{Dimension: billing.CacheHits, Delta: 1})
	wrap.Emit(billing.UsageEvent{Dimension: billing.CacheMisses, Delta: 1})
	wrap.Emit(billing.UsageEvent{Dimension: billing.DedupHits, Delta: 1})
	wrap.Emit(billing.UsageEvent{Dimension: billing.DedupBytesSaved, Delta: 4096})
	wrap.Emit(billing.UsageEvent{Dimension: billing.PutRequests, Delta: 1})

	if len(inner.events) != 5 {
		t.Fatalf("inner forwarded %d events, want 5", len(inner.events))
	}
	var buf bytes.Buffer
	_ = r.write(&buf)
	for _, want := range []string{
		"zkof_cache_hit_total 1",
		"zkof_cache_miss_total 1",
		"zkof_dedup_hit_total 1",
		"zkof_dedup_bytes_saved_total 4096",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing %q in:\n%s", want, buf.String())
		}
	}
}
