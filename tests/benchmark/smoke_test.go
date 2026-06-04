package benchmark

import (
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
)

// TestSmoke_LocalFSLatencyTargets is the CI-level production-validation
// smoke test referenced by the staging-validation workflow
// (.github/workflows/ci.yml) and docs/PROGRESS.md. It drives an
// in-process SustainedRunner against the local_fs_dev provider for a
// short burst at ~1000 RPS and asserts that the harness meets the
// published p99 latency and error-rate SLA targets at reduced scale.
//
// It deliberately does NOT validate the production SLA (that is the
// Tier 3 Linode+Wasabi run in docs/runbooks/load-testing.md): a local
// filesystem at 1000 RPS is far below the 10K-RPS-per-node production
// target, so the local p99 should sit comfortably under the origin-
// tier caps. The value here is catching a regression that breaks the
// runner, rate limiter, histograms, or blows past a latency target
// even at toy scale — without needing paid infra.
//
// Margins: the assertions use the production origin-tier latency caps
// (TargetPutP99OriginMs / TargetGetP99OriginMs) as upper bounds. These
// are ~200-300 ms, two-plus orders of magnitude above the sub-
// millisecond local-fs latencies, so the test has a wide margin and is
// not expected to flake on slow CI hosts. The rps-efficiency bound is
// intentionally loose (>= 0.6) for the same reason.
func TestSmoke_LocalFSLatencyTargets(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ~10s load smoke in -short mode")
	}

	prov, err := local_fs_dev.New(t.TempDir())
	if err != nil {
		t.Fatalf("local_fs_dev.New: %v", err)
	}

	runner := NewSustainedRunner(prov)
	// ~10s burst at ~1000 RPS. DurationOverride caps wall-clock time
	// independently of the scenario's declared (production) duration.
	runner.DurationOverride = 10 * time.Second
	// Keep payloads small so the smoke run stays I/O-light and memory
	// bounded; we are validating the harness + provider path, not
	// large-object throughput.
	runner.MaxObjectSizeBytes = 8 * 1024

	sc := Scenario{
		Name:        "smoke-local-fs-latency",
		Description: "Reduced-scale mixed PUT/GET burst against local_fs_dev gating the origin-tier p99 + error-rate SLAs.",
		Workload: Workload{
			RequestMix:      map[string]float64{"PUT": 0.3, "GET": 0.7},
			ObjectSizeBytes: 8 * 1024,
			TenantCount:     4,
			DurationSeconds: 10,
			TargetRPS:       1000,
		},
		Targets: []Target{
			{Metric: MetricPutP99, Max: TargetPutP99OriginMs, Unit: "ms"},
			{Metric: MetricGetP99, Max: TargetGetP99OriginMs, Unit: "ms"},
			{Metric: MetricErrorRate, Max: TargetErrorRateMax, Unit: "ratio"},
			{Metric: MetricSustainedRPS, Unit: "req/s"},
			{Metric: MetricRPSEfficiency, Unit: "ratio"},
		},
	}

	results, err := runner.Run(sc)
	if err != nil {
		t.Fatalf("SustainedRunner.Run: %v", err)
	}

	byMetric := make(map[Metric]Result, len(results))
	for _, r := range results {
		byMetric[r.Metric] = r
	}

	// The latency + error-rate targets carry a Max, so the canonical
	// pass/fail is exactly EvaluateTarget — the same gate RunSuite and
	// the Tier 3 verifier use. Drive every Max/Min-bounded target
	// through it so this test stays in lockstep with production
	// evaluation semantics rather than re-implementing the comparison.
	for _, tgt := range sc.Targets {
		if tgt.Max == 0 && tgt.Min == 0 {
			continue // informational metric, asserted explicitly below
		}
		res, ok := byMetric[tgt.Metric]
		if !ok {
			t.Errorf("no Result produced for target metric %q", tgt.Metric)
			continue
		}
		if res.Pending {
			t.Errorf("metric %q reported Pending in smoke run (%s); it must be measurable against local_fs_dev",
				tgt.Metric, res.PendingReason)
			continue
		}
		if msg := EvaluateTarget(tgt, res.Value); msg != "" {
			t.Errorf("smoke SLA gate failed: %s", msg)
		}
	}

	// Sanity-check the latency histograms actually carry a sample so a
	// silently-empty run (e.g. all ops skipped) cannot pass the Max
	// gates with a 0 ms reading.
	for _, m := range []Metric{MetricPutP99, MetricGetP99} {
		res := byMetric[m]
		if res.Histogram == nil || res.Histogram.Count == 0 {
			t.Errorf("metric %q: expected a non-empty latency histogram, got %+v", m, res.Histogram)
		}
	}

	// rps_efficiency: loose lower bound. local_fs_dev at 1000 RPS
	// should let the rate limiter keep pace easily; a value well under
	// 1.0 would mean the provider path can't sustain a trivial rate.
	if eff := byMetric[MetricRPSEfficiency].Value; eff < 0.6 {
		t.Errorf("rps_efficiency = %.3f at TargetRPS=%d (sustained_rps=%.1f), want >= 0.6",
			eff, sc.Workload.TargetRPS, byMetric[MetricSustainedRPS].Value)
	}
}
