// Latency histogram used by the sustained load runner.
//
// The histogram is HDR-style: each sample is decomposed into a
// log2 magnitude and a linear sub-bucket within that magnitude.
// This gives bounded relative error on every recorded value
// (1/subBucketCount, i.e. ~0.4% with the default 256 sub-buckets)
// while keeping the storage cost a small fixed slice regardless
// of sample count.
//
// The exported shape mirrors the JSON shape consumed by the
// load-test report renderer, so a downstream tool can deserialize
// it without re-deriving anything.
package benchmark

import (
	"encoding/json"
	"errors"
	"math"
	"math/bits"
	"sort"
	"time"
)

// histogram precision constants.
//
// subBucketCountPow chooses the linear precision per magnitude.
// 8 -> 256 sub-buckets per magnitude -> max relative error ~0.4%.
//
// magnitudeCount caps the largest representable value at
// (subBucketCount << magnitudeCount) - 1 nanoseconds. With the
// defaults below that is 2^(8+24) - 1 = ~4.29s, plenty of head
// room for any operation we care about. Samples above the cap are
// clamped to the last bucket (which is the right behaviour for a
// timeout: it is the highest observed bucket, not silently
// dropped).
const (
	subBucketCountPow = 8
	subBucketCount    = 1 << subBucketCountPow
	subBucketMask     = subBucketCount - 1
	magnitudeCount    = 24
	totalBuckets      = subBucketCount * magnitudeCount
)

// LatencyHistogram is the HDR-style histogram described above.
// The zero value is usable.
type LatencyHistogram struct {
	counts [totalBuckets]int64
	count  int64
	sumNS  int64
	maxNS  int64
	minNS  int64 // 0 means "no samples yet"; treat after Count > 0
}

// NewLatencyHistogram returns an empty histogram. Provided for
// callers that prefer an explicit constructor.
func NewLatencyHistogram() *LatencyHistogram { return &LatencyHistogram{} }

// Record adds one sample. Negative durations are clamped to zero
// so a clock skew does not poison the percentile output. The
// histogram is intended to be written by a single goroutine; the
// sustained runner gives each worker its own histogram and merges
// them at the end (see Merge).
func (h *LatencyHistogram) Record(d time.Duration) {
	if d < 0 {
		d = 0
	}
	ns := int64(d)
	idx := bucketIndex(ns)
	h.counts[idx]++
	h.count++
	h.sumNS += ns
	if ns > h.maxNS {
		h.maxNS = ns
	}
	if h.count == 1 || ns < h.minNS {
		h.minNS = ns
	}
}

// Count returns the total number of samples recorded.
func (h *LatencyHistogram) Count() int64 { return h.count }

// Mean returns the arithmetic mean latency. Zero if no samples.
func (h *LatencyHistogram) Mean() time.Duration {
	if h.count == 0 {
		return 0
	}
	return time.Duration(h.sumNS / h.count)
}

// Max returns the largest sample recorded. Zero if no samples.
func (h *LatencyHistogram) Max() time.Duration { return time.Duration(h.maxNS) }

// Min returns the smallest sample recorded. Zero if no samples.
func (h *LatencyHistogram) Min() time.Duration { return time.Duration(h.minNS) }

// Percentile returns the latency at the given percentile in [0,100].
// Values outside the range are clamped. The reported value is the
// upper bound of the bucket containing the sample, so the returned
// duration is a slight over-estimate (bounded by the histogram's
// per-bucket precision).
func (h *LatencyHistogram) Percentile(p float64) time.Duration {
	if h.count == 0 {
		return 0
	}
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	// "Rank" semantics matching the conventional p99 definition:
	// the smallest sample whose cumulative rank is >= p% of count.
	target := int64(math.Ceil(p / 100.0 * float64(h.count)))
	if target < 1 {
		target = 1
	}
	var seen int64
	for i, c := range h.counts {
		if c == 0 {
			continue
		}
		seen += c
		if seen >= target {
			return time.Duration(bucketUpperBound(i))
		}
	}
	return time.Duration(h.maxNS)
}

