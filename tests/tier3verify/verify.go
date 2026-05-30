// Package tier3verify is the audit-side verifier for the Tier 3
// (Linode + Wasabi) staging load run. It takes a
// benchmark.Report — typically the JSON file written by the
// benchmark-runner CLI on the staging load driver — and
// independently re-checks the five acceptance criteria the
// load-testing runbook (docs/runbooks/load-testing.md §4) gates
// the production promotion on:
//
//  1. Report.AllPassed == true.
//  2. All six Tier 3 scenarios are present in the report
//     (put-cache-hit, put-origin, get-l0-cache-hit,
//     get-l1-cache-hit, get-origin, sustained-throughput-10k-rps).
//  3. Every latency p99 metric is at or under the Target*Ms
//     constant declared in tests/benchmark/suite.go.
//  4. sustained-throughput-10k-rps attains
//     >= TargetSustainedRPS and >= TargetRPSEfficiencyMin and
//     <= TargetErrorRateMax.
//  5. No required metric is reported Pending. A Pending result is
//     a structural gap (e.g. the runner couldn't measure that
//     metric) and disqualifies the run for SLA gating even if
//     the surrounding scenario was otherwise green.
//
// The verifier re-applies the thresholds against the measured
// values in the report instead of trusting the report's own
// Pass / AllPassed flags, so an operator who hand-edits the
// JSON to flip the booleans without changing the underlying
// numbers will still fail verification. This makes the verifier
// suitable as the audit-side gate independent of the runner
// that produced the report.
//
// The verifier is intentionally independent of the providers
// package — it consumes only the JSON shape exposed by
// benchmark.Report and benchmark.ReportScenario, so the audit
// dossier can be verified without standing up a Wasabi or
// Linode credential set.
package tier3verify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/kennguy3n/zk-object-fabric/tests/benchmark"
)

// RequiredScenarios is the canonical Tier 3 scenario set the
// staging run must produce. A report missing any of these is
// rejected even if all the scenarios it did contain passed.
//
// Order is the order the verdict prints scenarios in, chosen to
// match the load-testing runbook's table.
var RequiredScenarios = []string{
	"put-cache-hit",
	"put-origin",
	"get-l0-cache-hit",
	"get-l1-cache-hit",
	"get-origin",
	"sustained-throughput-10k-rps",
}

// Threshold is the directional bound applied to a single metric.
// Min != 0 means the measured value must be at least Min; Max != 0
// means it must be at most Max. Exactly one of the two is set
// for any given (scenario, metric) pair in the Tier 3 contract.
type Threshold struct {
	Min  float64
	Max  float64
	Unit string
}

// Direction returns "min" or "max" describing which bound is
// in effect. Used only for the human-readable verdict output;
// the verifier itself always checks both Min and Max when set.
func (t Threshold) Direction() string {
	switch {
	case t.Min != 0:
		return "min"
	case t.Max != 0:
		return "max"
	default:
		return "unset"
	}
}

// Tier3Thresholds returns the per-(scenario, metric) Threshold
// map the verifier applies. Values come directly from the
// Target* constants in tests/benchmark/suite.go so changing a
// target there propagates here without a second edit. The
// returned map is freshly allocated on each call so callers can
// mutate it for tests without affecting subsequent invocations.
func Tier3Thresholds() map[string]map[benchmark.Metric]Threshold {
	return map[string]map[benchmark.Metric]Threshold{
		"put-cache-hit": {
			benchmark.MetricPutP99CacheHit: {Max: benchmark.TargetPutP99CacheHitMs, Unit: "ms"},
		},
		"put-origin": {
			benchmark.MetricPutP99Origin: {Max: benchmark.TargetPutP99OriginMs, Unit: "ms"},
		},
		"get-l0-cache-hit": {
			benchmark.MetricGetP99L0CacheHit: {Max: benchmark.TargetGetP99L0Ms, Unit: "ms"},
		},
		"get-l1-cache-hit": {
			benchmark.MetricGetP99L1CacheHit: {Max: benchmark.TargetGetP99L1Ms, Unit: "ms"},
		},
		"get-origin": {
			benchmark.MetricGetP99Origin: {Max: benchmark.TargetGetP99OriginMs, Unit: "ms"},
		},
		"sustained-throughput-10k-rps": {
			benchmark.MetricSustainedRPS:        {Min: benchmark.TargetSustainedRPS, Unit: "req/s"},
			benchmark.MetricRPSEfficiency:       {Min: benchmark.TargetRPSEfficiencyMin, Unit: "ratio"},
			benchmark.MetricErrorRate:           {Max: benchmark.TargetErrorRateMax, Unit: "ratio"},
			benchmark.MetricSkippedOpFraction:   {Max: 0.05, Unit: "ratio"},
		},
	}
}

