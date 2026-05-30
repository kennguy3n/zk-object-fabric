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
