// Package benchmark defines the benchmark harness structure for the
// ZK Object Fabric data plane. See docs/PROPOSAL.md §3.11 and
// docs/PROGRESS.md for the product-level targets this suite exists to
// enforce.
//
// Phase 1 ships this file as a specification: the Suite, Scenario,
// and Target types describe what Phase 2 will execute, not a working
// runner. Having the target metrics encoded in Go rather than prose
// lets the Phase 2 implementation be generated and checked against
// the product spec instead of re-interpreted per test run.
package benchmark

import (
	"fmt"
	"time"
)

// Suite is the top-level benchmark definition that Phase 2+ tooling
// materializes into concrete runs. A Suite is a named collection of
// Scenarios plus the Targets that a scenario must meet to pass.
type Suite struct {
	// Name is a stable identifier for the suite
	// (e.g. "phase2-single-cell").
	Name string

	// Scenarios are the individually runnable workloads. Every
	// scenario carries its own Targets so results can be judged
	// without consulting a separate rubric.
	Scenarios []Scenario
}

// Scenario is one runnable workload definition.
type Scenario struct {
	// Name is a stable identifier
	// (e.g. "put-1mb-objects", "get-1gb-range").
	Name string

	// Description is a one-line human summary of what the scenario
	// exercises.
	Description string

	// Workload describes the request mix.
	Workload Workload

	// Targets are the pass/fail thresholds. All targets must be met
	// for the scenario to be considered passing.
	Targets []Target
}

// Workload describes the shape of requests the benchmark driver will
// generate for a scenario.
type Workload struct {
	// RequestMix sums to 1.0. Keys are S3 operation names:
	// "PUT", "GET", "HEAD", "DELETE", "LIST".
	RequestMix map[string]float64

	// ObjectSizeBytes is the mean object size for the scenario.
	// Drivers may dither around this value; the mean must match.
	ObjectSizeBytes int64

	// RangeGETFraction is the fraction of GETs served as byte-range
	// reads. 0.0 means all GETs are full-object reads.
	RangeGETFraction float64

	// TenantCount is the number of distinct tenants simulated.
	TenantCount int

	// DurationSeconds is the length of the run.
	DurationSeconds int

	// TargetRPS is the aggregate steady-state request rate.
	TargetRPS int

	// ListObjectCount pre-populates a tenant with this many objects
	// before LIST scenarios execute. Used for the 10M / 100M / 1B
	// targets below.
	ListObjectCount int64

	// DedupHitFraction is the fraction of PUTs whose plaintext is
	// already known to the gateway when the scenario starts. The
	// driver pre-warms the content_index with HitFraction * total
	// PUT objects before the run begins so the steady-state hit
	// ratio is deterministic. 0.0 disables pre-warming. Used by
	// the Phase 3.5 B2C-80%-dup and B2B-60%-dup scenarios.
	DedupHitFraction float64
}

// Target is a single pass/fail assertion against a measured metric.
type Target struct {
	// Metric names the measurement; see the constants below.
	Metric Metric

	// Max is the upper bound that a measurement must stay at or
	// below. Zero means "no upper bound".
	Max float64

	// Min is the lower bound that a measurement must stay at or
	// above. Zero means "no lower bound".
	Min float64

	// Unit documents the unit of Max/Min
	// (e.g. "ms", "ratio", "objects_per_second").
	Unit string
}

// Metric is the canonical name for a product-level measurement.
type Metric string