// MetricVerdict is the per-metric independent check the verifier
// reports back. Measured / Threshold / Direction together let an
// auditor reproduce the pass/fail call by hand.
type MetricVerdict struct {
	Metric    string  `json:"metric"`
	Measured  float64 `json:"measured"`
	Threshold float64 `json:"threshold"`
	Direction string  `json:"direction"`
	Unit      string  `json:"unit"`
	Pending   bool    `json:"pending,omitempty"`
	Pass      bool    `json:"pass"`
	Reason    string  `json:"reason,omitempty"`
}

// ScenarioVerdict aggregates per-metric verdicts for one Tier 3
// scenario. Present=false means the scenario was not found in
// the report at all (a hard failure regardless of other fields).
type ScenarioVerdict struct {
	Name     string          `json:"name"`
	Present  bool            `json:"present"`
	Pass     bool            `json:"pass"`
	Metrics  []MetricVerdict `json:"metrics,omitempty"`
	Failures []string        `json:"failures,omitempty"`
}

// Verdict is the top-level audit verdict. AllPassed is the
// conjunction of every required scenario being present AND every
// required metric within its threshold AND no required metric
// being Pending.
type Verdict struct {
	Suite        string            `json:"suite"`
	ReportPath   string            `json:"report_path"`
	BuildSHA     string            `json:"build_sha,omitempty"`
	Environment  string            `json:"environment,omitempty"`
	StartedAt    string            `json:"report_started_at,omitempty"`
	FinishedAt   string            `json:"report_finished_at,omitempty"`
	AllPassed    bool              `json:"all_passed"`
	ReportClaim  bool              `json:"report_claim_all_passed"`
	Scenarios    []ScenarioVerdict `json:"scenarios"`
	Failures     []string          `json:"failures,omitempty"`
}

// Options carries optional metadata stamped onto the verdict so
// the dossier records which environment and gateway build the
// report came from.
type Options struct {
	ReportPath  string
	BuildSHA    string
	Environment string
}

// Verify produces a Verdict for the supplied report. The function
// is deterministic and side-effect-free; given the same inputs it
// returns the same output, which makes it suitable for snapshot
// testing.
func Verify(rpt *benchmark.Report, opts Options) Verdict {
	if rpt == nil {
		return Verdict{
			AllPassed:   false,
			ReportPath:  opts.ReportPath,
			BuildSHA:    opts.BuildSHA,
			Environment: opts.Environment,
			Failures:    []string{"report is nil"},
		}
	}

	v := Verdict{
		Suite:       rpt.Suite,
		ReportPath:  opts.ReportPath,
		BuildSHA:    opts.BuildSHA,
		Environment: opts.Environment,
		StartedAt:   rpt.StartedAt.UTC().Format("2006-01-02T15:04:05Z"),
		FinishedAt:  rpt.FinishedAt.UTC().Format("2006-01-02T15:04:05Z"),
		ReportClaim: rpt.AllPassed,
		AllPassed:   true,
	}

	byName := make(map[string]benchmark.ReportScenario, len(rpt.Scenarios))
	for _, sc := range rpt.Scenarios {
		byName[sc.Name] = sc
	}

	thresholds := Tier3Thresholds()
	for _, name := range RequiredScenarios {
		sv := verifyScenario(name, byName, thresholds[name])
		if !sv.Pass {
			v.AllPassed = false
		}
		v.Scenarios = append(v.Scenarios, sv)
	}

	if !rpt.AllPassed {
		v.AllPassed = false
		v.Failures = append(v.Failures, "report.AllPassed is false")
	}

	// Aggregate top-level failures from per-scenario verdicts so
	// the consumer can render a short summary without walking the
	// nested structure.
	for _, sc := range v.Scenarios {
		for _, f := range sc.Failures {
			v.Failures = append(v.Failures, sc.Name+": "+f)
		}
	}
	return v
}

