package tier3verify

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/tests/benchmark"
)

// goodScenario builds a ReportScenario with each Tier 3 metric
// measured comfortably within its threshold. Tests then perturb
// individual values via the mutate callback.
func goodScenario(name string, mutate func(*benchmark.ReportScenario)) benchmark.ReportScenario {
	thresholds := Tier3Thresholds()[name]
	results := make([]benchmark.Result, 0, len(thresholds))
	for m, th := range thresholds {
		var v float64
		switch {
		case th.Max > 0:
			v = th.Max * 0.5
		case th.Min > 0:
			v = th.Min * 1.5
		}
		results = append(results, benchmark.Result{
			Metric: m,
			Value:  v,
			Labels: map[string]string{"unit": th.Unit},
		})
	}
	sc := benchmark.ReportScenario{
		Name:    name,
		Pass:    true,
		Results: results,
	}
	if mutate != nil {
		mutate(&sc)
	}
	return sc
}

// goodReport builds a fully-passing Tier 3 report.
func goodReport(mutate func(*benchmark.Report)) *benchmark.Report {
	rpt := &benchmark.Report{
		Suite:      "tier3-staging",
		StartedAt:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC),
		AllPassed:  true,
	}
	for _, name := range RequiredScenarios {
		rpt.Scenarios = append(rpt.Scenarios, goodScenario(name, nil))
	}
	if mutate != nil {
		mutate(rpt)
	}
	return rpt
}

func TestVerify_PassingReport(t *testing.T) {
	v := Verify(goodReport(nil), Options{
		ReportPath:  "dossier/report.json",
		BuildSHA:    "abcdef1",
		Environment: "tier3-staging",
	})
	if !v.AllPassed {
		t.Fatalf("Verify(good) AllPassed = false, want true; failures = %v", v.Failures)
	}
	if !v.ReportClaim {
		t.Errorf("ReportClaim = false, want true")
	}
	if got, want := len(v.Scenarios), len(RequiredScenarios); got != want {
		t.Fatalf("len(Scenarios) = %d, want %d", got, want)
	}
	for _, sc := range v.Scenarios {
		if !sc.Present {
			t.Errorf("scenario %s Present = false", sc.Name)
		}
		if !sc.Pass {
			t.Errorf("scenario %s Pass = false; metrics = %+v", sc.Name, sc.Metrics)
		}
	}
}

func TestVerify_MissingScenarioFails(t *testing.T) {
	v := Verify(goodReport(func(r *benchmark.Report) {
		// Drop the sustained-throughput scenario entirely.
		filtered := r.Scenarios[:0]
		for _, sc := range r.Scenarios {
			if sc.Name != "sustained-throughput-10k-rps" {
				filtered = append(filtered, sc)
			}
		}
		r.Scenarios = filtered
	}), Options{})
	if v.AllPassed {
		t.Fatalf("Verify(missing scenario) AllPassed = true, want false")
	}
	var foundMissing bool
	for _, sc := range v.Scenarios {
		if sc.Name == "sustained-throughput-10k-rps" {
			if sc.Present {
				t.Errorf("missing scenario Present = true")
			}
			if sc.Pass {
				t.Errorf("missing scenario Pass = true")
			}
			foundMissing = true
		}
	}
	if !foundMissing {
		t.Errorf("verdict did not include scenario 'sustained-throughput-10k-rps' as missing")
	}
}

func TestVerify_LatencyOverMaxFails(t *testing.T) {
	v := Verify(goodReport(func(r *benchmark.Report) {
		for i := range r.Scenarios {
			if r.Scenarios[i].Name != "put-cache-hit" {
				continue
			}
			for j := range r.Scenarios[i].Results {
				if r.Scenarios[i].Results[j].Metric == benchmark.MetricPutP99CacheHit {
					// Push p99 over the cap.
					r.Scenarios[i].Results[j].Value = benchmark.TargetPutP99CacheHitMs + 1
				}
			}
		}
	}), Options{})
	if v.AllPassed {
		t.Fatalf("Verify(p99 over cap) AllPassed = true, want false")
	}
	if !containsContains(v.Failures, "exceeds max threshold") {
		t.Errorf("verdict failures missing 'exceeds max threshold': %v", v.Failures)
	}
}

func TestVerify_SustainedRPSUnderFloorFails(t *testing.T) {
	v := Verify(goodReport(func(r *benchmark.Report) {
		for i := range r.Scenarios {
			if r.Scenarios[i].Name != "sustained-throughput-10k-rps" {
				continue
			}
			for j := range r.Scenarios[i].Results {
				if r.Scenarios[i].Results[j].Metric == benchmark.MetricSustainedRPS {
					r.Scenarios[i].Results[j].Value = benchmark.TargetSustainedRPS - 1
				}
			}
		}
	}), Options{})
	if v.AllPassed {
		t.Fatalf("Verify(rps under floor) AllPassed = true, want false")
	}
	if !containsContains(v.Failures, "below min threshold") {
		t.Errorf("verdict failures missing 'below min threshold': %v", v.Failures)
	}
}

