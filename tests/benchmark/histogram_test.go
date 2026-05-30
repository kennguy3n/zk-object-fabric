package benchmark

import (
	"encoding/json"
	"math/rand/v2"
	"sort"
	"testing"
	"time"
)

// TestHistogram_RecordPercentilesExactSmall verifies that for a
// small, well-known sample set every percentile we report falls
// within the histogram's per-bucket precision.
func TestHistogram_RecordPercentilesExactSmall(t *testing.T) {
	h := NewLatencyHistogram()
	samples := []time.Duration{
		1 * time.Microsecond,
		5 * time.Microsecond,
		10 * time.Microsecond,
		50 * time.Microsecond,
		100 * time.Microsecond,
		500 * time.Microsecond,
		1 * time.Millisecond,
		5 * time.Millisecond,
		10 * time.Millisecond,
		50 * time.Millisecond,
		100 * time.Millisecond,
		500 * time.Millisecond,
	}
	for _, s := range samples {
		h.Record(s)
	}
	if h.Count() != int64(len(samples)) {
		t.Fatalf("Count = %d, want %d", h.Count(), len(samples))
	}

	// Sort and verify each percentile is at or above the true
	// rank-based sample. The histogram returns the upper bound of
	// the containing bucket so we only assert the lower side.
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	checks := []struct {
		p     float64
		atOr  time.Duration
		label string
	}{
		{50, samples[len(samples)/2-1], "p50"},
		{95, samples[(len(samples)*95)/100-1], "p95"},
		{99, samples[len(samples)-1], "p99"},
	}
	for _, c := range checks {
		got := h.Percentile(c.p)
		if got < c.atOr {
			t.Errorf("%s = %v, want >= %v (got bucket upper bound)", c.label, got, c.atOr)
		}
	}
}

// TestHistogram_PercentileMonotonic ensures p50 <= p95 <= p99 <=
// p99.9 <= max for an arbitrary sample distribution.
func TestHistogram_PercentileMonotonic(t *testing.T) {
	h := NewLatencyHistogram()
	rng := rand.New(rand.NewPCG(42, 7))
	for i := 0; i < 10_000; i++ {
		// Lognormal-ish: most samples around 1ms, a long tail.
		ns := int64(rng.ExpFloat64() * float64(time.Millisecond))
		h.Record(time.Duration(ns))
	}
	p50 := h.Percentile(50)
	p95 := h.Percentile(95)
	p99 := h.Percentile(99)
	p999 := h.Percentile(99.9)
	if !(p50 <= p95 && p95 <= p99 && p99 <= p999 && p999 <= h.Max()) {
		t.Fatalf("percentiles not monotonic: p50=%v p95=%v p99=%v p999=%v max=%v",
			p50, p95, p99, p999, h.Max())
	}
}

// TestHistogram_RelativeError pins the worst-case bucket-bound
// precision. With subBucketCountPow=8 every reported value is
// within 1/256 (~0.4%) above the true sample. We use the 99.9
// percentile (target rank = 999 of 1000) so the tail value is
// the one that determines the reported value.
func TestHistogram_RelativeError(t *testing.T) {
	h := NewLatencyHistogram()
	const trueTail = 23456 * time.Microsecond
	for i := 0; i < 999; i++ {
		h.Record(1 * time.Millisecond)
	}
	h.Record(trueTail)
	got := h.Percentile(99.9)
	if got < trueTail {
		t.Fatalf("p99.9 = %v, want >= %v", got, trueTail)
	}
	rel := float64(got-trueTail) / float64(trueTail)
	if rel > 1.0/float64(subBucketCount) {
		t.Fatalf("p99.9 relative error %.4f exceeds 1/%d", rel, subBucketCount)
	}
}

// TestHistogram_MergePreservesCounts verifies that Merge does not
// drop or duplicate samples and that the merged percentile lines
// up with the percentile of the union sample set.
func TestHistogram_MergePreservesCounts(t *testing.T) {
	a := NewLatencyHistogram()
	b := NewLatencyHistogram()
	all := NewLatencyHistogram()
	rng := rand.New(rand.NewPCG(1, 1))
	for i := 0; i < 5_000; i++ {
		ns := int64(rng.ExpFloat64() * float64(time.Millisecond))
		d := time.Duration(ns)
		if i%2 == 0 {
			a.Record(d)
		} else {
			b.Record(d)
		}
		all.Record(d)
	}
	a.Merge(b)
	if a.Count() != all.Count() {
		t.Fatalf("merged count = %d, want %d", a.Count(), all.Count())
	}
	for _, p := range []float64{50, 95, 99} {
		if a.Percentile(p) != all.Percentile(p) {
			t.Errorf("p%v: merged=%v all=%v", p, a.Percentile(p), all.Percentile(p))
		}
	}
}

// TestHistogram_JSONRoundTrip checks that we can serialise and
// deserialise a histogram without losing the count or the
// percentiles. Sum-based statistics (Mean) are approximate after
// round-trip because the JSON shape stores the mean directly.
func TestHistogram_JSONRoundTrip(t *testing.T) {
	src := NewLatencyHistogram()
	rng := rand.New(rand.NewPCG(7, 7))
	for i := 0; i < 2_000; i++ {
		src.Record(time.Duration(rng.IntN(int(50 * time.Millisecond))))
	}
	data, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var dst LatencyHistogram
	if err := json.Unmarshal(data, &dst); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if dst.Count() != src.Count() {
		t.Fatalf("Count after round-trip = %d, want %d", dst.Count(), src.Count())
	}
	for _, p := range []float64{50, 95, 99} {
		if dst.Percentile(p) != src.Percentile(p) {
			t.Errorf("p%v: src=%v dst=%v", p, src.Percentile(p), dst.Percentile(p))
		}
	}
}

// TestHistogram_ClampsLargeSamples ensures very large values do
// not panic and are recorded as the maximum bucket.
func TestHistogram_ClampsLargeSamples(t *testing.T) {
	h := NewLatencyHistogram()
	h.Record(time.Hour) // beyond the histogram cap
	if h.Count() != 1 {
		t.Fatalf("Count = %d, want 1", h.Count())
	}
	if h.Percentile(50) < time.Second {
		t.Fatalf("p50 = %v, want >= 1s after clamp", h.Percentile(50))
	}
}

// TestHistogram_NegativeSamplesClampedToZero protects against
// clock-skew poisoning the percentile output.
func TestHistogram_NegativeSamplesClampedToZero(t *testing.T) {
	h := NewLatencyHistogram()
	h.Record(-time.Microsecond)
	h.Record(0)
	h.Record(time.Microsecond)
	if h.Min() != 0 {
		t.Fatalf("Min = %v, want 0 after clamp", h.Min())
	}
	if h.Count() != 3 {
		t.Fatalf("Count = %d, want 3", h.Count())
	}
}
