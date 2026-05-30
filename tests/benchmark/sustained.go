// Sustained load runner.
//
// This file implements the production-grade load driver
// referenced in docs/PROGRESS.md "Production Readiness > Load
// testing". Unlike the unit-test-sized ProviderRunner in
// runner.go (which is capped at 256 requests for fast CI), the
// SustainedRunner drives the full requested workload at the
// declared TargetRPS for the declared DurationSeconds against a
// real providers.StorageProvider.
//
// Workload shape:
//
//   * N worker goroutines pull tokens from a shared rate limiter
//     and execute one S3-style operation per token.
//   * Operation choice is sampled from the Scenario's RequestMix.
//   * Latencies are recorded into per-worker LatencyHistograms
//     and merged into the per-operation aggregate at run-end, so
//     workers never contend on a shared lock during measurement.
//   * Pre-seeding populates the provider with the requested
//     number of objects before the steady-state run begins so
//     GET / HEAD / DELETE / LIST have something to hit.
//
// The runner does not invent metric values: every metric the
// scenario asks for is either computed from the recorded sample
// stream (latency percentiles, sustained throughput, cache hit
// ratio) or marked observed=false in the Result so a downstream
// reviewer knows the harness did not yet have a wire-up for it.
package benchmark

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// SustainedRunner is the production-grade load driver. The zero
// value is not valid; use NewSustainedRunner.
type SustainedRunner struct {
	Provider providers.StorageProvider
	Cache    hot_object_cache.HotObjectCache

	// Concurrency is the number of worker goroutines. 0 -> auto
	// (8 + TargetRPS/250, capped at 512).
	Concurrency int

	// SeedObjects is the number of pieces to pre-populate before
	// the steady-state run begins. 0 -> max(64, TargetRPS/10).
	// Pre-seeded keys are reused as the working set for GET, HEAD
	// and DELETE operations.
	SeedObjects int

	// DurationOverride, when > 0, replaces the scenario's declared
	// duration. Used by CI to run a long-form scenario in a few
	// seconds.
	DurationOverride time.Duration

	// RPSOverride, when > 0, replaces the scenario's declared
	// TargetRPS. Used by CI smoke runs.
	RPSOverride int

	// MaxObjectSizeBytes, when > 0, caps the scenario's
	// declared ObjectSizeBytes. Used by CI to keep payload
	// memory bounded without rewriting the scenario.
	MaxObjectSizeBytes int64

	// Now overrides time.Now for measurement timestamps in tests.
	// This affects scenario start/finish timestamps and per-operation
	// latency measurement only. Run-duration enforcement uses
	// context.WithTimeout, which is wall-clock based regardless of
	// this field; a mock clock here will not shorten or lengthen the
	// drive phase. Optional.
	Now func() time.Time

	// RNGSeed seeds the deterministic op-choice RNG so two runs
	// at the same scenario produce the same op sequence.
	RNGSeed uint64

	// FailureLimit caps the number of *consecutive* request errors
	// before the run aborts. "Consecutive" is fleet-wide: any
	// successful request from any worker resets the counter, so
	// the limit trips only when no worker has succeeded in the
	// last FailureLimit requests. 0 -> 64.
	FailureLimit int

	// Ctx, if non-nil, parents the run's internal context. A
	// caller (e.g. the CLI signal handler) can cancel a long
	// scenario by cancelling this context. When nil, the run
	// uses context.Background() so the existing Runner interface
	// stays backward compatible.
	Ctx context.Context
}

// NewSustainedRunner returns a runner for provider.
func NewSustainedRunner(provider providers.StorageProvider) *SustainedRunner {
	return &SustainedRunner{Provider: provider}
}