func verifyScenario(name string, byName map[string]benchmark.ReportScenario, thresholds map[benchmark.Metric]Threshold) ScenarioVerdict {
	sc, ok := byName[name]
	if !ok {
		return ScenarioVerdict{
			Name:     name,
			Present:  false,
			Pass:     false,
			Failures: []string{"scenario missing from report"},
		}
	}
	sv := ScenarioVerdict{
		Name:    name,
		Present: true,
		Pass:    true,
	}

	measured := make(map[benchmark.Metric]benchmark.Result, len(sc.Results))
	for _, r := range sc.Results {
		measured[r.Metric] = r
	}

	// Stable ordering for the verdict so the dossier diffs cleanly
	// across runs.
	metricKeys := make([]benchmark.Metric, 0, len(thresholds))
	for m := range thresholds {
		metricKeys = append(metricKeys, m)
	}
	sort.Slice(metricKeys, func(i, j int) bool { return string(metricKeys[i]) < string(metricKeys[j]) })

	for _, m := range metricKeys {
		th := thresholds[m]
		mv := MetricVerdict{
			Metric:    string(m),
			Threshold: thresholdValue(th),
			Direction: th.Direction(),
			Unit:      th.Unit,
		}
		r, present := measured[m]
		if !present {
			mv.Pass = false
			mv.Reason = "metric missing from scenario result set"
			sv.Pass = false
			sv.Failures = append(sv.Failures, fmt.Sprintf("metric %s missing", m))
			sv.Metrics = append(sv.Metrics, mv)
			continue
		}
		mv.Measured = r.Value
		mv.Pending = r.Pending
		if r.Pending {
			mv.Pass = false
			mv.Reason = "metric reported pending; cannot gate SLA on an un-wired measurement"
			sv.Pass = false
			sv.Failures = append(sv.Failures, fmt.Sprintf("metric %s pending", m))
			sv.Metrics = append(sv.Metrics, mv)
			continue
		}
		if th.Max != 0 && r.Value > th.Max {
			mv.Pass = false
			mv.Reason = fmt.Sprintf("measured %g %s exceeds max threshold %g %s", r.Value, th.Unit, th.Max, th.Unit)
			sv.Pass = false
			sv.Failures = append(sv.Failures, mv.Reason)
			sv.Metrics = append(sv.Metrics, mv)
			continue
		}
		if th.Min != 0 && r.Value < th.Min {
			mv.Pass = false
			mv.Reason = fmt.Sprintf("measured %g %s below min threshold %g %s", r.Value, th.Unit, th.Min, th.Unit)
			sv.Pass = false
			sv.Failures = append(sv.Failures, mv.Reason)
			sv.Metrics = append(sv.Metrics, mv)
			continue
		}
		mv.Pass = true
		sv.Metrics = append(sv.Metrics, mv)
	}

	// Surface declared scenario failures from the report so an
	// auditor who scans only the verdict still sees them.
	if !sc.Pass {
		sv.Pass = false
		for _, f := range sc.Failures {
			sv.Failures = append(sv.Failures, "report-declared: "+f)
		}
	}
	return sv
}

func thresholdValue(t Threshold) float64 {
	if t.Min != 0 {
		return t.Min
	}
	return t.Max
}

// LoadReport reads and JSON-decodes a benchmark.Report from r.
// Returned errors are wrapped with context so the CLI can surface
// them verbatim.
func LoadReport(r io.Reader) (*benchmark.Report, error) {
	if r == nil {
		return nil, errors.New("tier3verify: nil reader")
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("tier3verify: read report: %w", err)
	}
	rpt := &benchmark.Report{}
	if err := json.Unmarshal(body, rpt); err != nil {
		return nil, fmt.Errorf("tier3verify: decode report: %w", err)
	}
	return rpt, nil
}

// WriteVerdict serialises v as indented JSON to w. The output is
// stable across runs.
func WriteVerdict(w io.Writer, v Verdict) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("tier3verify: marshal verdict: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("tier3verify: write verdict: %w", err)
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		return fmt.Errorf("tier3verify: write verdict newline: %w", err)
	}
	return nil
}
