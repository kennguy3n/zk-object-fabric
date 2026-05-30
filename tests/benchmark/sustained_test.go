package benchmark

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// fakeProvider is an in-memory StorageProvider used only by the
// sustained-runner tests. It supports configurable per-op latency
// and per-op error injection so we can pin the runner's behaviour
// without standing up a real backend.
type fakeProvider struct {
	mu     sync.Mutex
	pieces map[string][]byte

	latency map[string]time.Duration
	failOp  map[string]bool
	// failAfter[op] is the number of successful calls of op that
	// must complete before subsequent calls start failing. 0
	// means "fail immediately when failOp is true". This lets a
	// test seed the working set successfully and only then start
	// returning failures.
	failAfter map[string]int64
	// failEveryN[op] = N>0 makes only every Nth call (after
	// failAfter) fail, simulating a low transient error rate
	// rather than a hard outage. 0 keeps the all-fail behaviour
	// of setFailAfter.
	failEveryN map[string]int64

	puts    atomic.Int64
	gets    atomic.Int64
	heads   atomic.Int64
	deletes atomic.Int64
	lists   atomic.Int64
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		pieces:     map[string][]byte{},
		latency:    map[string]time.Duration{},
		failOp:     map[string]bool{},
		failAfter:  map[string]int64{},
		failEveryN: map[string]int64{},
	}
}

func (f *fakeProvider) setLatency(op string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latency[op] = d
}

// setFailAfter arms a failure injector that fires only after the
// given number of successful calls of op have completed.
func (f *fakeProvider) setFailAfter(op string, after int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOp[op] = true
	f.failAfter[op] = after
}

// setFailEveryNth arms a transient-failure injector: only every
// Nth call of op (counted past `after`) fails. Use this to model
// a low background error rate without simulating a hard outage.
func (f *fakeProvider) setFailEveryNth(op string, every, after int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOp[op] = true
	f.failAfter[op] = after
	f.failEveryN[op] = every
}

func (f *fakeProvider) sleepFor(op string) {
	f.mu.Lock()
	d := f.latency[op]
	f.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
}

func (f *fakeProvider) shouldFail(op string, count int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.failOp[op] {
		return false
	}
	if count <= f.failAfter[op] {
		return false
	}
	if every := f.failEveryN[op]; every > 0 {
		// (count - failAfter) is the index of this drive-phase
		// call; fail only every `every`th one.
		return ((count - f.failAfter[op]) % every) == 0
	}
	return true
}

func (f *fakeProvider) PutPiece(_ context.Context, pieceID string, r io.Reader, _ providers.PutOptions) (providers.PutResult, error) {
	f.sleepFor("PUT")
	n := f.puts.Add(1)
	if f.shouldFail("PUT", n) {
		return providers.PutResult{}, fmt.Errorf("fake: PUT injected failure")
	}
	buf, err := io.ReadAll(r)
	if err != nil {
		return providers.PutResult{}, err
	}
	f.mu.Lock()
	f.pieces[pieceID] = buf
	f.mu.Unlock()
	return providers.PutResult{PieceID: pieceID, SizeBytes: int64(len(buf)), Backend: "fake"}, nil
}

func (f *fakeProvider) GetPiece(_ context.Context, pieceID string, _ *providers.ByteRange) (io.ReadCloser, error) {
	f.sleepFor("GET")
	n := f.gets.Add(1)
	if f.shouldFail("GET", n) {
		return nil, fmt.Errorf("fake: GET injected failure")
	}
	f.mu.Lock()
	buf, ok := f.pieces[pieceID]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fake: GET %q: not found", pieceID)
	}
	return io.NopCloser(bytes.NewReader(buf)), nil
}

func (f *fakeProvider) HeadPiece(_ context.Context, pieceID string) (providers.PieceMetadata, error) {
	f.sleepFor("HEAD")
	n := f.heads.Add(1)
	if f.shouldFail("HEAD", n) {
		return providers.PieceMetadata{}, fmt.Errorf("fake: HEAD injected failure")
	}
	f.mu.Lock()
	buf, ok := f.pieces[pieceID]
	f.mu.Unlock()
	if !ok {
		return providers.PieceMetadata{}, fmt.Errorf("fake: HEAD %q: not found", pieceID)
	}
	return providers.PieceMetadata{PieceID: pieceID, SizeBytes: int64(len(buf))}, nil
}

