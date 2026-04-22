// This file implements the concrete Runner that Workstream 8
// requires: a driver that executes the Scenarios declared in
// suite.go against any providers.StorageProvider, records the
// requested metrics, and emits a JSON report for CI consumption.
//
// The runner intentionally keeps scope tight: it measures PUT/GET
// latency, LIST throughput, cache hit ratio, and Wasabi origin
// egress ratio. Longer-horizon metrics (repair time, network cost)
// are reported as labelled Results so a later driver can populate
// them without changing the report schema.
package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sort"
	"sync/atomic"
	"time"

	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// ProviderRunner is a concrete implementation of Runner. It drives
// the Scenario's request mix against a single StorageProvider and
// records the metrics declared on the scenario's Targets.
//
// Optional: supply a HotObjectCache to measure cache hit ratio. If
// nil, cache-related metrics are reported as 0 and skipped in the
// evaluation.
type ProviderRunner struct {
	Provider providers.StorageProvider
	Cache    hot_object_cache.HotObjectCache

	// WasabiStoredBytes and WasabiEgressBytes let the runner report
	// the origin-egress ratio without re-deriving it from the raw
	// workload. A real operator wires these into the billing
	// pipeline; tests pre-populate them.
	WasabiStoredBytes int64
	WasabiEgressBytes int64

	// Now overrides time.Now for deterministic tests. Optional.
	Now func() time.Time
}

// NewProviderRunner returns a runner for provider.
func NewProviderRunner(provider providers.StorageProvider) *ProviderRunner {
	return &ProviderRunner{Provider: provider, Now: time.Now}
}

// Run executes one scenario and returns a Result for every Target
// declared on the scenario plus one labelled Result per scenario-
// level counter (e.g. total_requests). Unknown metrics return a
// zeroed Result so CI can pick them up as "not yet measured".
func (r *ProviderRunner) Run(scenario Scenario) ([]Result, error) {
	if r.Provider == nil {
		return nil, errors.New("benchmark: ProviderRunner.Provider is required")
	}
	if err := scenario.validate(); err != nil {
		return nil, fmt.Errorf("benchmark: scenario %q: %w", scenario.Name, err)
	}
	now := r.nowFn()

	putLats, getLats, listLats, hits, misses, err := r.generateLoad(scenario, now)
	if err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(scenario.Targets))
	for _, t := range scenario.Targets {
		res := Result{
			Metric: t.Metric,
			Labels: map[string]string{"scenario": scenario.Name},
		}
		switch t.Metric {
		case MetricPutP50:
			res.Value = percentileMS(putLats, 50)
		case MetricPutP95:
			res.Value = percentileMS(putLats, 95)
		case MetricPutP99:
			res.Value = percentileMS(putLats, 99)
		case MetricGetP50:
			res.Value = percentileMS(getLats, 50)
		case MetricGetP95:
			res.Value = percentileMS(getLats, 95)
		case MetricGetP99:
			res.Value = percentileMS(getLats, 99)
		case MetricListP95:
			res.Value = percentileMS(listLats, 95)
		case MetricCacheHitRatioHot:
			total := hits + misses
			if total > 0 {
				res.Value = float64(hits) / float64(total)
			}
		case MetricWasabiOriginEgressRatio:
			if r.WasabiStoredBytes > 0 {
				res.Value = float64(r.WasabiEgressBytes) / float64(r.WasabiStoredBytes)
			}
		case MetricMigrationThroughput, MetricRepairTimeSeconds, MetricNetworkCostUSDPerTB:
			// Reported as zero-valued placeholders for CI to track
			// until the control-plane driver wires them in.
		}
		results = append(results, res)
	}
	return results, nil
}