const (
	// MetricPutP50 is the 50th-percentile PUT latency.
	MetricPutP50 Metric = "put_latency_p50"
	// MetricPutP95 is the 95th-percentile PUT latency.
	MetricPutP95 Metric = "put_latency_p95"
	// MetricPutP99 is the 99th-percentile PUT latency.
	MetricPutP99 Metric = "put_latency_p99"

	// MetricGetP50 is the 50th-percentile GET latency.
	MetricGetP50 Metric = "get_latency_p50"
	// MetricGetP95 is the 95th-percentile GET latency.
	MetricGetP95 Metric = "get_latency_p95"
	// MetricGetP99 is the 99th-percentile GET latency.
	MetricGetP99 Metric = "get_latency_p99"

	// MetricCacheHitRatioHot is the ratio of hot-tier cache hits to
	// total reads in that tier. The Phase 3 target is > 0.9.
	MetricCacheHitRatioHot Metric = "cache_hit_ratio_hot"

	// MetricWasabiOriginEgressRatio is the ratio of bytes read from
	// the Wasabi origin to bytes stored on Wasabi, per tenant, per
	// month. The Phase 2–3 target is <= 1.0 (the Wasabi fair-use
	// ceiling).
	MetricWasabiOriginEgressRatio Metric = "wasabi_origin_egress_ratio"

	// MetricListP95 is the 95th-percentile LIST latency.
	MetricListP95 Metric = "list_latency_p95"

	// MetricMigrationThroughput is the net ciphertext throughput of
	// the Wasabi → local cell migration engine in bytes/sec.
	MetricMigrationThroughput Metric = "migration_throughput_bytes_per_sec"

	// MetricRepairTimeSeconds is the wall-clock time, in seconds, to
	// recover a single failed storage node for a Phase 2+ local-DC
	// cell. Measured as the interval from node-loss detection to
	// restored durability targets.
	MetricRepairTimeSeconds Metric = "repair_time_seconds"

	// MetricNetworkCostUSDPerTB is the aggregate network cost in US
	// dollars per terabyte of ciphertext served (includes Wasabi
	// origin egress, Linode transit, and local-DC peering). Per
	// docs/PROGRESS.md "Key Metrics to Track".
	MetricNetworkCostUSDPerTB Metric = "network_cost_usd_per_tb"

	// MetricDedupHitRatio is the fraction of PUT requests that
	// landed on an existing content_index entry. The Phase 3.5
	// targets are 0.8 for the B2C scenario and 0.6 for the B2B
	// scenario.
	MetricDedupHitRatio Metric = "dedup_hit_ratio"

	// MetricDedupBytesSavedRatio is the fraction of inbound
	// plaintext bytes the gateway avoided writing to the backend
	// because of dedup. Reported as DedupBytesSaved / inboundBytes
	// over the scenario window.
	MetricDedupBytesSavedRatio Metric = "dedup_bytes_saved_ratio"

	// MetricDedupPutLatencyOverheadP95 is the additional p95 PUT
	// latency the content_index lookup adds compared to a dedup-
	// off baseline. Phase 3.5 cap is 5 ms.
	MetricDedupPutLatencyOverheadP95 Metric = "dedup_put_latency_overhead_p95"

	// MetricSustainedRPS is the attained aggregate request rate
	// (across all operations) during the steady-state run window.
	// The Workstream 1.1 target is >= 10,000 req/s per gateway
	// node for the canonical mixed scenario.
	MetricSustainedRPS Metric = "sustained_rps"

	// MetricRPSEfficiency is attained_rps / target_rps. A value
	// below 1.0 indicates the gateway could not keep up with the
	// declared offered load; values close to 1.0 indicate the
	// runner reached the rate ceiling.
	MetricRPSEfficiency Metric = "rps_efficiency"

	// MetricErrorRate is errored_requests / total_requests over
	// the steady-state window. Workstream 1.1 caps it at 1e-3.
	MetricErrorRate Metric = "error_rate"

	// MetricSkippedOpFraction is skipped_ops / (attempts + skipped),
	// where a skipped op is one drawn from the request mix but
	// not actually executed because its precondition was not met
	// (currently: GET / HEAD / DELETE drawn while the working
	// set is empty). It is an informational signal — typically
	// non-zero only in the brief ramp window before PUTs have
	// seeded the working set, or in mis-configured scenarios
	// that draw reads against a never-seeded provider. A high
	// value means the reported attempted-RPS is far below the
	// configured TargetRPS (rate-limiter tokens were spent on
	// no-ops) and the histograms were built from a smaller
	// success sample than expected. The harness reports this
	// value as informational; scenarios may set Max if they
	// want a hard gate.
	MetricSkippedOpFraction Metric = "skipped_op_fraction"

	// MetricPutP99CacheHit is the 99th-percentile PUT latency
	// when the gateway's hot-tier cache is warm. Workstream 1.1
	// caps it at 50 ms. The harness selects this metric for
	// scenarios that pre-warm the cache before measuring.
	MetricPutP99CacheHit Metric = "put_latency_p99_cache_hit"

	// MetricPutP99Origin is the 99th-percentile PUT latency when
	// the gateway must round-trip to the Wasabi origin (cold
	// cache). Workstream 1.1 caps it at 200 ms.
	MetricPutP99Origin Metric = "put_latency_p99_origin"

	// MetricGetP99L0CacheHit is the 99th-percentile GET latency
	// when the L0 (process-local) cache serves the read.
	// Workstream 1.1 caps it at 20 ms.
	MetricGetP99L0CacheHit Metric = "get_latency_p99_l0_cache_hit"

	// MetricGetP99L1CacheHit is the 99th-percentile GET latency
	// when the L1 (NVMe disk) cache serves the read. Workstream
	// 1.1 caps it at 100 ms.
	MetricGetP99L1CacheHit Metric = "get_latency_p99_l1_cache_hit"

	// MetricGetP99Origin is the 99th-percentile GET latency when
	// the gateway misses both caches and round-trips to Wasabi.
	// Workstream 1.1 caps it at 300 ms.
	MetricGetP99Origin Metric = "get_latency_p99_origin"
)