func (f *fakeProvider) DeletePiece(_ context.Context, pieceID string) error {
	f.sleepFor("DELETE")
	n := f.deletes.Add(1)
	if f.shouldFail("DELETE", n) {
		return fmt.Errorf("fake: DELETE injected failure")
	}
	f.mu.Lock()
	delete(f.pieces, pieceID)
	f.mu.Unlock()
	return nil
}

func (f *fakeProvider) ListPieces(_ context.Context, _, _ string) (providers.ListResult, error) {
	f.sleepFor("LIST")
	n := f.lists.Add(1)
	if f.shouldFail("LIST", n) {
		return providers.ListResult{}, fmt.Errorf("fake: LIST injected failure")
	}
	f.mu.Lock()
	keys := make([]string, 0, len(f.pieces))
	for k := range f.pieces {
		keys = append(keys, k)
	}
	f.mu.Unlock()
	sort.Strings(keys)
	out := providers.ListResult{}
	for _, k := range keys {
		out.Pieces = append(out.Pieces, providers.PieceMetadata{PieceID: k})
	}
	return out, nil
}

func (f *fakeProvider) Capabilities() providers.ProviderCapabilities {
	return providers.ProviderCapabilities{SupportsRangeReads: true}
}

func (f *fakeProvider) CostModel() providers.ProviderCostModel { return providers.ProviderCostModel{} }

func (f *fakeProvider) PlacementLabels() providers.PlacementLabels {
	return providers.PlacementLabels{Provider: "fake"}
}

// quickScenario returns a fast scenario suitable for the test
// suite. It is intentionally short so the test binary stays well
// under a second per scenario.
func quickScenario(op string, targets ...Target) Scenario {
	return Scenario{
		Name:        "quick-" + strings.ToLower(op),
		Description: "test scenario for op " + op,
		Workload: Workload{
			RequestMix:      map[string]float64{op: 1.0},
			ObjectSizeBytes: 4096,
			TenantCount:     1,
			DurationSeconds: 1,
			TargetRPS:       200,
		},
		Targets: targets,
	}
}

// TestSustainedRunner_RunPopulatesAllTargets verifies every Target
// declared on a scenario produces a Result (no silently-dropped
// metrics).
func TestSustainedRunner_RunPopulatesAllTargets(t *testing.T) {
	prov := newFakeProvider()
	runner := NewSustainedRunner(prov)
	runner.DurationOverride = 300 * time.Millisecond

	sc := quickScenario("PUT",
		Target{Metric: MetricPutP50, Unit: "ms"},
		Target{Metric: MetricPutP95, Unit: "ms"},
		Target{Metric: MetricPutP99, Unit: "ms"},
		Target{Metric: MetricSustainedRPS, Unit: "req/s"},
		Target{Metric: MetricRPSEfficiency, Unit: "ratio"},
		Target{Metric: MetricErrorRate, Unit: "ratio"},
	)

	results, err := runner.Run(sc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != len(sc.Targets) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(sc.Targets))
	}
	for i, r := range results {
		if r.Metric != sc.Targets[i].Metric {
			t.Errorf("results[%d].Metric = %q, want %q", i, r.Metric, sc.Targets[i].Metric)
		}
		if r.Metric == MetricPutP50 || r.Metric == MetricPutP95 || r.Metric == MetricPutP99 {
			if r.Histogram == nil {
				t.Errorf("results[%d] (%q): want Histogram, got nil", i, r.Metric)
			}
		}
	}
}