// Run implements Runner.
func (r *SustainedRunner) Run(scenario Scenario) ([]Result, error) {
	if r.Provider == nil {
		return nil, errors.New("benchmark: SustainedRunner.Provider is required")
	}
	if err := scenario.validate(); err != nil {
		return nil, fmt.Errorf("benchmark: scenario %q: %w", scenario.Name, err)
	}

	parent := r.Ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	cfg, err := r.effectiveWorkload(scenario.Workload)
	if err != nil {
		return nil, fmt.Errorf("benchmark: scenario %q: %w", scenario.Name, err)
	}

	payload := makePayload(cfg.objectSize)
	seeded, err := r.seedObjects(ctx, scenario.Name, payload, cfg)
	if err != nil {
		return nil, fmt.Errorf("benchmark: scenario %q seed: %w", scenario.Name, err)
	}

	aggregate := newAggregate()
	startedAt := r.nowFn()()
	err = r.drive(ctx, scenario.Name, payload, cfg, seeded, aggregate)
	finishedAt := r.nowFn()()
	if err != nil {
		return nil, fmt.Errorf("benchmark: scenario %q drive: %w", scenario.Name, err)
	}

	results := r.buildResults(scenario, cfg, aggregate, finishedAt.Sub(startedAt))
	return results, nil
}

// effectiveWorkload returns the actual workload knobs the runner
// will use after applying CI overrides and defaults.
func (r *SustainedRunner) effectiveWorkload(w Workload) (runConfig, error) {
	out := runConfig{
		requestMix:   w.RequestMix,
		objectSize:   w.ObjectSizeBytes,
		tenantCount:  w.TenantCount,
		duration:     time.Duration(w.DurationSeconds) * time.Second,
		targetRPS:    w.TargetRPS,
		listObjects:  w.ListObjectCount,
		dedupHitFrac: w.DedupHitFraction,
		rangeFrac:    w.RangeGETFraction,
	}
	if r.DurationOverride > 0 {
		out.duration = r.DurationOverride
	}
	if r.RPSOverride > 0 {
		out.targetRPS = r.RPSOverride
	}
	if r.MaxObjectSizeBytes > 0 && out.objectSize > r.MaxObjectSizeBytes {
		out.objectSize = r.MaxObjectSizeBytes
	}
	if out.objectSize <= 0 {
		out.objectSize = 1024
	}
	if out.targetRPS <= 0 {
		return out, errors.New("workload.target_rps must be > 0")
	}
	if out.duration <= 0 {
		return out, errors.New("workload.duration must be > 0")
	}
	if out.tenantCount <= 0 {
		out.tenantCount = 1
	}
	out.concurrency = r.Concurrency
	if out.concurrency <= 0 {
		out.concurrency = 8 + out.targetRPS/250
		if out.concurrency > 512 {
			out.concurrency = 512
		}
	}
	out.seedObjects = r.SeedObjects
	if out.seedObjects <= 0 {
		out.seedObjects = out.targetRPS / 10
		if out.seedObjects < 64 {
			out.seedObjects = 64
		}
	}
	out.failureLimit = r.FailureLimit
	if out.failureLimit <= 0 {
		out.failureLimit = 64
	}
	return out, nil
}

type runConfig struct {
	requestMix   map[string]float64
	objectSize   int64
	tenantCount  int
	duration     time.Duration
	targetRPS    int
	listObjects  int64
	dedupHitFrac float64
	rangeFrac    float64
	concurrency  int
	seedObjects  int
	failureLimit int
}

func makePayload(size int64) []byte {
	if size <= 0 {
		size = 1024
	}
	buf := make([]byte, size)
	// Fill with a deterministic, non-trivial bit pattern. This is
	// the same payload for every PUT in the run; uniqueness comes
	// from the object key, not the body. That is the correct shape
	// for raw provider-I/O benchmarking. It is NOT suitable for
	// content-based dedup benchmarking, which would need
	// per-operation byte variation. The dedup scenarios in
	// DefaultSuite mark their dedup-specific metrics Pending for
	// exactly this reason.
	for i := range buf {
		buf[i] = byte(i*1103515245 + 12345)
	}
	return buf
}