func TestVerify_PendingMetricFails(t *testing.T) {
	v := Verify(goodReport(func(r *benchmark.Report) {
		for i := range r.Scenarios {
			if r.Scenarios[i].Name != "sustained-throughput-10k-rps" {
				continue
			}
			for j := range r.Scenarios[i].Results {
				if r.Scenarios[i].Results[j].Metric == benchmark.MetricSustainedRPS {
					r.Scenarios[i].Results[j].Pending = true
					r.Scenarios[i].Results[j].PendingReason = "runner not wired"
				}
			}
		}
	}), Options{})
	if v.AllPassed {
		t.Fatalf("Verify(pending metric) AllPassed = true, want false")
	}
	if !containsContains(v.Failures, "pending") {
		t.Errorf("verdict failures missing 'pending': %v", v.Failures)
	}
}

func TestVerify_AllPassedFlagTamperedDetected(t *testing.T) {
	// Operator flipped AllPassed to true but ALSO left a measured
	// latency value over the cap. The verifier must reject because
	// it re-applies the thresholds independently.
	rpt := goodReport(func(r *benchmark.Report) {
		for i := range r.Scenarios {
			if r.Scenarios[i].Name != "get-origin" {
				continue
			}
			for j := range r.Scenarios[i].Results {
				if r.Scenarios[i].Results[j].Metric == benchmark.MetricGetP99Origin {
					r.Scenarios[i].Results[j].Value = benchmark.TargetGetP99OriginMs + 50
				}
			}
		}
	})
	rpt.AllPassed = true // Operator-tampered claim.
	v := Verify(rpt, Options{})
	if v.AllPassed {
		t.Fatalf("Verify(tampered AllPassed) AllPassed = true, want false")
	}
	if !v.ReportClaim {
		t.Errorf("ReportClaim = false, want true (we want to record the operator's claim faithfully)")
	}
}

func TestVerify_ReportClaimFalseAlsoFails(t *testing.T) {
	rpt := goodReport(func(r *benchmark.Report) {
		r.AllPassed = false
	})
	v := Verify(rpt, Options{})
	if v.AllPassed {
		t.Fatalf("Verify(report claim false) AllPassed = true, want false")
	}
	if !containsContains(v.Failures, "report.AllPassed is false") {
		t.Errorf("missing 'report.AllPassed is false' failure: %v", v.Failures)
	}
}

func TestVerify_NilReport(t *testing.T) {
	v := Verify(nil, Options{ReportPath: "missing.json"})
	if v.AllPassed {
		t.Fatal("Verify(nil) AllPassed = true, want false")
	}
	if v.ReportPath != "missing.json" {
		t.Errorf("ReportPath = %q, want missing.json", v.ReportPath)
	}
}

func TestLoadReportRoundTrip(t *testing.T) {
	rpt := goodReport(nil)
	body, err := rpt.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}
	got, err := LoadReport(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("LoadReport: %v", err)
	}
	if got.Suite != rpt.Suite {
		t.Errorf("Suite = %q, want %q", got.Suite, rpt.Suite)
	}
	if got.AllPassed != rpt.AllPassed {
		t.Errorf("AllPassed = %v, want %v", got.AllPassed, rpt.AllPassed)
	}
	if len(got.Scenarios) != len(rpt.Scenarios) {
		t.Errorf("len(Scenarios) = %d, want %d", len(got.Scenarios), len(rpt.Scenarios))
	}
}

func TestLoadReport_InvalidJSON(t *testing.T) {
	if _, err := LoadReport(strings.NewReader("{not json")); err == nil {
		t.Fatal("LoadReport(invalid) returned nil error")
	}
}

func TestWriteVerdict(t *testing.T) {
	var buf bytes.Buffer
	v := Verify(goodReport(nil), Options{ReportPath: "report.json"})
	if err := WriteVerdict(&buf, v); err != nil {
		t.Fatalf("WriteVerdict: %v", err)
	}
	// Ensure the written verdict is parseable JSON ending with newline.
	if !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Errorf("verdict not newline-terminated")
	}
	var round Verdict
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &round); err != nil {
		t.Fatalf("verdict not valid JSON: %v", err)
	}
	if round.ReportPath != "report.json" {
		t.Errorf("round.ReportPath = %q, want report.json", round.ReportPath)
	}
}

func TestVerify_StableScenarioOrdering(t *testing.T) {
	rpt := goodReport(func(r *benchmark.Report) {
		// Reverse the scenario order in the report — the verdict
		// must still emit RequiredScenarios in canonical order so
		// dossier diffs stay clean across runs.
		for i, j := 0, len(r.Scenarios)-1; i < j; i, j = i+1, j-1 {
			r.Scenarios[i], r.Scenarios[j] = r.Scenarios[j], r.Scenarios[i]
		}
	})
	v := Verify(rpt, Options{})
	for i, sc := range v.Scenarios {
		if sc.Name != RequiredScenarios[i] {
			t.Errorf("verdict[%d].Name = %q, want %q", i, sc.Name, RequiredScenarios[i])
		}
	}
}

func TestTier3Thresholds_FreshCopy(t *testing.T) {
	a := Tier3Thresholds()
	a["put-cache-hit"][benchmark.MetricPutP99CacheHit] = Threshold{Max: 1, Unit: "ms"}
	b := Tier3Thresholds()
	if b["put-cache-hit"][benchmark.MetricPutP99CacheHit].Max == 1 {
		t.Fatal("Tier3Thresholds returned a shared map; mutation leaked across calls")
	}
}

func containsContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