// generateLoad drives one scenario's request mix against the
// provider and returns per-operation latency samples plus cache
// hit/miss counters.
func (r *ProviderRunner) generateLoad(scenario Scenario, now func() time.Time) (put, get, list []time.Duration, hits, misses int64, err error) {
	rng := rand.New(rand.NewPCG(1, 2))
	ctx := context.Background()

	size := scenario.Workload.ObjectSizeBytes
	if size <= 0 {
		size = 1024
	}
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}

	totalRequests := scenario.Workload.TargetRPS * scenario.Workload.DurationSeconds
	if totalRequests <= 0 {
		totalRequests = 1
	}
	// Runner is a pure unit test; cap the iteration count so a
	// 24h scenario with TargetRPS=2000 does not execute 172.8M
	// iterations against local_fs_dev. The real load driver will
	// read the declared totals directly.
	if totalRequests > 256 {
		totalRequests = 256
	}

	// Pre-seed some keys so GET and LIST have something to hit.
	seeded := []string{}
	for i := 0; i < 8; i++ {
		pieceID := fmt.Sprintf("seed-%d", i)
		if _, perr := r.Provider.PutPiece(ctx, pieceID, bytes.NewReader(payload), providers.PutOptions{
			ContentLength: int64(len(payload)),
			ContentType:   "application/octet-stream",
		}); perr != nil {
			return nil, nil, nil, 0, 0, fmt.Errorf("benchmark: seed put: %w", perr)
		}
		seeded = append(seeded, pieceID)
	}

	ops := flattenMix(scenario.Workload.RequestMix)
	for i := 0; i < totalRequests; i++ {
		op := ops[rng.IntN(len(ops))]
		switch op {
		case "PUT":
			pieceID := fmt.Sprintf("bench-%s-%d-%d", scenario.Name, now().UnixNano(), i)
			t0 := now()
			if _, perr := r.Provider.PutPiece(ctx, pieceID, bytes.NewReader(payload), providers.PutOptions{
				ContentLength: int64(len(payload)),
			}); perr != nil {
				return nil, nil, nil, 0, 0, fmt.Errorf("benchmark: PUT: %w", perr)
			}
			put = append(put, now().Sub(t0))
			seeded = append(seeded, pieceID)
		case "GET":
			if len(seeded) == 0 {
				continue
			}
			pieceID := seeded[rng.IntN(len(seeded))]
			t0 := now()
			hit, gerr := r.getOnce(ctx, pieceID)
			if gerr != nil {
				return nil, nil, nil, 0, 0, fmt.Errorf("benchmark: GET: %w", gerr)
			}
			get = append(get, now().Sub(t0))
			if hit {
				hits++
			} else {
				misses++
			}
		case "HEAD":
			if len(seeded) == 0 {
				continue
			}
			pieceID := seeded[rng.IntN(len(seeded))]
			if _, herr := r.Provider.HeadPiece(ctx, pieceID); herr != nil {
				return nil, nil, nil, 0, 0, fmt.Errorf("benchmark: HEAD: %w", herr)
			}
		case "DELETE":
			if len(seeded) == 0 {
				continue
			}
			idx := rng.IntN(len(seeded))
			pieceID := seeded[idx]
			if derr := r.Provider.DeletePiece(ctx, pieceID); derr != nil {
				continue
			}
			seeded = append(seeded[:idx], seeded[idx+1:]...)
		case "LIST":
			t0 := now()
			if _, lerr := r.Provider.ListPieces(ctx, "", ""); lerr != nil {
				return nil, nil, nil, 0, 0, fmt.Errorf("benchmark: LIST: %w", lerr)
			}
			list = append(list, now().Sub(t0))
		}
	}
	return put, get, list, hits, misses, nil
}

// getOnce performs one GET, consulting the cache first when one is
// configured. Returns (true, nil) on a cache hit.
func (r *ProviderRunner) getOnce(ctx context.Context, pieceID string) (bool, error) {
	if r.Cache != nil {
		rc, _, err := r.Cache.Get(ctx, pieceID)
		if err == nil {
			_, _ = io.Copy(io.Discard, rc)
			_ = rc.Close()
			return true, nil
		}
	}
	rc, err := r.Provider.GetPiece(ctx, pieceID, nil)
	if err != nil {
		return false, err
	}
	defer rc.Close()
	buf, err := io.ReadAll(rc)
	if err != nil {
		return false, err
	}
	if r.Cache != nil {
		_ = r.Cache.Put(ctx, pieceID, bytes.NewReader(buf), hot_object_cache.PutOptions{
			SizeBytes: int64(len(buf)),
		})
	}
	return false, nil
}

