package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/tests/benchmark"
	"github.com/kennguy3n/zk-object-fabric/tests/tier3verify"
)

// goodScenario / goodReport mirror the test helpers in
// tests/tier3verify but live in this package so the CLI can be
// exercised end-to-end without importing _test code from another
// package (Go does not allow that).
func goodScenario(name string) benchmark.ReportScenario {
	thresholds := tier3verify.Tier3Thresholds()[name]
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
	return benchmark.ReportScenario{
		Name:    name,
		Pass:    true,
		Results: results,
	}
}

func writeReport(t *testing.T, dir string, mutate func(*benchmark.Report)) string {
	t.Helper()
	rpt := &benchmark.Report{
		Suite:      "tier3-staging",
		StartedAt:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC),
		AllPassed:  true,
	}
	for _, name := range tier3verify.RequiredScenarios {
		rpt.Scenarios = append(rpt.Scenarios, goodScenario(name))
	}
	if mutate != nil {
		mutate(rpt)
	}
	body, err := rpt.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}
	path := filepath.Join(dir, "report.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write report: %v", err)
	}
	return path
}

func TestRun_PassingReport(t *testing.T) {
	dir := t.TempDir()
	reportPath := writeReport(t, dir, nil)
	outPath := filepath.Join(dir, "verdict.json")

	var stdout, stderr bytes.Buffer
	err := run([]string{
		"-report", reportPath,
		"-out", outPath,
		"-build-sha", "deadbeef",
		"-env", "tier3-staging",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run(passing) returned error: %v\nstderr=%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "VERDICT: PASS") {
		t.Errorf("stderr missing PASS verdict line:\n%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout not empty when -out is set: %q", stdout.String())
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read verdict: %v", err)
	}
	var v tier3verify.Verdict
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("verdict json: %v", err)
	}
	if !v.AllPassed {
		t.Errorf("verdict.AllPassed = false")
	}
	if v.BuildSHA != "deadbeef" {
		t.Errorf("BuildSHA = %q, want deadbeef", v.BuildSHA)
	}
	if v.Environment != "tier3-staging" {
		t.Errorf("Environment = %q, want tier3-staging", v.Environment)
	}
}

func TestRun_FailingReport_ExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	reportPath := writeReport(t, dir, func(r *benchmark.Report) {
		// Force put-cache-hit p99 over the cap.
		for i := range r.Scenarios {
			if r.Scenarios[i].Name != "put-cache-hit" {
				continue
			}
			for j := range r.Scenarios[i].Results {
				if r.Scenarios[i].Results[j].Metric == benchmark.MetricPutP99CacheHit {
					r.Scenarios[i].Results[j].Value = benchmark.TargetPutP99CacheHitMs + 1
				}
			}
		}
	})

	var stdout, stderr bytes.Buffer
	err := run([]string{"-report", reportPath}, &stdout, &stderr)
	if err == nil {
		t.Fatalf("run(failing) returned nil error; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "VERDICT: FAIL") {
		t.Errorf("stderr missing FAIL verdict line:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "do NOT promote") {
		t.Errorf("stderr missing 'do NOT promote' guidance:\n%s", stderr.String())
	}
}

func TestRun_MissingReportFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(nil, &stdout, &stderr)
	if err == nil {
		t.Fatal("run() with no args returned nil error")
	}
	if !strings.Contains(err.Error(), "-report is required") {
		t.Errorf("error = %v, want '-report is required'", err)
	}
}

func TestRun_VerdictGoesToStdoutWhenNoOutFlag(t *testing.T) {
	dir := t.TempDir()
	reportPath := writeReport(t, dir, nil)

	var stdout, stderr bytes.Buffer
	err := run([]string{"-report", reportPath}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var v tier3verify.Verdict
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &v); err != nil {
		t.Fatalf("stdout not valid verdict JSON: %v\nbody=%s", err, stdout.String())
	}
	if v.ReportPath != reportPath {
		t.Errorf("stdout verdict ReportPath = %q, want %q", v.ReportPath, reportPath)
	}
}

func TestRun_TamperedAllPassedFlagIsDetected(t *testing.T) {
	dir := t.TempDir()
	reportPath := writeReport(t, dir, func(r *benchmark.Report) {
		// Tamper: leave a clearly-failing measured value but flip
		// AllPassed and Scenario.Pass to true. The verifier must
		// still reject.
		for i := range r.Scenarios {
			if r.Scenarios[i].Name != "get-origin" {
				continue
			}
			r.Scenarios[i].Pass = true
			for j := range r.Scenarios[i].Results {
				if r.Scenarios[i].Results[j].Metric == benchmark.MetricGetP99Origin {
					r.Scenarios[i].Results[j].Value = benchmark.TargetGetP99OriginMs + 100
				}
			}
		}
		r.AllPassed = true
	})

	var stdout, stderr bytes.Buffer
	err := run([]string{"-report", reportPath}, &stdout, &stderr)
	if err == nil {
		t.Fatal("verifier missed the tampered all_passed flag")
	}
	if !strings.Contains(stderr.String(), "verifier disagreed") {
		t.Errorf("stderr should call out the verifier-vs-report disagreement:\n%s", stderr.String())
	}
}