// Merge folds other into h. Other is unmodified. Used to combine
// per-worker histograms once the run finishes.
func (h *LatencyHistogram) Merge(other *LatencyHistogram) {
	if other == nil || other.count == 0 {
		return
	}
	for i, c := range other.counts {
		h.counts[i] += c
	}
	h.count += other.count
	h.sumNS += other.sumNS
	if other.maxNS > h.maxNS {
		h.maxNS = other.maxNS
	}
	// Use the receiver's pre-merge count to decide whether it had
	// no samples yet. h.minNS == 0 cannot be used as a sentinel
	// here because Record clamps negative durations to zero, so a
	// genuine 0ns sample is a possible legitimate minimum. The
	// fold-merge step has already incremented h.count above; track
	// the pre-merge value via the difference.
	hadNoSamples := (h.count - other.count) == 0
	if hadNoSamples || other.minNS < h.minNS {
		h.minNS = other.minNS
	}
}

// HistogramBucket is one entry in the JSON-serialised histogram.
// UpperBoundNS is the (inclusive) upper-bound nanoseconds of the
// bucket, Count is how many samples fell in it. Empty buckets are
// omitted from the JSON shape to keep reports compact.
type HistogramBucket struct {
	UpperBoundNS int64 `json:"upper_bound_ns"`
	Count        int64 `json:"count"`
}

// HistogramSummary is the serialisable view of the histogram. It
// is what gets embedded in the load-test report so a downstream
// tool can re-render or re-evaluate percentiles without re-running
// the load test.
type HistogramSummary struct {
	Count    int64             `json:"count"`
	MinNS    int64             `json:"min_ns"`
	MeanNS   int64             `json:"mean_ns"`
	MaxNS    int64             `json:"max_ns"`
	P50NS    int64             `json:"p50_ns"`
	P95NS    int64             `json:"p95_ns"`
	P99NS    int64             `json:"p99_ns"`
	P999NS   int64             `json:"p999_ns"`
	Buckets  []HistogramBucket `json:"buckets,omitempty"`
	Encoding string            `json:"encoding"`
}

// Summary materialises the histogram into the serialisable shape.
// Empty buckets are dropped from Buckets to keep the report compact.
func (h *LatencyHistogram) Summary() HistogramSummary {
	s := HistogramSummary{
		Count:    h.count,
		MinNS:    int64(h.Min()),
		MeanNS:   int64(h.Mean()),
		MaxNS:    int64(h.Max()),
		P50NS:    int64(h.Percentile(50)),
		P95NS:    int64(h.Percentile(95)),
		P99NS:    int64(h.Percentile(99)),
		P999NS:   int64(h.Percentile(99.9)),
		Encoding: "hdr-log2-subbucket-pow8",
	}
	for i, c := range h.counts {
		if c == 0 {
			continue
		}
		s.Buckets = append(s.Buckets, HistogramBucket{
			UpperBoundNS: bucketUpperBound(i),
			Count:        c,
		})
	}
	// Buckets are already in ascending order because counts is
	// indexed by ascending magnitude+sub-bucket; sort defensively
	// in case the layout ever changes.
	sort.Slice(s.Buckets, func(i, j int) bool {
		return s.Buckets[i].UpperBoundNS < s.Buckets[j].UpperBoundNS
	})
	return s
}

// MarshalJSON serialises through Summary so external consumers
// never depend on the internal counts layout.
func (h *LatencyHistogram) MarshalJSON() ([]byte, error) {
	return json.Marshal(h.Summary())
}