func (r *ProviderRunner) nowFn() func() time.Time {
	if r.Now != nil {
		return r.Now
	}
	return time.Now
}

// flattenMix expands a RequestMix map into a slice whose entries
// appear in rough proportion to their fractional weights, so a
// uniform random pick from the slice approximates the mix.
func flattenMix(mix map[string]float64) []string {
	if len(mix) == 0 {
		return []string{"PUT"}
	}
	const bucketSize = 100
	ops := make([]string, 0, bucketSize)
	// Deterministic iteration so two runs with the same mix produce
	// the same op sequence when the RNG is seeded identically.
	keys := make([]string, 0, len(mix))
	for k := range mix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n := int(mix[k]*float64(bucketSize) + 0.5)
		for i := 0; i < n; i++ {
			ops = append(ops, k)
		}
	}
	if len(ops) == 0 {
		ops = append(ops, keys[0])
	}
	return ops
}

// percentileMS returns the p-th percentile of the samples in
// milliseconds. Empty input returns 0.
func percentileMS(samples []time.Duration, p int) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (p * (len(sorted) - 1)) / 100
	return float64(sorted[idx]) / float64(time.Millisecond)
}

// ReportScenario is one scenario's results in the output JSON.
type ReportScenario struct {
	Name      string   `json:"name"`
	TotalPuts int      `json:"total_puts"`
	TotalGets int      `json:"total_gets"`
	Results   []Result `json:"results"`
	Pass      bool     `json:"pass"`
	Failures  []string `json:"failures,omitempty"`
}

// Report is the top-level JSON emitted by RunSuite. The shape is
// stable so CI can assert against specific metrics without parsing
// free-form text.
type Report struct {
	Suite        string            `json:"suite"`
	StartedAt    time.Time         `json:"started_at"`
	FinishedAt   time.Time         `json:"finished_at"`
	Scenarios    []ReportScenario  `json:"scenarios"`
	AllPassed    bool              `json:"all_passed"`
	ExtraMetrics map[string]string `json:"extra_metrics,omitempty"`
}

// RunSuite executes every scenario in suite through runner and
// returns a Report. The runner's internal counters and per-
// scenario Results are bundled into a structure ready to be marshalled
// with (*Report).ToJSON.
func RunSuite(suite Suite, runner Runner) (*Report, error) {
	if err := suite.Validate(); err != nil {
		return nil, err
	}
	rep := &Report{
		Suite:     suite.Name,
		StartedAt: time.Now(),
		AllPassed: true,
	}
	for _, sc := range suite.Scenarios {
		results, err := runner.Run(sc)
		if err != nil {
			return nil, fmt.Errorf("benchmark: scenario %q: %w", sc.Name, err)
		}
		reportSc := ReportScenario{
			Name:    sc.Name,
			Results: results,
			Pass:    true,
		}
		for i, t := range sc.Targets {
			if i >= len(results) {
				break
			}
			if msg := EvaluateTarget(t, results[i].Value); msg != "" {
				reportSc.Pass = false
				reportSc.Failures = append(reportSc.Failures, msg)
			}
		}
		if !reportSc.Pass {
			rep.AllPassed = false
		}
		rep.Scenarios = append(rep.Scenarios, reportSc)
	}
	rep.FinishedAt = time.Now()
	return rep, nil
}

// ToJSON renders the report as indented JSON suitable for CI
// artifact publishing.
func (r *Report) ToJSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// monotonicCounter is an atomic counter used internally by the
// runner when concurrent drivers are added in a later phase. It is
// exposed to the package so shared helpers can grow against a single
// accounting primitive.
type monotonicCounter struct{ n atomic.Int64 }

func (c *monotonicCounter) Add(delta int64) int64 { return c.n.Add(delta) }
func (c *monotonicCounter) Load() int64           { return c.n.Load() }