// Target values drawn from docs/PROPOSAL.md §3.11 and
// docs/PROGRESS.md "Key Metrics to Track". These are declarative; the
// actual numbers are TBD until Phase 2 produces real measurements.
// The constants exist so that Phase 2 Scenarios can be constructed
// programmatically against the documented targets rather than ad hoc
// literals.
const (
	// TargetCacheHitRatioHotMin is the Phase 3 Hot-tier cache hit
	// ratio target: > 0.9 of reads served from the cache.
	TargetCacheHitRatioHotMin = 0.9

	// TargetWasabiOriginEgressRatioMax is the Phase 2–3 Wasabi
	// origin egress ratio ceiling: egress bytes <= stored bytes per
	// tenant per month.
	TargetWasabiOriginEgressRatioMax = 1.0

	// TargetPutP99CacheHitMs is the Workstream 1.1 cap on PUT
	// 99th-percentile latency when the destination cell is the
	// hot tier (cache hit on the way out).
	TargetPutP99CacheHitMs = 50.0

	// TargetPutP99OriginMs is the Workstream 1.1 cap on PUT
	// 99th-percentile latency when the destination cell is the
	// Wasabi origin (cache miss on the way out).
	TargetPutP99OriginMs = 200.0

	// TargetGetP99L0Ms is the Workstream 1.1 cap on GET 99th-
	// percentile latency for L0 (process-local memory) cache
	// hits.
	TargetGetP99L0Ms = 20.0

	// TargetGetP99L1Ms is the Workstream 1.1 cap on GET 99th-
	// percentile latency for L1 (NVMe disk) cache hits.
	TargetGetP99L1Ms = 100.0

	// TargetGetP99OriginMs is the Workstream 1.1 cap on GET
	// 99th-percentile latency when the gateway must reach
	// Wasabi.
	TargetGetP99OriginMs = 300.0

	// TargetSustainedRPS is the Workstream 1.1 floor on
	// sustained aggregate request rate per gateway node.
	TargetSustainedRPS = 10_000.0

	// TargetErrorRateMax is the Workstream 1.1 ceiling on the
	// per-request error rate during a sustained-load run.
	TargetErrorRateMax = 1e-3

	// TargetRPSEfficiencyMin is the Workstream 1.1 floor on
	// rps_efficiency. A run that attains less than this
	// fraction of its declared TargetRPS is reported as a fail
	// even if all latency targets passed, because it means the
	// gateway could not keep up with the offered load.
	TargetRPSEfficiencyMin = 0.95

	// ListSize10M is the list scenario size for 10M objects.
	ListSize10M int64 = 10_000_000

	// ListSize100M is the list scenario size for 100M objects.
	ListSize100M int64 = 100_000_000

	// ListSize1B is the list scenario size for 1B objects.
	ListSize1B int64 = 1_000_000_000
)