// seedObjects pre-populates the provider so GET/HEAD/DELETE/LIST
// have something to operate on. Each piece is the same payload
// with a distinct ID so the working set is deterministic.
func (r *SustainedRunner) seedObjects(ctx context.Context, scenario string, payload []byte, cfg runConfig) ([]string, error) {
	keys := make([]string, 0, cfg.seedObjects)
	for i := 0; i < cfg.seedObjects; i++ {
		key := fmt.Sprintf("bench-seed-%s-%d", scenario, i)
		if _, err := r.Provider.PutPiece(ctx, key, bytes.NewReader(payload), providers.PutOptions{
			ContentLength: int64(len(payload)),
			ContentType:   "application/octet-stream",
		}); err != nil {
			return nil, fmt.Errorf("seed put %q: %w", key, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// drive spins up the worker pool and runs until cfg.duration
// elapses or the failure limit trips.
func (r *SustainedRunner) drive(ctx context.Context, scenario string, payload []byte, cfg runConfig, seeded []string, agg *aggregate) error {
	// Run-duration is a wall-clock budget. context.WithTimeout
	// uses time.Now() internally; do not substitute r.nowFn()
	// here because the context library will still compare against
	// the real wall clock and a mock-time deadline would either
	// expire instantly or never. r.nowFn() is reserved for
	// measurement timestamps (see the Now field doc).
	ctx, cancel := context.WithTimeout(ctx, cfg.duration)
	defer cancel()

	limiter := newTokenBucket(cfg.targetRPS)
	limiter.start(ctx)

	workers := cfg.concurrency
	var wg sync.WaitGroup
	wg.Add(workers)

	// Worker-local histograms keep the hot path lock-free.
	perWorker := make([]*workerState, workers)
	for i := range perWorker {
		perWorker[i] = newWorkerState(cfg.targetRPS, uint64(i), r.RNGSeed)
	}

	// Shared mutable working set: workers append on PUT and read on
	// GET/HEAD/DELETE. A mutex is acceptable because the protected
	// region is a slice-index pick or append, not the I/O itself.
	workingSet := &keySet{keys: append([]string(nil), seeded...)}

	// consecutive tracks fleet-wide consecutive errors: any
	// success resets it to 0, any error increments it. When the
	// counter reaches cfg.failureLimit the run aborts. The race
	// where a success in worker A clobbers an error increment in
	// worker B is acceptable: the worst case is a slightly
	// delayed abort, which is the safe direction for a circuit
	// breaker. tripped is set once when the limit is hit so the
	// caller can distinguish "failure limit reached" from "clean
	// shutdown".
	var consecutive atomic.Int64
	var tripped atomic.Bool

	for i := 0; i < workers; i++ {
		state := perWorker[i]
		go func() {
			defer wg.Done()
			for {
				if !limiter.acquire(ctx) {
					return
				}
				op := state.nextOp(cfg.requestMix)
				start := r.nowFn()()
				err := r.executeOnce(ctx, scenario, op, payload, cfg, state, workingSet)
				dur := r.nowFn()().Sub(start)
				state.record(op, dur, err)
				if err != nil {
					if consecutive.Add(1) >= int64(cfg.failureLimit) {
						tripped.Store(true)
						cancel()
						return
					}
				} else {
					consecutive.Store(0)
				}
			}
		}()
	}

	wg.Wait()

	for _, ws := range perWorker {
		agg.merge(ws)
	}
	if tripped.Load() {
		return fmt.Errorf("benchmark: consecutive failure limit %d reached", cfg.failureLimit)
	}
	return nil
}

// executeOnce performs one operation. It is intentionally simple:
// the goal is to measure the wire latency, not to model client
// retry policies.
func (r *SustainedRunner) executeOnce(ctx context.Context, scenario, op string, payload []byte, cfg runConfig, state *workerState, ks *keySet) error {
	switch op {
	case "PUT":
		key := fmt.Sprintf("bench-%s-w%d-%d", scenario, state.id, state.seq.Add(1))
		_, err := r.Provider.PutPiece(ctx, key, bytes.NewReader(payload), providers.PutOptions{
			ContentLength: int64(len(payload)),
			ContentType:   "application/octet-stream",
		})
		if err != nil {
			return err
		}
		ks.add(key)
		return nil
	case "GET":
		key, ok := ks.pick(state.rng)
		if !ok {
			return nil
		}
		if r.Cache != nil {
			rc, _, cerr := r.Cache.Get(ctx, key)
			if cerr == nil {
				_, _ = io.Copy(io.Discard, rc)
				_ = rc.Close()
				state.cacheHit++
				return nil
			}
		}
		rc, err := r.Provider.GetPiece(ctx, key, byteRangeFor(cfg.rangeFrac, int64(len(payload)), state.rng))
		if err != nil {
			return err
		}
		defer rc.Close()
		buf, err := io.ReadAll(rc)
		if err != nil {
			return err
		}
		if r.Cache != nil {
			_ = r.Cache.Put(ctx, key, bytes.NewReader(buf), hot_object_cache.PutOptions{
				SizeBytes: int64(len(buf)),
			})
		}
		state.cacheMiss++
		return nil
	case "HEAD":
		key, ok := ks.pick(state.rng)
		if !ok {
			return nil
		}
		_, err := r.Provider.HeadPiece(ctx, key)
		return err
	case "DELETE":
		key, ok := ks.popOne(state.rng)
		if !ok {
			return nil
		}
		return r.Provider.DeletePiece(ctx, key)
	case "LIST":
		_, err := r.Provider.ListPieces(ctx, "", "")
		return err
	default:
		return fmt.Errorf("benchmark: unsupported op %q", op)
	}
}

// byteRangeFor returns a *providers.ByteRange for the given
// fraction of the payload, or nil for a full-object read.
func byteRangeFor(rangeFrac float64, size int64, rng *rand.Rand) *providers.ByteRange {
	if rangeFrac <= 0 || size <= 0 {
		return nil
	}
	if rng.Float64() >= rangeFrac {
		return nil
	}
	// Read a random window of up to 16 KiB or 25% of the payload.
	max := size / 4
	if max <= 0 {
		max = size
	}
	if max > 16*1024 {
		max = 16 * 1024
	}
	width := rng.Int64N(max) + 1
	start := rng.Int64N(size - width + 1)
	return &providers.ByteRange{Start: start, End: start + width - 1}
}

// buildResults turns the aggregate into one Result per Target.
func (r *SustainedRunner) buildResults(scenario Scenario, cfg runConfig, agg *aggregate, elapsed time.Duration) []Result {
	results := make([]Result, 0, len(scenario.Targets))
	// Use attempts (success + failure) as the denominator for
	// throughput and error-rate, not the histogram counts, since
	// histograms exclude failed requests by design (see
	// workerState.record).
	totalRequests := agg.attempts
	attainedRPS := 0.0
	if elapsed > 0 {
		attainedRPS = float64(totalRequests) / elapsed.Seconds()
	}
	cacheTotal := agg.cacheHit + agg.cacheMiss
	for _, t := range scenario.Targets {
		res := Result{
			Metric: t.Metric,
			Labels: map[string]string{
				"scenario":     scenario.Name,
				"unit":         t.Unit,
				"target_rps":   fmt.Sprintf("%d", cfg.targetRPS),
				"concurrency":  fmt.Sprintf("%d", cfg.concurrency),
				"object_bytes": fmt.Sprintf("%d", cfg.objectSize),
			},
		}
		switch t.Metric {
		case MetricPutP50:
			res.Value = float64(agg.put.Percentile(50)) / float64(time.Millisecond)
		case MetricPutP95:
			res.Value = float64(agg.put.Percentile(95)) / float64(time.Millisecond)
		case MetricPutP99, MetricPutP99CacheHit, MetricPutP99Origin:
			res.Value = float64(agg.put.Percentile(99)) / float64(time.Millisecond)
		case MetricGetP50:
			res.Value = float64(agg.get.Percentile(50)) / float64(time.Millisecond)
		case MetricGetP95:
			res.Value = float64(agg.get.Percentile(95)) / float64(time.Millisecond)
		case MetricGetP99, MetricGetP99L0CacheHit, MetricGetP99L1CacheHit, MetricGetP99Origin:
			res.Value = float64(agg.get.Percentile(99)) / float64(time.Millisecond)
		case MetricListP95:
			res.Value = float64(agg.list.Percentile(95)) / float64(time.Millisecond)
		case MetricCacheHitRatioHot:
			// We can only measure cache hit ratio when a
			// HotObjectCache is wired into the runner; against a
			// raw StorageProvider there is no cache layer to
			// hit. Surface that distinction so a Min-bounded
			// gate doesn't silently fail on a 0.0 reading.
			if r.Cache == nil {
				res.Pending = true
			} else if cacheTotal > 0 {
				res.Value = float64(agg.cacheHit) / float64(cacheTotal)
			}
		case MetricSustainedRPS:
			res.Value = attainedRPS
		case MetricRPSEfficiency:
			if cfg.targetRPS > 0 {
				res.Value = attainedRPS / float64(cfg.targetRPS)
			}
		case MetricErrorRate:
			if totalRequests > 0 {
				res.Value = float64(agg.errors) / float64(totalRequests)
			}
		case MetricDedupHitRatio,
			MetricDedupBytesSavedRatio,
			MetricDedupPutLatencyOverheadP95,
			MetricWasabiOriginEgressRatio,
			MetricMigrationThroughput,
			MetricRepairTimeSeconds,
			MetricNetworkCostUSDPerTB:
			// These metrics are not measurable from a raw
			// providers.StorageProvider — they require visibility
			// into gateway-layer state (dedup index, content
			// index, cross-cell migration coordinator, repair
			// worker, egress accounting). Mark Pending so RunSuite
			// skips EvaluateTarget instead of failing the scenario
			// with a 0.0 reading. A future gateway-level runner
			// can populate these from the gateway's metrics
			// pipeline.
			res.Pending = true
		default:
			// Unknown metric — also mark Pending so a typo in a
			// scenario definition surfaces in the report's
			// Pending list rather than silently passing a 0.0
			// reading against a Min-bounded target.
			res.Pending = true
		}
		res.Histogram = histogramFor(t.Metric, agg)
		res.Duration = elapsed
		results = append(results, res)
	}
	return results
}

func histogramFor(m Metric, agg *aggregate) *HistogramSummary {
	switch m {
	case MetricPutP50, MetricPutP95, MetricPutP99,
		MetricPutP99CacheHit, MetricPutP99Origin:
		s := agg.put.Summary()
		return &s
	case MetricGetP50, MetricGetP95, MetricGetP99,
		MetricGetP99L0CacheHit, MetricGetP99L1CacheHit, MetricGetP99Origin:
		s := agg.get.Summary()
		return &s
	case MetricListP95:
		s := agg.list.Summary()
		return &s
	}
	return nil
}

// aggregate holds the merged statistics for a scenario.
//
// attempts is the total number of request operations sent to the
// provider, success or failure. Histogram counts are
// success-only (failed-request latency is not recorded; see
// workerState.record), so the histogram counts cannot be used as
// the denominator for error-rate or attempted-throughput metrics.
type aggregate struct {
	put       LatencyHistogram
	get       LatencyHistogram
	head      LatencyHistogram
	del       LatencyHistogram
	list      LatencyHistogram
	cacheHit  int64
	cacheMiss int64
	attempts  int64
	errors    int64
}

func newAggregate() *aggregate { return &aggregate{} }

func (a *aggregate) merge(ws *workerState) {
	a.put.Merge(&ws.put)
	a.get.Merge(&ws.get)
	a.head.Merge(&ws.head)
	a.del.Merge(&ws.del)
	a.list.Merge(&ws.list)
	a.cacheHit += ws.cacheHit
	a.cacheMiss += ws.cacheMiss
	a.attempts += ws.attempts
	a.errors += ws.errors
}

// workerState is the per-goroutine recording state.
type workerState struct {
	id        uint64
	rng       *rand.Rand
	put       LatencyHistogram
	get       LatencyHistogram
	head      LatencyHistogram
	del       LatencyHistogram
	list      LatencyHistogram
	cacheHit  int64
	cacheMiss int64
	attempts  int64
	errors    int64
	seq       atomic.Uint64
	opOrder   []string
}

func newWorkerState(targetRPS int, id, baseSeed uint64) *workerState {
	_ = targetRPS // currently unused; kept for future per-worker tuning
	return &workerState{
		id:  id,
		rng: rand.New(rand.NewPCG(baseSeed+1, id+1)),
	}
}

// nextOp samples an operation from the mix. We rebuild the
// flattened slice the first time and reuse it thereafter.
func (s *workerState) nextOp(mix map[string]float64) string {
	if len(s.opOrder) == 0 {
		s.opOrder = flattenMix(mix)
	}
	return s.opOrder[s.rng.IntN(len(s.opOrder))]
}

// record updates the worker-local counters and per-op latency
// histogram for one completed operation.
//
// attempts is incremented for every call, success or failure, so
// it is the correct denominator for error-rate and
// attempted-throughput metrics.
//
// Failed-request durations are NOT recorded into the histogram:
// latency SLAs are a success-path property, and including
// error-path durations (often near-zero connection refused or
// near-timeout) would pollute the percentile metrics. This
// matches the older ProviderRunner semantics and the conventional
// behaviour of load-test tools (wrk, vegeta, k6).
func (s *workerState) record(op string, d time.Duration, err error) {
	s.attempts++
	if err != nil {
		s.errors++
		return
	}
	switch op {
	case "PUT":
		s.put.Record(d)
	case "GET":
		s.get.Record(d)
	case "HEAD":
		s.head.Record(d)
	case "DELETE":
		s.del.Record(d)
	case "LIST":
		s.list.Record(d)
	}
}

// keySet is the shared working set of pre-seeded plus
// runtime-PUT keys. The mutex protects slice membership only;
// the actual I/O happens outside the lock.
type keySet struct {
	mu   sync.Mutex
	keys []string
}

func (k *keySet) add(key string) {
	k.mu.Lock()
	k.keys = append(k.keys, key)
	k.mu.Unlock()
}

func (k *keySet) pick(rng *rand.Rand) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.keys) == 0 {
		return "", false
	}
	return k.keys[rng.IntN(len(k.keys))], true
}

func (k *keySet) popOne(rng *rand.Rand) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.keys) == 0 {
		return "", false
	}
	idx := rng.IntN(len(k.keys))
	key := k.keys[idx]
	k.keys = append(k.keys[:idx], k.keys[idx+1:]...)
	return key, true
}