// TestSustainedRunner_AttainsTargetRPS verifies the rate limiter
// drives the runner to within a tight tolerance of TargetRPS for
// fast operations.
func TestSustainedRunner_AttainsTargetRPS(t *testing.T) {
	prov := newFakeProvider()
	runner := NewSustainedRunner(prov)
	runner.DurationOverride = 600 * time.Millisecond

	sc := Scenario{
		Name: "rps-attainment",
		Workload: Workload{
			RequestMix:      map[string]float64{"PUT": 1.0},
			ObjectSizeBytes: 256,
			DurationSeconds: 1,
			TargetRPS:       500,
		},
		Targets: []Target{
			{Metric: MetricSustainedRPS, Unit: "req/s"},
			{Metric: MetricRPSEfficiency, Unit: "ratio"},
		},
	}

	results, err := runner.Run(sc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var rps, eff float64
	for _, r := range results {
		switch r.Metric {
		case MetricSustainedRPS:
			rps = r.Value
		case MetricRPSEfficiency:
			eff = r.Value
		}
	}
	// Accept anything between 70% and 110% of target — the upper
	// bound is the burst margin built into the token bucket; the
	// lower bound covers slow CI hosts.
	if eff < 0.7 || eff > 1.1 {
		t.Fatalf("rps_efficiency = %.3f at TargetRPS=%d (rps=%.1f), want in [0.7, 1.1]",
			eff, sc.Workload.TargetRPS, rps)
	}
}

// TestSustainedRunner_RecordsInjectedLatency verifies the runner
// reports a p99 close to the injected per-op delay.
func TestSustainedRunner_RecordsInjectedLatency(t *testing.T) {
	prov := newFakeProvider()
	prov.setLatency("PUT", 5*time.Millisecond)

	runner := NewSustainedRunner(prov)
	runner.DurationOverride = 500 * time.Millisecond
	runner.Concurrency = 16

	sc := Scenario{
		Name: "latency",
		Workload: Workload{
			RequestMix:      map[string]float64{"PUT": 1.0},
			ObjectSizeBytes: 128,
			DurationSeconds: 1,
			TargetRPS:       200,
		},
		Targets: []Target{
			{Metric: MetricPutP99, Unit: "ms"},
		},
	}

	results, err := runner.Run(sc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	p99 := results[0].Value
	// Each PUT sleeps for 5ms; expect p99 latency to be in a
	// reasonable window around that. Allow 4ms..30ms to absorb
	// goroutine wake-up jitter on slow CI hosts.
	if p99 < 4.0 || p99 > 30.0 {
		t.Fatalf("PUT p99 = %.2fms with 5ms injected latency, want in [4, 30]", p99)
	}
}

// TestSustainedRunner_CountsErrorRate verifies the error_rate
// metric reflects injected failures.
func TestSustainedRunner_CountsErrorRate(t *testing.T) {
	prov := newFakeProvider()
	// Allow the seed phase (4 PUTs) to succeed, then fail every
	// subsequent PUT.
	runner := NewSustainedRunner(prov)
	runner.DurationOverride = 200 * time.Millisecond
	runner.FailureLimit = 1 << 30 // do not abort
	runner.SeedObjects = 4
	prov.setFailAfter("PUT", int64(runner.SeedObjects))

	sc := Scenario{
		Name: "error-rate",
		Workload: Workload{
			// Use a GET seed so PUT failures are not the only
			// requests issued.
			RequestMix:      map[string]float64{"PUT": 0.5, "GET": 0.5},
			ObjectSizeBytes: 128,
			DurationSeconds: 1,
			TargetRPS:       100,
		},
		Targets: []Target{
			{Metric: MetricErrorRate, Unit: "ratio"},
			{Metric: MetricSustainedRPS, Unit: "req/s"},
		},
	}

	results, err := runner.Run(sc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var er float64
	for _, r := range results {
		if r.Metric == MetricErrorRate {
			er = r.Value
		}
	}
	if er <= 0 {
		t.Fatalf("error_rate = %.4f, want > 0 after injecting PUT failures", er)
	}
	if er >= 1.0 {
		t.Fatalf("error_rate = %.4f, want < 1.0 (GETs against the seeded pool should succeed)", er)
	}
}

// TestSustainedRunner_FailureLimitAborts verifies that a sustained
// stream of failures trips the FailureLimit guard.
func TestSustainedRunner_FailureLimitAborts(t *testing.T) {
	prov := newFakeProvider()
	runner := NewSustainedRunner(prov)
	runner.DurationOverride = 5 * time.Second
	runner.FailureLimit = 16
	runner.SeedObjects = 8
	// Let the seed phase succeed, then fail every drive-phase
	// PUT so the failure-limit guard trips quickly.
	prov.setFailAfter("PUT", int64(runner.SeedObjects))

	sc := Scenario{
		Name: "fail-fast",
		Workload: Workload{
			RequestMix:      map[string]float64{"PUT": 1.0},
			ObjectSizeBytes: 128,
			DurationSeconds: 1,
			TargetRPS:       200,
		},
		Targets: []Target{
			{Metric: MetricPutP99, Unit: "ms"},
		},
	}

	t0 := time.Now()
	_, err := runner.Run(sc)
	elapsed := time.Since(t0)
	if err == nil {
		t.Fatalf("Run: want failure-limit error, got nil")
	}
	if !strings.Contains(err.Error(), "failure limit") {
		t.Fatalf("Run error = %v, want failure-limit error", err)
	}
	// The seed phase already does 64 PUTs which the fake provider
	// fails. Confirm we did not run for the full 5s.
	if elapsed > 4*time.Second {
		t.Fatalf("Run elapsed %v, want < 4s after failure-limit trip", elapsed)
	}
}

// TestRunSuite_WithSustainedRunner verifies that the existing
// RunSuite glue works against the new runner.
func TestRunSuite_WithSustainedRunner(t *testing.T) {
	prov := newFakeProvider()
	runner := NewSustainedRunner(prov)
	runner.DurationOverride = 150 * time.Millisecond
	runner.RPSOverride = 50
	runner.MaxObjectSizeBytes = 4096
	runner.SeedObjects = 16

	// Build a tiny suite of the same shape DefaultSuite produces.
	suite := Suite{
		Name: "smoke",
		Scenarios: []Scenario{
			{
				Name: "put-only",
				Workload: Workload{
					RequestMix:      map[string]float64{"PUT": 1.0},
					ObjectSizeBytes: 1024,
					DurationSeconds: 1,
					TargetRPS:       50,
				},
				Targets: []Target{
					{Metric: MetricPutP99, Max: 200.0, Unit: "ms"},
				},
			},
		},
	}

	rep, err := RunSuite(suite, runner)
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if rep == nil || len(rep.Scenarios) != 1 {
		t.Fatalf("Report scenarios = %d, want 1", len(rep.Scenarios))
	}
	if !rep.Scenarios[0].Pass {
		t.Fatalf("Scenario did not pass: %v", rep.Scenarios[0].Failures)
	}
}

// TestSustainedRunner_SeedAndWorkingSetCleanup is a regression
// guard: the working-set helper must not deadlock when DELETE
// drains the pool faster than PUT refills it.
func TestSustainedRunner_SeedAndWorkingSetCleanup(t *testing.T) {
	prov := newFakeProvider()
	runner := NewSustainedRunner(prov)
	runner.DurationOverride = 300 * time.Millisecond
	runner.SeedObjects = 16

	sc := Scenario{
		Name: "delete-heavy",
		Workload: Workload{
			RequestMix:      map[string]float64{"DELETE": 1.0},
			ObjectSizeBytes: 64,
			DurationSeconds: 1,
			TargetRPS:       200,
		},
		Targets: []Target{
			{Metric: MetricSustainedRPS, Unit: "req/s"},
		},
	}

	_, err := runner.Run(sc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestSustainedRunner_ContextDeadline verifies the runner exits
// cleanly when its context is cancelled before the configured
// duration elapses (used by CLI signal-handling).
func TestSustainedRunner_ContextDeadline(t *testing.T) {
	prov := newFakeProvider()
	prov.setLatency("PUT", 1*time.Millisecond)
	runner := NewSustainedRunner(prov)
	runner.DurationOverride = 100 * time.Millisecond

	sc := Scenario{
		Name: "ctx",
		Workload: Workload{
			RequestMix:      map[string]float64{"PUT": 1.0},
			ObjectSizeBytes: 64,
			DurationSeconds: 1,
			TargetRPS:       100,
		},
		Targets: []Target{
			{Metric: MetricPutP99, Unit: "ms"},
		},
	}
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(sc)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Run did not return within 3s")
	}
}

// TestSustainedRunner_FailureLimitDoesNotTripUnderTransientErrors
// verifies the fix for the cumulative-vs-consecutive bug: a low
// background error rate must NOT trip the consecutive failure
// limit, even when total errors over the run exceed it.
func TestSustainedRunner_FailureLimitDoesNotTripUnderTransientErrors(t *testing.T) {
	prov := newFakeProvider()
	runner := NewSustainedRunner(prov)
	runner.DurationOverride = 500 * time.Millisecond
	runner.FailureLimit = 8 // low, so a cumulative counter would trip
	runner.SeedObjects = 8

	// Inject a transient PUT failure every 10th call. With 50/50
	// PUT/GET this gives ~5% error rate -- well above
	// TargetErrorRateMax for a real run, but no streak ever hits
	// 8 consecutive failures because GETs always succeed and PUT
	// successes between failures reset the counter.
	prov.setFailEveryNth("PUT", 10, int64(runner.SeedObjects))

	sc := Scenario{
		Name: "transient-errors",
		Workload: Workload{
			RequestMix:      map[string]float64{"PUT": 0.5, "GET": 0.5},
			ObjectSizeBytes: 128,
			DurationSeconds: 1,
			TargetRPS:       400,
		},
		Targets: []Target{{Metric: MetricPutP99, Unit: "ms"}},
	}

	results, err := runner.Run(sc)
	if err != nil {
		t.Fatalf("Run: unexpected failure-limit trip on transient errors: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Run returned 0 results")
	}
}

// TestSustainedRunner_PendingForUnmeasurableMetrics verifies that
// SustainedRunner emits Result.Pending=true for metrics that the
// raw StorageProvider interface cannot observe (e.g. dedup hit
// ratio, cross-cell migration throughput). These must NOT cause
// the scenario to fail; instead they surface in
// ReportScenario.Pending when RunSuite is used.
func TestSustainedRunner_PendingForUnmeasurableMetrics(t *testing.T) {
	prov := newFakeProvider()
	runner := NewSustainedRunner(prov)
	runner.DurationOverride = 200 * time.Millisecond
	runner.SeedObjects = 4

	sc := Scenario{
		Name: "dedup-pending",
		Workload: Workload{
			RequestMix:      map[string]float64{"PUT": 0.5, "GET": 0.5},
			ObjectSizeBytes: 128,
			DurationSeconds: 1,
			TargetRPS:       100,
		},
		Targets: []Target{
			{Metric: MetricPutP99, Unit: "ms"},
			{Metric: MetricDedupHitRatio, Min: 0.75, Unit: "ratio"},
			{Metric: MetricMigrationThroughput, Min: 1e6, Unit: "bytes_per_sec"},
		},
	}

	results, err := runner.Run(sc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("Run returned %d results, want 3", len(results))
	}
	if results[0].Pending {
		t.Errorf("MetricPutP99 unexpectedly Pending")
	}
	if !results[1].Pending {
		t.Errorf("MetricDedupHitRatio should be Pending (provider-level runner cannot measure dedup)")
	}
	if !results[2].Pending {
		t.Errorf("MetricMigrationThroughput should be Pending (provider-level runner cannot measure cross-cell migration)")
	}

	// And via RunSuite the scenario should pass (Pending is not a
	// failure), with the two unmeasured metrics surfaced in
	// ReportScenario.Pending.
	rep, err := RunSuite(Suite{Name: "test", Scenarios: []Scenario{sc}}, runner)
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if !rep.AllPassed {
		t.Errorf("AllPassed = false; want true (Pending metrics must not fail the suite). Failures=%v", rep.Scenarios[0].Failures)
	}
	if len(rep.Scenarios[0].Pending) != 2 {
		t.Errorf("ReportScenario.Pending = %v, want 2 entries (dedup_hit_ratio, migration_throughput_bytes_per_sec)", rep.Scenarios[0].Pending)
	}
}

// TestSustainedRunner_CtxCancelStopsRun verifies that cancelling
// the Ctx field aborts an in-flight scenario instead of waiting
// for the duration to elapse.
func TestSustainedRunner_CtxCancelStopsRun(t *testing.T) {
	prov := newFakeProvider()
	runner := NewSustainedRunner(prov)
	runner.DurationOverride = 30 * time.Second // would block well past test timeout
	runner.SeedObjects = 4

	ctx, cancel := context.WithCancel(context.Background())
	runner.Ctx = ctx

	sc := Scenario{
		Name: "ctx-cancel",
		Workload: Workload{
			RequestMix:      map[string]float64{"PUT": 1.0},
			ObjectSizeBytes: 128,
			DurationSeconds: 30,
			TargetRPS:       50,
		},
		Targets: []Target{{Metric: MetricPutP99, Unit: "ms"}},
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	t0 := time.Now()
	_, err := runner.Run(sc)
	elapsed := time.Since(t0)
	// Ctx cancellation should drain quickly. We don't assert on
	// err vs nil because the runner may return either a clean
	// nil (all goroutines drained on Ctx.Done) or a context
	// error depending on which phase saw the cancel first; what
	// matters is we did not run for the full 30s.
	_ = err
	if elapsed > 3*time.Second {
		t.Fatalf("Run elapsed %v after Ctx cancel; want fast exit", elapsed)
	}
}

// TestSustainedRunner_LatencyExcludesFailedRequests asserts that
// per-op latency histograms record only successful calls, so
// fast-failing error responses cannot pull p99 numbers down and
// hide an SLA breach. Regression for Devin Review finding
// ANALYSIS_0001 (sustained.go:567 workerState.record).
func TestSustainedRunner_LatencyExcludesFailedRequests(t *testing.T) {
	prov := newFakeProvider()
	prov.setLatency("PUT", 50*time.Millisecond) // success path: slow
	// Errors return instantly with no latency injected. Without
	// the fix, the fast-failing PUTs would yank the p99 toward 0.
	// Seed first (PUTs 0..3 succeed), then start the every-other-N
	// failure pattern during the drive phase.
	runner := NewSustainedRunner(prov)
	runner.DurationOverride = 1 * time.Second
	runner.SeedObjects = 4
	runner.FailureLimit = 1 << 30 // disable the failure-limit gate
	prov.setFailEveryNth("PUT", 2, int64(runner.SeedObjects))

	sc := Scenario{
		Name: "latency-only-success",
		Workload: Workload{
			RequestMix:      map[string]float64{"PUT": 1.0},
			ObjectSizeBytes: 128,
			DurationSeconds: 1,
			TargetRPS:       40,
		},
		Targets: []Target{{Metric: MetricPutP99, Unit: "ms"}},
	}

	results, err := runner.Run(sc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Run returned %d results, want 1", len(results))
	}
	gotMs := results[0].Value
	// Success calls take 50ms; histogram bucket precision is
	// roughly 0.4% which is well under the 5ms tolerance we
	// allow here. With error samples mixed in, p99 would land
	// far below 50ms; the test exists to guard against that
	// regression.
	if gotMs < 40 || gotMs > 70 {
		t.Errorf("MetricPutP99 = %v ms; want roughly 50 ms (success-only latency). "+
			"A value far from 50 ms means error-path durations leaked into the histogram.", gotMs)
	}
}

// TestAutoConcurrencyMatchesEffectiveWorkload asserts that the
// exported AutoConcurrency helper returns the same value the
// runner itself derives in effectiveWorkload. Regression for
// Devin Review finding BUG_0001 (AutoConcurrency had a
// runtime.NumCPU() floor that effectiveWorkload did not).
func TestAutoConcurrencyMatchesEffectiveWorkload(t *testing.T) {
	prov := newFakeProvider()
	runner := NewSustainedRunner(prov)

	// Use a few representative RPS values, including some that
	// would land below NumCPU on a typical CI box (8-16 cores).
	for _, rps := range []int{1, 10, 50, 100, 250, 500, 1000, 5000, 10_000, 100_000, 1_000_000} {
		cfg, err := runner.effectiveWorkload(Workload{
			RequestMix:      map[string]float64{"PUT": 1.0},
			ObjectSizeBytes: 128,
			DurationSeconds: 1,
			TargetRPS:       rps,
		})
		if err != nil {
			t.Fatalf("effectiveWorkload(rps=%d): %v", rps, err)
		}
		gotAuto := AutoConcurrency(rps)
		if gotAuto != cfg.concurrency {
			t.Errorf("AutoConcurrency(%d) = %d; runner derived concurrency = %d. "+
				"These must match so callers can predict the runner's worker count.",
				rps, gotAuto, cfg.concurrency)
		}
	}
}

// TestSustainedRunner_RunDurationUsesWallClock asserts that the
// run-duration enforcement is wall-clock based and is NOT
// influenced by a mocked Now field. The previous implementation
// used context.WithDeadline(ctx, r.nowFn()().Add(d)), which
// silently broke under a mock clock because the context library
// compares against time.Now() internally. The fix is to use
// context.WithTimeout(ctx, d), which is duration-based and does
// not consult any clock for the input. Regression for Devin
// Review finding ANALYSIS_0002 (sustained.go:245-246).
func TestSustainedRunner_RunDurationUsesWallClock(t *testing.T) {
	prov := newFakeProvider()
	runner := NewSustainedRunner(prov)
	runner.DurationOverride = 200 * time.Millisecond
	runner.SeedObjects = 4
	// Mock clock far in the future. With the old WithDeadline
	// code path this still terminated (because context used
	// wall clock and ignored the mocked future), but the
	// deadline value was nonsense. We assert the new code is
	// independent of this field's value: real wall-clock budget
	// is still ~200ms.
	mockNow := time.Now().Add(24 * time.Hour)
	runner.Now = func() time.Time { return mockNow }

	sc := Scenario{
		Name: "wall-clock-budget",
		Workload: Workload{
			RequestMix:      map[string]float64{"PUT": 1.0},
			ObjectSizeBytes: 128,
			DurationSeconds: 1,
			TargetRPS:       40,
		},
		Targets: []Target{{Metric: MetricPutP99, Unit: "ms"}},
	}

	t0 := time.Now()
	_, err := runner.Run(sc)
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Allow generous slack on the upper bound for CI scheduler
	// jitter, but the run must finish well under 2s — far less
	// than 24h — proving the mock clock did not extend the
	// budget.
	if elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("Run elapsed %v; expected roughly 200ms (wall-clock budget, "+
			"unaffected by mocked Now)", elapsed)
	}
}

// TestLatencyHistogram_MergeKeepsZeroMin asserts that Merge
// preserves a genuine 0ns minimum from the other histogram
// instead of treating minNS==0 as a "no samples" sentinel.
// Regression for Devin Review finding ANALYSIS_0003
// (histogram.go:145-147).
func TestLatencyHistogram_MergeKeepsZeroMin(t *testing.T) {
	// Receiver has one 5ms sample.
	h := &LatencyHistogram{}
	h.Record(5 * time.Millisecond)

	// Other has a single 0ns sample (legitimate clamp from a
	// negative duration via Record's clamp path).
	other := &LatencyHistogram{}
	other.Record(-1 * time.Nanosecond)
	if other.Min() != 0 {
		t.Fatalf("other.Min() = %v; want 0 (Record clamps negative to 0)", other.Min())
	}

	h.Merge(other)
	if got := h.Min(); got != 0 {
		t.Errorf("h.Min() after merge = %v; want 0 (zero-sample minimum must be preserved)", got)
	}
}

// TestMakePayload_DeterministicAndReused asserts the documented
// contract: makePayload returns the same deterministic bytes for
// the same size, and the comment-vs-code mismatch from the
// previous version (which claimed per-piece distinctness) is no
// longer true. Regression for Devin Review finding ANALYSIS_0004.
func TestMakePayload_DeterministicAndReused(t *testing.T) {
	a := makePayload(2048)
	b := makePayload(2048)
	if !bytes.Equal(a, b) {
		t.Fatal("makePayload(2048) returned different bytes on repeated calls; " +
			"the runner relies on deterministic payload for predictable I/O cost")
	}

	// Zero or negative size must yield a 1KiB default.
	if got := len(makePayload(0)); got != 1024 {
		t.Errorf("makePayload(0) length = %d; want 1024 default", got)
	}
	if got := len(makePayload(-5)); got != 1024 {
		t.Errorf("makePayload(-5) length = %d; want 1024 default", got)
	}
}

// TestEffectiveWorkload_MaxObjectSizeBytesCapsDefault verifies the
// MaxObjectSizeBytes cap is honoured even when a scenario declares
// ObjectSizeBytes=0 (e.g. LIST-only scenarios). Regression for
// Devin Review finding ANALYSIS_pr-review-job-…_0002: the cap was
// previously applied before the 1024-default fallback, so a 0
// declared size fell through to 1024 and silently bypassed the
// operator-set ceiling.
func TestEffectiveWorkload_MaxObjectSizeBytesCapsDefault(t *testing.T) {
	prov := newFakeProvider()
	runner := NewSustainedRunner(prov)
	runner.MaxObjectSizeBytes = 512

	sc := Scenario{
		Name: "size-cap-default",
		Workload: Workload{
			RequestMix:      map[string]float64{"PUT": 1.0},
			ObjectSizeBytes: 0, // explicit 0 — runner must apply 1024 default, then cap to 512
			DurationSeconds: 1,
			TargetRPS:       100,
		},
	}
	cfg, err := runner.effectiveWorkload(sc.Workload)
	if err != nil {
		t.Fatalf("effectiveWorkload: %v", err)
	}
	if cfg.objectSize != 512 {
		t.Errorf("effectiveWorkload.objectSize = %d; want 512 "+
			"(1024 default capped to MaxObjectSizeBytes=512). "+
			"Cap must be applied AFTER the default fallback so a "+
			"declared 0 cannot silently bypass the ceiling.", cfg.objectSize)
	}

	// Also verify the explicit-size path still caps correctly:
	// a 2048 declared size capped to 512.
	sc.Workload.ObjectSizeBytes = 2048
	cfg, err = runner.effectiveWorkload(sc.Workload)
	if err != nil {
		t.Fatalf("effectiveWorkload: %v", err)
	}
	if cfg.objectSize != 512 {
		t.Errorf("effectiveWorkload.objectSize = %d; want 512 "+
			"(2048 capped to MaxObjectSizeBytes=512)", cfg.objectSize)
	}
}

// TestProviderRunner_PendingForUnmeasurableMetrics verifies that
// ProviderRunner (the synchronous unit-test driver) marks
// sustained-load and cache-tier-segmented metrics as Pending
// instead of silently reporting 0 — symmetric with SustainedRunner.
// Regression for Devin Review finding ANALYSIS_…_0003: the runner
// previously fell through to a zero-valued Result, which would
// breach Min-bounded SLA gates (e.g. sustained_rps Min=10000).
func TestProviderRunner_PendingForUnmeasurableMetrics(t *testing.T) {
	prov := newFakeProvider()
	runner := NewProviderRunner(prov)

	sc := Scenario{
		Name: "provider-pending",
		Workload: Workload{
			RequestMix:      map[string]float64{"PUT": 1.0},
			ObjectSizeBytes: 128,
			DurationSeconds: 1,
			TargetRPS:       1,
		},
		Targets: []Target{
			// ProviderRunner CAN measure these — no Pending.
			{Metric: MetricPutP99, Max: 1000, Unit: "ms"},
			// ProviderRunner CANNOT measure these — must be Pending,
			// not silently 0.
			{Metric: MetricSustainedRPS, Min: 10000, Unit: "req/s"},
			{Metric: MetricRPSEfficiency, Min: 0.95, Unit: "ratio"},
			{Metric: MetricErrorRate, Max: 1e-3, Unit: "ratio"},
			{Metric: MetricPutP99CacheHit, Max: 50, Unit: "ms"},
			{Metric: MetricPutP99Origin, Max: 200, Unit: "ms"},
			{Metric: MetricGetP99L0CacheHit, Max: 20, Unit: "ms"},
			{Metric: MetricGetP99L1CacheHit, Max: 100, Unit: "ms"},
			{Metric: MetricGetP99Origin, Max: 300, Unit: "ms"},
			// Control-plane metrics — also Pending.
			{Metric: MetricMigrationThroughput, Min: 1e6, Unit: "bytes_per_sec"},
		},
	}

	results, err := runner.Run(sc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != len(sc.Targets) {
		t.Fatalf("Run returned %d results, want %d", len(results), len(sc.Targets))
	}
	if results[0].Pending {
		t.Errorf("MetricPutP99 unexpectedly Pending (ProviderRunner CAN measure put p99)")
	}
	for i := 1; i < len(results); i++ {
		r := results[i]
		if !r.Pending {
			t.Errorf("Targets[%d] (%s) should be Pending — ProviderRunner has no "+
				"measurement path. value=%v", i, r.Metric, r.Value)
		}
		if r.PendingReason == "" {
			t.Errorf("Targets[%d] (%s) Pending but PendingReason is empty; "+
				"runners must explain WHY a metric is unmeasurable so report "+
				"consumers can distinguish 'not wired in' from 'not applicable'.",
				i, r.Metric)
		}
	}

	// And via RunSuite the scenario should still pass: Pending
	// must NOT be treated as a Min-gate breach.
	rep, err := RunSuite(Suite{Name: "test", Scenarios: []Scenario{sc}}, runner)
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if !rep.AllPassed {
		t.Errorf("AllPassed = false; want true (Pending metrics must not fail). "+
			"Failures=%v", rep.Scenarios[0].Failures)
	}
	if len(rep.Scenarios[0].Pending) != 9 {
		t.Errorf("ReportScenario.Pending count = %d; want 9 unmeasurable metrics. "+
			"Pending list = %v", len(rep.Scenarios[0].Pending), rep.Scenarios[0].Pending)
	}
}

// TestBucketIndex_NoNegativeShift exercises the bucketIndex
// invariant that mag is always >= 1 for ns >= subBucketCount.
// Regression for Devin Review finding ANALYSIS_…_0004: the
// previous `if mag < 0 { mag = 0 }` clamp was unreachable and
// would have caused a negative-count shift panic if it ever
// triggered. This test asserts the bucketing is panic-free across
// the full spectrum from 0ns to the histogram ceiling.
func TestBucketIndex_NoNegativeShift(t *testing.T) {
	// Walk a wide spread of nanosecond values including the edge
	// cases that previously triggered the dead guard's review.
	samples := []int64{
		0,
		1,
		int64(subBucketCount - 1),
		int64(subBucketCount),
		int64(subBucketCount + 1),
		1 << 16,
		1 << 24,
		1 << 32,
		int64((1 << 60) - 1),
		1 << 60,
	}
	for _, ns := range samples {
		// Must not panic and must return a valid bucket index in
		// [0, totalBuckets).
		idx := bucketIndex(ns)
		if idx < 0 || idx >= totalBuckets {
			t.Errorf("bucketIndex(%d) = %d; out of range [0, %d)",
				ns, idx, totalBuckets)
		}
	}
}
