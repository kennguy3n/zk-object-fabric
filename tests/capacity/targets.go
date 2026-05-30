// Package capacity is the single source of truth for the zk-object-fabric
// capacity dossier: every numeric target the gateway commits to (latency
// floors, throughput ceilings, per-region and per-tenant capacity envelopes,
// operational SLOs) lives here.
//
// Why a dedicated package and not just constants strewn across the
// implementation:
//
//   - The dossier is consumed by external auditors and prospective
//     customers, not just by tests. A reviewer reading the audit bundle
//     needs to see every committed number in one place and trust that
//     "what the doc says" and "what the gate enforces" are the same value.
//
//   - The complement is also true: when a target value is changed, the
//     change must surface as a single, reviewable diff in this file.
//     Touching a constant breaks tests/capacity/targets_test.go AND
//     tests/capacity/dossier_test.go (the doc-grep check), forcing the
//     PR author to update docs/CAPACITY.md and any cross-referencing
//     runbook in the same commit.
//
//   - Performance targets that are already enforced by the in-process
//     suite (tests/benchmark/suite.go) are RE-EXPORTED here rather than
//     duplicated. Aliasing prevents accidental value drift between the
//     enforcement gate and the dossier; tests/capacity/targets_test.go
//     pins the aliases so a rename of the upstream constant breaks the
//     build instead of silently producing a stale dossier.
//
// The dossier is intentionally narrow on durability. Per
// docs/PROPOSAL.md §11.4.x ("Anti-patterns to avoid"):
//
//	"Publish theoretical 'eleven nines' durability — Cannot be
//	 validated in Phase 1. Only publish measured durability from
//	 chaos tests."
//
// We therefore do NOT publish a theoretical durability nines target
// here. Durability lives in the chaos-test measurement reports
// produced under WS1.2 (PR #76) and is recorded in the audit bundle
// as measured, not committed.
package capacity

import "github.com/kennguy3n/zk-object-fabric/tests/benchmark"

// ----------------------------------------------------------------------
// §1. Performance targets — re-exported from tests/benchmark/suite.go.
//
// These are the values machine-enforced by cmd/benchmark-runner against
// every sustained-load run (see docs/runbooks/load-testing.md). The
// dossier surfaces them under one stable import path so that
// cmd/capacity-report and downstream auditors do not have to know
// which package defined which target.
// ----------------------------------------------------------------------

const (
	// PutP99CacheHitMs is the 99th-percentile PUT latency ceiling
	// (in milliseconds) when the destination cell can absorb the
	// write into a hot-tier cache without spilling to the origin.
	// Enforced by cmd/benchmark-runner via benchmark.TargetPutP99CacheHitMs.
	PutP99CacheHitMs = benchmark.TargetPutP99CacheHitMs

	// PutP99OriginMs is the 99th-percentile PUT latency ceiling (ms)
	// when the write must traverse the Wasabi origin (cache miss on
	// the way out). Enforced by cmd/benchmark-runner.
	PutP99OriginMs = benchmark.TargetPutP99OriginMs

	// GetP99L0Ms is the 99th-percentile GET latency ceiling (ms) for
	// the L0 (process-local memory) cache hit path.
	GetP99L0Ms = benchmark.TargetGetP99L0Ms

	// GetP99L1Ms is the 99th-percentile GET latency ceiling (ms) for
	// the L1 (NVMe disk) cache hit path.
	GetP99L1Ms = benchmark.TargetGetP99L1Ms

	// GetP99OriginMs is the 99th-percentile GET latency ceiling (ms)
	// when the gateway misses both cache tiers and reaches Wasabi.
	GetP99OriginMs = benchmark.TargetGetP99OriginMs

	// SustainedRPS is the floor on the sustained aggregate request
	// rate per gateway node (req/s).
	SustainedRPS = benchmark.TargetSustainedRPS

	// ErrorRateMax is the ceiling on the per-request error rate
	// measured across a sustained-load run.
	ErrorRateMax = benchmark.TargetErrorRateMax

	// RPSEfficiencyMin is the floor on rps_efficiency
	// (attained_rps / offered_rps). Below this the gateway is
	// considered unable to keep up with the offered load even if
	// every latency target passed.
	RPSEfficiencyMin = benchmark.TargetRPSEfficiencyMin

	// CacheHitRatioHotMin is the Phase 3 Hot-tier cache hit ratio
	// target: the fraction of reads served from L0 or L1 must
	// exceed this value.
	CacheHitRatioHotMin = benchmark.TargetCacheHitRatioHotMin

	// WasabiOriginEgressRatioMax is the Phase 2-3 Wasabi origin
	// egress ratio ceiling: monthly egress bytes pulled from Wasabi
	// must be at most this multiple of the tenant's stored bytes.
	WasabiOriginEgressRatioMax = benchmark.TargetWasabiOriginEgressRatioMax
)

// ----------------------------------------------------------------------
// §2. S3 protocol capacity envelope.
//
// These are the limits the S3 API itself imposes on a single
// multipart upload. Repeated here as constants so that gateway code
// that enforces the limit (api/s3compat/multipart_handler.go) can
// import them from one place. The numbers are not negotiable: they
// come from the AWS S3 reference documentation and any client built
// against the S3 SDK assumes them.
//
//	https://docs.aws.amazon.com/AmazonS3/latest/userguide/qfacts.html
//
// ----------------------------------------------------------------------