// tokenBucket is a simple rate-controlled token source. A
// background goroutine emits tokens at the configured rate; the
// bucket is bounded so a slow consumer cannot accumulate
// unbounded burst credit. The emitter goroutine and all
// consumers exit via the shared context passed to start/acquire;
// no separate stop channel is needed.
type tokenBucket struct {
	rate   int
	tokens chan struct{}
}

func newTokenBucket(rate int) *tokenBucket {
	if rate <= 0 {
		rate = 1
	}
	burst := rate / 10
	if burst < 1 {
		burst = 1
	}
	if burst > 4096 {
		burst = 4096
	}
	return &tokenBucket{
		rate:   rate,
		tokens: make(chan struct{}, burst),
	}
}

func (b *tokenBucket) start(ctx context.Context) {
	// Emit tokens in batches of perTick = max(1, rate/100) every
	// 10ms. At 10K rps that is 100 tokens / 10ms, which keeps
	// the ticker frequency at 100 Hz regardless of rate.
	perTick := b.rate / 100
	if perTick < 1 {
		perTick = 1
	}
	interval := time.Duration(int64(time.Second) * int64(perTick) / int64(b.rate))
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for i := 0; i < perTick; i++ {
					select {
					case b.tokens <- struct{}{}:
					default:
						// Drop tokens that exceed the burst
						// budget. This keeps the consumer's
						// achievable RPS bounded by `rate`
						// rather than spiking after a stall.
					}
				}
			}
		}
	}()
}

func (b *tokenBucket) acquire(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case _, ok := <-b.tokens:
		return ok
	}
}

// nowFn returns Now or time.Now if Now is nil. Mirrors the helper
// on ProviderRunner so tests can substitute a clock.
func (r *SustainedRunner) nowFn() func() time.Time {
	if r.Now != nil {
		return r.Now
	}
	return time.Now
}

// AutoConcurrency returns the default concurrency for the given
// RPS. Exposed so the CLI can echo the effective value back to
// the operator without re-running the runner. The formula must
// match the runner's actual derivation in effectiveWorkload
// (8 + rps/250, capped at 512); any caller relying on this
// function to predict or display the runner's effective
// concurrency would otherwise get a wrong value.
func AutoConcurrency(rps int) int {
	c := 8 + rps/250
	if c > 512 {
		c = 512
	}
	return c
}