// Result is one measured point that the driver reports back for later
// comparison against a Target.
type Result struct {
	Metric   Metric            `json:"metric"`
	Value    float64           `json:"value"`
	Duration time.Duration     `json:"duration_ns"`
	Labels   map[string]string `json:"labels,omitempty"`
	// Histogram is the per-operation latency distribution the
	// metric was computed from. Latency-shaped metrics populate
	// it; counters (RPS, ratios, error rate) leave it nil.
	Histogram *HistogramSummary `json:"histogram,omitempty"`
	// Pending is set by a runner when it cannot yet measure the
	// requested metric — e.g. SustainedRunner asked to measure a
	// dedup hit ratio against a raw StorageProvider, where dedup
	// is enforced one layer up in the gateway and is structurally
	// invisible to the provider interface. RunSuite skips
	// EvaluateTarget for Pending=true results and tallies them in
	// ReportScenario.Pending rather than failing the scenario.
	// Zero value (false) means "measured normally" so existing
	// runners stay correct without a code change.
	Pending bool `json:"pending,omitempty"`
	// PendingReason is an optional human-readable explanation of
	// why the runner could not measure this metric. Populated by
	// runners that mark a result Pending so report consumers can
	// distinguish "not wired in yet" from "not applicable to this
	// runner" without re-reading the runner source. Empty when
	// Pending is false.
	PendingReason string `json:"pending_reason,omitempty"`
}

// Runner executes a Suite against a single StorageProvider and
// returns per-scenario Results. Phase 1 ships the interface only;
// Phase 2 supplies a concrete driver that generates load, measures
// latency, aggregates counters, and emits Results for downstream
// Target evaluation.
//
// The interface is intentionally provider-shaped rather than
// gateway-shaped so a benchmark can be pointed at any
// providers.StorageProvider (wasabi, local_fs_dev, ceph_rgw, etc.)
// without standing up the full S3 handler stack.
type Runner interface {
	// Run executes scenario and returns one Result per Target.
	Run(scenario Scenario) ([]Result, error)
}

// EvaluateTarget checks a measured value against a Target and
// reports a human-readable failure message (empty string means pass).
// Zero Max or Min means "no bound on that side".
func EvaluateTarget(t Target, value float64) string {
	if t.Max != 0 && value > t.Max {
		return fmt.Sprintf("%s = %v %s exceeds max %v", t.Metric, value, t.Unit, t.Max)
	}
	if t.Min != 0 && value < t.Min {
		return fmt.Sprintf("%s = %v %s below min %v", t.Metric, value, t.Unit, t.Min)
	}
	return ""
}

// Validate performs structural checks on the suite definition.
//
// A Suite is valid when it has a name, at least one scenario, and
// every scenario has:
//   - a non-empty name,
//   - a RequestMix that sums to 1.0 (±1e-6),
//   - a positive duration and target RPS, and
//   - at least one target that names a known Metric.
func (s Suite) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("benchmark: suite name is required")
	}
	if len(s.Scenarios) == 0 {
		return fmt.Errorf("benchmark: suite %q has no scenarios", s.Name)
	}
	seen := map[string]bool{}
	for i, sc := range s.Scenarios {
		if sc.Name == "" {
			return fmt.Errorf("benchmark: suite %q scenario[%d] name is required", s.Name, i)
		}
		if seen[sc.Name] {
			return fmt.Errorf("benchmark: suite %q scenario name %q is duplicated", s.Name, sc.Name)
		}
		seen[sc.Name] = true
		if err := sc.validate(); err != nil {
			return fmt.Errorf("benchmark: suite %q scenario %q: %w", s.Name, sc.Name, err)
		}
	}
	return nil
}