const (
	// MaxObjectSizeBytes is the largest object the S3 API permits
	// in a single object (multipart or otherwise). 5 TiB.
	MaxObjectSizeBytes int64 = 5 * 1024 * 1024 * 1024 * 1024

	// MaxMultipartParts is the largest part-number value allowed
	// in a multipart upload (S3 specifies the inclusive range
	// [1, 10000]).
	//
	// Declared as int (not int64) on purpose: this is a count, not
	// a byte size. It is used as a slice index / loop bound / map
	// capacity hint in gateway code (api/s3compat/multipart_handler.go),
	// where int is the idiomatic Go type. The byte-size constants
	// in this section are int64 because they index into and compare
	// against ContentLength values, which the S3 SDK types as int64.
	MaxMultipartParts int = 10_000

	// MinMultipartPartSizeBytes is the smallest per-part size for
	// every part EXCEPT the final part of a multipart upload.
	// 5 MiB.
	MinMultipartPartSizeBytes int64 = 5 * 1024 * 1024

	// MaxMultipartPartSizeBytes is the largest per-part size for a
	// multipart upload. 5 GiB.
	MaxMultipartPartSizeBytes int64 = 5 * 1024 * 1024 * 1024
)

// ----------------------------------------------------------------------
// §3. Per-gateway-node capacity envelope.
//
// These describe the sustainable steady-state operating envelope of
// a single gateway node — the unit the operator scales horizontally.
// PerGatewayNodeSustainedRPS is the same number as SustainedRPS but
// re-stated under a name that makes the per-node semantic explicit
// in dossier prose.
// ----------------------------------------------------------------------

const (
	// PerGatewayNodeSustainedRPS is the sustainable steady-state
	// request rate per gateway node. The same gate as SustainedRPS,
	// renamed for dossier clarity. A region's aggregate request
	// capacity is approximately PerGatewayNodeSustainedRPS *
	// (number of gateway nodes provisioned in that cell).
	PerGatewayNodeSustainedRPS = benchmark.TargetSustainedRPS
)

// ----------------------------------------------------------------------
// §4. Per-cell capacity envelope.
//
// A "cell" is the architectural unit defined in docs/PROPOSAL.md §6.
// Per §6.2 ("Cell sizing"):
//
//	"A cell is 2-20 PB usable. Below 2 PB the per-cell overhead
//	 dominates; above 20 PB repair and failure domains get
//	 unwieldy."
//
// The PB-range bounds are restated here as constants so that the
// dossier table is computable from one source. Bytes use 1024-based
// units to match every other size constant in the package; the
// docs/PROPOSAL.md prose uses PB as shorthand and means PiB.
// ----------------------------------------------------------------------

const (
	pibBytes int64 = 1024 * 1024 * 1024 * 1024 * 1024 // 2^50

	// MinCellUsableCapacityBytes is the floor on a cell's usable
	// (post-erasure-coding) byte capacity. Below this the per-cell
	// metadata + repair overhead dominates.
	MinCellUsableCapacityBytes int64 = 2 * pibBytes

	// MaxCellUsableCapacityBytes is the ceiling on a cell's usable
	// (post-erasure-coding) byte capacity. Above this the failure
	// domain and repair-time envelope becomes operationally
	// unworkable for a single cell.
	MaxCellUsableCapacityBytes int64 = 20 * pibBytes
)

// ----------------------------------------------------------------------
// §5. Reliability targets — measurable, not theoretical.
//
// Availability is the only nines-style target we commit to in the
// dossier: it is observable from the gateway's own request-success
// counters during a sustained-load run, and it is the inverse of
// ErrorRateMax. We deliberately do NOT include a durability number
// because (a) docs/PROPOSAL.md forbids publishing theoretical
// durability nines and (b) the only honest durability number is the
// one measured by tests/chaos (WS1.2) and recorded in the audit
// bundle for the specific gateway build under audit.
// ----------------------------------------------------------------------

const (
	// AvailabilityFractionMin is the floor on per-request
	// availability across a sustained-load run, defined as
	// 1 - ErrorRateMax. Stated as a fraction (0.999) rather than a
	// percentage so the value composes cleanly with other ratios
	// in dossier arithmetic. The same target appears as a
	// percentage (99.9%) in the dossier's prose.
	AvailabilityFractionMin = 1.0 - ErrorRateMax
)

// ----------------------------------------------------------------------
// §6. Operational targets — open.
//
// The PROGRESS.md appendix lists three operational metrics whose
// targets are TBD ("repair time on single-node loss", "storage COGS
// per TB-month", "Wasabi -> local cell migration throughput"). These
// will become committed constants once Phase 2 / Phase 3 measurement
// closes them. The dossier explicitly enumerates them as OPEN so an
// auditor reviewing the bundle does not mistake their absence for an
// oversight.
//
// When a number lands here, also remove the corresponding entry
// from the "Open" subsection of docs/CAPACITY.md and link the
// measurement report.
// ----------------------------------------------------------------------

// OpenOperationalTarget is a metric whose target value is not yet
// committed: the dossier explicitly enumerates these so an auditor
// sees the gap as a known, tracked gap and not a missing entry.
type OpenOperationalTarget struct {
	// Name is the dossier-facing identifier of the metric.
	Name string
	// Unit is the unit of the target value once committed.
	Unit string
	// Owner names the phase under which the target is expected to
	// be measured and committed.
	Owner string
}

// OpenOperationalTargets enumerates the operational metrics that
// PROGRESS.md flags as TBD and that the dossier surfaces as known
// gaps rather than missing entries.
func OpenOperationalTargets() []OpenOperationalTarget {
	return []OpenOperationalTarget{
		{
			Name:  "RepairTimeSingleNodeLoss",
			Unit:  "hours",
			Owner: "Phase 2",
		},
		{
			Name:  "StorageCOGSPerTBMonth",
			Unit:  "USD",
			Owner: "Phase 3",
		},
		{
			Name:  "MigrationThroughputWasabiToCell",
			Unit:  "bytes/sec",
			Owner: "Phase 3",
		},
	}
}