// UnmarshalJSON rebuilds the histogram from a Summary. Used by
// the report-replay tool and by tests round-tripping the JSON.
func (h *LatencyHistogram) UnmarshalJSON(data []byte) error {
	var s HistogramSummary
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s.Encoding != "" && s.Encoding != "hdr-log2-subbucket-pow8" {
		return errors.New("benchmark: unsupported histogram encoding: " + s.Encoding)
	}
	*h = LatencyHistogram{}
	for _, b := range s.Buckets {
		if b.Count <= 0 {
			continue
		}
		idx := indexForUpperBound(b.UpperBoundNS)
		h.counts[idx] += b.Count
		h.count += b.Count
		// Use the midpoint of the bucket as the per-sample weight
		// for sum-based statistics. This is the best we can do
		// when reconstructing from a summary; if the caller needs
		// the original mean they should round-trip via the Mean
		// field directly.
		mid := bucketMidpoint(idx)
		h.sumNS += mid * b.Count
	}
	h.minNS = s.MinNS
	h.maxNS = s.MaxNS
	return nil
}

// bucketIndex returns the bucket index for the given nanosecond
// value. The function follows the HDR layout: the magnitude is
// derived from the position of the highest set bit, and the
// sub-bucket is the next subBucketCountPow bits below that.
func bucketIndex(ns int64) int {
	if ns < 0 {
		ns = 0
	}
	// Values smaller than subBucketCount nanoseconds live in the
	// linear range of magnitude 0.
	if ns < subBucketCount {
		return int(ns)
	}
	// Determine magnitude: the most-significant bit position
	// relative to subBucketCountPow. The early return above for
	// ns < subBucketCount (== 1 << subBucketCountPow) guarantees
	// bitLen >= subBucketCountPow + 1 here, so mag is always >= 1
	// and the subsequent `ns >> (mag - 1)` is a non-negative
	// shift. No defensive `mag < 0` clamp: clamping to 0 would
	// turn line `ns >> (mag - 1)` into a negative-count shift
	// (panic in Go 1.13+), giving a false sense of safety. If a
	// future constants change ever broke the invariant, we want a
	// hard failure here surfaced by tests, not a silent clamp.
	bitLen := bits.Len64(uint64(ns))
	mag := bitLen - subBucketCountPow
	if mag >= magnitudeCount {
		return totalBuckets - 1
	}
	// Sub-bucket: drop the high bit (already accounted for by
	// magnitude) and take the next subBucketCountPow bits.
	sub := int((ns >> (mag - 1)) & subBucketMask)
	idx := mag*subBucketCount + sub
	if idx >= totalBuckets {
		return totalBuckets - 1
	}
	return idx
}

// bucketUpperBound returns the inclusive upper bound (in
// nanoseconds) of the bucket at index idx.
func bucketUpperBound(idx int) int64 {
	if idx <= 0 {
		return 0
	}
	if idx < subBucketCount {
		return int64(idx)
	}
	mag := idx / subBucketCount
	sub := idx % subBucketCount
	// Upper bound = ((subBucketCount | sub) << (mag - 1)) + step - 1
	// where step = 1 << (mag - 1).
	if mag < 1 {
		mag = 1
	}
	if mag >= magnitudeCount {
		mag = magnitudeCount - 1
	}
	step := int64(1) << (mag - 1)
	base := (int64(subBucketCount) | int64(sub)) << (mag - 1)
	return base + step - 1
}

// bucketMidpoint returns a representative nanosecond value for the
// bucket at index idx. Used only when reconstructing a histogram
// from its Summary.
func bucketMidpoint(idx int) int64 {
	upper := bucketUpperBound(idx)
	if idx == 0 {
		return upper
	}
	prev := bucketUpperBound(idx - 1)
	if prev < 0 {
		prev = 0
	}
	return (upper + prev) / 2
}

// indexForUpperBound returns the bucket whose UpperBoundNS equals
// the given value. Used by UnmarshalJSON to map a serialised
// bucket back into the internal layout.
func indexForUpperBound(upper int64) int {
	// The simplest reliable way is a binary search over the
	// monotonically increasing bucketUpperBound function.
	lo, hi := 0, totalBuckets-1
	for lo < hi {
		mid := (lo + hi) / 2
		if bucketUpperBound(mid) < upper {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