func (sc Scenario) validate() error {
	total := 0.0
	for op, fraction := range sc.Workload.RequestMix {
		if fraction < 0 {
			return fmt.Errorf("request_mix[%q] is negative (%v)", op, fraction)
		}
		total += fraction
	}
	if len(sc.Workload.RequestMix) > 0 {
		if diff := total - 1.0; diff < -1e-6 || diff > 1e-6 {
			return fmt.Errorf("request_mix must sum to 1.0, got %v", total)
		}
	}
	if sc.Workload.DurationSeconds <= 0 {
		return fmt.Errorf("workload.duration_seconds must be > 0")
	}
	if sc.Workload.TargetRPS <= 0 {
		return fmt.Errorf("workload.target_rps must be > 0")
	}
	if len(sc.Targets) == 0 {
		return fmt.Errorf("at least one target is required")
	}
	for i, t := range sc.Targets {
		if t.Metric == "" {
			return fmt.Errorf("target[%d].metric is required", i)
		}
	}
	return nil
}

// DefaultSuite returns the canonical benchmark suite. Targets that
// are still under negotiation (LIST p95 against 10M/100M/1B object
// catalogues, dedup ratios) are recorded without gates so the
// harness publishes a number even when the run-time SLA has not
// been set. The latency, throughput and error-rate targets
// referenced by Workstream 1.1 are gated.
func DefaultSuite() Suite {
	return Suite{
		Name: "zk-object-fabric-phase2",
		Scenarios: []Scenario{
			{
				Name:        "put-get-latency",
				Description: "Steady-state mixed PUT/GET, gating the production p99 targets defined in Workstream 1.1.",
				Workload: Workload{
					RequestMix:      map[string]float64{"PUT": 0.3, "GET": 0.7},
					ObjectSizeBytes: 1024 * 1024, // 1 MiB
					TenantCount:     4,
					DurationSeconds: 600,
					TargetRPS:       1000,
				},
				Targets: []Target{
					{Metric: MetricPutP50, Unit: "ms"},
					{Metric: MetricPutP95, Unit: "ms"},
					{Metric: MetricPutP99, Max: TargetPutP99OriginMs, Unit: "ms"},
					{Metric: MetricGetP50, Unit: "ms"},
					{Metric: MetricGetP95, Unit: "ms"},
					{Metric: MetricGetP99, Max: TargetGetP99OriginMs, Unit: "ms"},
				},
			},
			{
				Name:        "put-cache-hit",
				Description: "Hot-tier-warm PUT workload: gates PUT p99 at the cache-hit SLA from Workstream 1.1.",
				Workload: Workload{
					RequestMix:      map[string]float64{"PUT": 1.0},
					ObjectSizeBytes: 512 * 1024, // 512 KiB
					TenantCount:     2,
					DurationSeconds: 600,
					TargetRPS:       1000,
				},
				Targets: []Target{
					{Metric: MetricPutP99CacheHit, Max: TargetPutP99CacheHitMs, Unit: "ms"},
				},
			},
			{
				Name:        "put-origin",
				Description: "Cold-cache PUT workload that round-trips to the Wasabi origin: gates PUT p99 at the origin SLA.",
				Workload: Workload{
					RequestMix:      map[string]float64{"PUT": 1.0},
					ObjectSizeBytes: 4 * 1024 * 1024, // 4 MiB
					TenantCount:     4,
					DurationSeconds: 600,
					TargetRPS:       500,
				},
				Targets: []Target{
					{Metric: MetricPutP99Origin, Max: TargetPutP99OriginMs, Unit: "ms"},
				},
			},
			{
				Name:        "get-l0-cache-hit",
				Description: "GET workload served exclusively from the L0 (memory) cache: gates GET p99 at the L0 SLA.",
				Workload: Workload{
					RequestMix:      map[string]float64{"GET": 1.0},
					ObjectSizeBytes: 256 * 1024, // 256 KiB
					TenantCount:     2,
					DurationSeconds: 600,
					TargetRPS:       2000,
				},
				Targets: []Target{
					{Metric: MetricGetP99L0CacheHit, Max: TargetGetP99L0Ms, Unit: "ms"},
				},
			},
			{
				Name:        "get-l1-cache-hit",
				Description: "GET workload served from the L1 (NVMe disk) cache: gates GET p99 at the L1 SLA.",
				Workload: Workload{
					RequestMix:      map[string]float64{"GET": 1.0},
					ObjectSizeBytes: 1024 * 1024, // 1 MiB
					TenantCount:     2,
					DurationSeconds: 600,
					TargetRPS:       1500,
				},
				Targets: []Target{
					{Metric: MetricGetP99L1CacheHit, Max: TargetGetP99L1Ms, Unit: "ms"},
				},
			},
			{
				Name:        "get-origin",
				Description: "Cold-cache GET workload that round-trips to the Wasabi origin: gates GET p99 at the origin SLA.",
				Workload: Workload{
					RequestMix:      map[string]float64{"GET": 1.0},
					ObjectSizeBytes: 4 * 1024 * 1024, // 4 MiB
					TenantCount:     4,
					DurationSeconds: 600,
					TargetRPS:       500,
				},
				Targets: []Target{
					{Metric: MetricGetP99Origin, Max: TargetGetP99OriginMs, Unit: "ms"},
				},
			},
			{
				Name:        "sustained-throughput-10k-rps",
				Description: "Sustained mixed workload at 10K req/s per gateway node: gates throughput floor, efficiency, and error rate per Workstream 1.1.",
				Workload: Workload{
					RequestMix:      map[string]float64{"PUT": 0.2, "GET": 0.7, "HEAD": 0.1},
					ObjectSizeBytes: 64 * 1024, // 64 KiB
					TenantCount:     8,
					DurationSeconds: 600,
					TargetRPS:       10000,
				},
				Targets: []Target{
					{Metric: MetricSustainedRPS, Min: TargetSustainedRPS, Unit: "req/s"},
					{Metric: MetricRPSEfficiency, Min: TargetRPSEfficiencyMin, Unit: "ratio"},
					{Metric: MetricErrorRate, Max: TargetErrorRateMax, Unit: "ratio"},
					// Hard gate the no-op skip fraction at 5% for
					// the 10K-RPS scenario. The seeded working set
					// (SeedObjects defaults to max(64, TargetRPS/10)
					// = 1000 keys at 10K RPS) plus the 20% PUT
					// share should keep skips well under that bar;
					// a higher value would indicate the early-ramp
					// PUTs are not seeding fast enough and the GET
					// histograms are being built from a smaller
					// sample than expected (see MetricSkippedOpFraction).
					{Metric: MetricSkippedOpFraction, Max: 0.05, Unit: "ratio"},
				},
			},
			{
				Name:        "cache-hit-ratio-hot",
				Description: "Repeated-read workload to verify Hot-tier cache hit ratio > 90%.",
				Workload: Workload{
					RequestMix:      map[string]float64{"GET": 1.0},
					ObjectSizeBytes: 4 * 1024 * 1024, // 4 MiB
					TenantCount:     2,
					DurationSeconds: 600,
					TargetRPS:       500,
				},
				Targets: []Target{
					{
						Metric: MetricCacheHitRatioHot,
						Min:    TargetCacheHitRatioHotMin,
						Unit:   "ratio",
					},
				},
			},
			{
				Name:        "wasabi-origin-egress-ratio",
				Description: "Sustained mixed workload verifying monthly Wasabi origin egress <= stored bytes per tenant.",
				Workload: Workload{
					RequestMix:      map[string]float64{"PUT": 0.2, "GET": 0.8},
					ObjectSizeBytes: 8 * 1024 * 1024, // 8 MiB
					TenantCount:     16,
					DurationSeconds: 86400, // 24h
					TargetRPS:       2000,
				},
				Targets: []Target{
					{
						Metric: MetricWasabiOriginEgressRatio,
						Max:    TargetWasabiOriginEgressRatioMax,
						Unit:   "ratio",
					},
				},
			},
			{
				Name:        "list-performance-10m",
				Description: "LIST performance at 10M objects under one tenant/bucket.",
				Workload: Workload{
					RequestMix:      map[string]float64{"LIST": 1.0},
					ObjectSizeBytes: 0,
					TenantCount:     1,
					DurationSeconds: 300,
					TargetRPS:       50,
					ListObjectCount: ListSize10M,
				},
				Targets: []Target{
					{Metric: MetricListP95, Unit: "ms"},
				},
			},
			{
				Name:        "list-performance-100m",
				Description: "LIST performance at 100M objects under one tenant/bucket.",
				Workload: Workload{
					RequestMix:      map[string]float64{"LIST": 1.0},
					ObjectSizeBytes: 0,
					TenantCount:     1,
					DurationSeconds: 300,
					TargetRPS:       20,
					ListObjectCount: ListSize100M,
				},
				Targets: []Target{
					{Metric: MetricListP95, Unit: "ms"},
				},
			},
			{
				Name:        "list-performance-1b",
				Description: "LIST performance at 1B objects under one tenant/bucket.",
				Workload: Workload{
					RequestMix:      map[string]float64{"LIST": 1.0},
					ObjectSizeBytes: 0,
					TenantCount:     1,
					DurationSeconds: 600,
					TargetRPS:       5,
					ListObjectCount: ListSize1B,
				},
				Targets: []Target{
					{Metric: MetricListP95, Unit: "ms"},
				},
			},
			{
				Name:        "dedup-b2c-80pct",
				Description: "Synthetic B2C workload with 80% duplicate plaintext; measures dedup hit ratio, bytes saved, and PUT latency overhead.",
				Workload: Workload{
					RequestMix:       map[string]float64{"PUT": 0.6, "GET": 0.4},
					ObjectSizeBytes:  256 * 1024, // 256 KiB — typical media thumbnail
					TenantCount:      4,
					DurationSeconds:  600,
					TargetRPS:        500,
					DedupHitFraction: 0.8,
				},
				Targets: []Target{
					{Metric: MetricDedupHitRatio, Min: 0.75, Unit: "ratio"},
					{Metric: MetricDedupBytesSavedRatio, Min: 0.7, Unit: "ratio"},
					{Metric: MetricDedupPutLatencyOverheadP95, Max: 5.0, Unit: "ms"},
				},
			},
			{
				Name:        "dedup-b2b-60pct",
				Description: "Synthetic B2B workload with 60% duplicate plaintext (e.g. scheduled backups, log shipping); measures dedup hit ratio, bytes saved, and PUT latency overhead.",
				Workload: Workload{
					RequestMix:       map[string]float64{"PUT": 0.5, "GET": 0.5},
					ObjectSizeBytes:  4 * 1024 * 1024, // 4 MiB — typical archive shard
					TenantCount:      8,
					DurationSeconds:  600,
					TargetRPS:        300,
					DedupHitFraction: 0.6,
				},
				Targets: []Target{
					{Metric: MetricDedupHitRatio, Min: 0.55, Unit: "ratio"},
					{Metric: MetricDedupBytesSavedRatio, Min: 0.5, Unit: "ratio"},
					{Metric: MetricDedupPutLatencyOverheadP95, Max: 5.0, Unit: "ms"},
				},
			},
		},
	}
}
