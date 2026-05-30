package capacity

import (
	"testing"

	"github.com/kennguy3n/zk-object-fabric/tests/benchmark"
)

// These tests pin every dossier constant to its expected numeric
// value. The point is NOT to duplicate the const declarations (Go's
// type system already does that); it is to make any value change
// land in this file as part of the same PR. A reviewer who sees the
// dossier-test diff has the numeric change in their face and can
// decide whether the change is intended.
//
// Drift-detection responsibilities split as follows:
//
//   - Rename safety for benchmark.Target* upstream constants is
//     provided at COMPILE TIME by the alias declarations in
//     targets.go (`PutP99CacheHitMs = benchmark.TargetPutP99CacheHitMs`).
//     A rename of the upstream constant breaks the build of targets.go
//     directly — no runtime test required, and a runtime equality check
//     (`capacity.X == benchmark.X`) would be tautological because the
//     alias makes both sides the same memory location.
//
//   - Value-drift safety is provided by TestPerformanceTargets_DocumentedValues
//     below: it pins each constant to a literal (50.0, 200.0, ...) so any
//     upstream value change in tests/benchmark/suite.go propagates
//     through the alias and fails this test, forcing a coordinated
//     update of docs/CAPACITY.md, the literal in this test, and the
//     upstream constant.
//
//   - Structural-drift safety is provided by tests/capacity/dossier_test.go
//     (constant-name ↔ doc grep, both directions).
//
// TestPerformanceTargets_DocumentedSuiteValues below is a defence-in-depth
// belt-and-suspenders check: it asserts the benchmark.Target* values
// themselves match the dossier-documented literals, so that even if
// someone later replaces the alias in targets.go with an inline literal
// (severing the compile-time link), the upstream value still gets
// pinned. This catches the narrow but real "someone refactored away
// the alias" failure mode that the alias itself cannot detect.

func TestPerformanceTargets_DocumentedSuiteValues(t *testing.T) {
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"benchmark.TargetPutP99CacheHitMs", benchmark.TargetPutP99CacheHitMs, 50.0},
		{"benchmark.TargetPutP99OriginMs", benchmark.TargetPutP99OriginMs, 200.0},
		{"benchmark.TargetGetP99L0Ms", benchmark.TargetGetP99L0Ms, 20.0},
		{"benchmark.TargetGetP99L1Ms", benchmark.TargetGetP99L1Ms, 100.0},
		{"benchmark.TargetGetP99OriginMs", benchmark.TargetGetP99OriginMs, 300.0},
		{"benchmark.TargetSustainedRPS", benchmark.TargetSustainedRPS, 10_000.0},
		{"benchmark.TargetErrorRateMax", benchmark.TargetErrorRateMax, 1e-3},
		{"benchmark.TargetRPSEfficiencyMin", benchmark.TargetRPSEfficiencyMin, 0.95},
		{"benchmark.TargetCacheHitRatioHotMin", benchmark.TargetCacheHitRatioHotMin, 0.9},
		{"benchmark.TargetWasabiOriginEgressRatioMax", benchmark.TargetWasabiOriginEgressRatioMax, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("upstream %s = %v, want %v — the benchmark suite value changed and the dossier was not updated to match; either revert the upstream change, or update both the documented literal here and docs/CAPACITY.md", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestPerformanceTargets_DocumentedValues(t *testing.T) {
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"PutP99CacheHitMs", PutP99CacheHitMs, 50.0},
		{"PutP99OriginMs", PutP99OriginMs, 200.0},
		{"GetP99L0Ms", GetP99L0Ms, 20.0},
		{"GetP99L1Ms", GetP99L1Ms, 100.0},
		{"GetP99OriginMs", GetP99OriginMs, 300.0},
		{"SustainedRPS", SustainedRPS, 10_000.0},
		{"PerGatewayNodeSustainedRPS", PerGatewayNodeSustainedRPS, 10_000.0},
		{"ErrorRateMax", ErrorRateMax, 1e-3},
		{"RPSEfficiencyMin", RPSEfficiencyMin, 0.95},
		{"CacheHitRatioHotMin", CacheHitRatioHotMin, 0.9},
		{"WasabiOriginEgressRatioMax", WasabiOriginEgressRatioMax, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("dossier %s = %v, want %v — update docs/CAPACITY.md", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestS3ProtocolLimits_StableValues(t *testing.T) {
	cases := []struct {
		name string
		got  int64
		want int64
	}{
		{"MaxObjectSizeBytes (5 TiB)", MaxObjectSizeBytes, 5 * 1024 * 1024 * 1024 * 1024},
		{"MinMultipartPartSizeBytes (5 MiB)", MinMultipartPartSizeBytes, 5 * 1024 * 1024},
		{"MaxMultipartPartSizeBytes (5 GiB)", MaxMultipartPartSizeBytes, 5 * 1024 * 1024 * 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("S3 protocol limit %s drifted: got=%d, want=%d — this number comes from the AWS S3 reference doc and is not negotiable", tc.name, tc.got, tc.want)
			}
		})
	}
	if MaxMultipartParts != 10_000 {
		t.Fatalf("MaxMultipartParts = %d, want 10000 — see AWS S3 multipart upload limits", MaxMultipartParts)
	}
}

func TestS3ProtocolLimits_PartSizeRangeIsValid(t *testing.T) {
	if MinMultipartPartSizeBytes >= MaxMultipartPartSizeBytes {
		t.Fatalf("min part size (%d) must be < max part size (%d)", MinMultipartPartSizeBytes, MaxMultipartPartSizeBytes)
	}
	// 10000 max parts * 5 GiB max part = 50 TiB theoretical upper
	// bound, but S3 caps the object at 5 TiB. Confirm the per-part
	// envelope is at least consistent with the object envelope.
	upper := int64(MaxMultipartParts) * MaxMultipartPartSizeBytes
	if upper < MaxObjectSizeBytes {
		t.Fatalf("multipart envelope (%d B = parts %d * %d) smaller than MaxObjectSizeBytes (%d)", upper, MaxMultipartParts, MaxMultipartPartSizeBytes, MaxObjectSizeBytes)
	}
}

func TestCellSizing_PinnedToProposalDoc(t *testing.T) {
	const pib int64 = 1024 * 1024 * 1024 * 1024 * 1024
	if MinCellUsableCapacityBytes != 2*pib {
		t.Fatalf("MinCellUsableCapacityBytes = %d, want 2 PiB (%d) — see docs/PROPOSAL.md §6.2", MinCellUsableCapacityBytes, 2*pib)
	}
	if MaxCellUsableCapacityBytes != 20*pib {
		t.Fatalf("MaxCellUsableCapacityBytes = %d, want 20 PiB (%d) — see docs/PROPOSAL.md §6.2", MaxCellUsableCapacityBytes, 20*pib)
	}
	if MinCellUsableCapacityBytes >= MaxCellUsableCapacityBytes {
		t.Fatalf("min cell capacity (%d) must be < max cell capacity (%d)", MinCellUsableCapacityBytes, MaxCellUsableCapacityBytes)
	}
}

func TestAvailability_DerivedFromErrorRate(t *testing.T) {
	// We do NOT assert `AvailabilityFractionMin == 1.0 - ErrorRateMax`:
	// targets.go declares the constant as exactly that expression, so
	// the equality is tautological at compile time. Per the round-2
	// cleanup of TestPerformanceTargets_PinnedToBenchmarkSuite, this
	// package does not ship tautological runtime checks.
	//
	// What this test actually verifies is the prose-value round-trip:
	// the constant — derivation aside — must materialize to exactly
	// the "99.9%" figure that docs/CAPACITY.md commits to in prose.
	// If anyone ever tightens ErrorRateMax (e.g. to 5e-4 for a 99.95%
	// commitment), this assertion will fail until either the prose in
	// the dossier is updated to the new percentage, or the new value
	// is wired through here. Catches "constant moved, prose didn't."
	const documentedPct = 99.9
	gotPct := AvailabilityFractionMin * 100.0
	if !approxEqual(gotPct, documentedPct, 1e-6) {
		t.Fatalf("AvailabilityFractionMin*100 = %v, want %v (docs/CAPACITY.md §7 states %v%%) — either revert the ErrorRateMax change, or update the dossier prose and the documentedPct literal here together", gotPct, documentedPct, documentedPct)
	}
}

func TestOpenOperationalTargets_EnumeratesAllKnownGaps(t *testing.T) {
	got := OpenOperationalTargets()
	wantNames := []string{
		"RepairTimeSingleNodeLoss",
		"StorageCOGSPerTBMonth",
		"MigrationThroughputWasabiToCell",
	}
	if len(got) != len(wantNames) {
		t.Fatalf("got %d open targets, want %d", len(got), len(wantNames))
	}
	for i, name := range wantNames {
		if got[i].Name != name {
			t.Errorf("open target [%d].Name = %q, want %q", i, got[i].Name, name)
		}
		if got[i].Unit == "" {
			t.Errorf("open target %q has empty Unit", got[i].Name)
		}
		if got[i].Owner == "" {
			t.Errorf("open target %q has empty Owner", got[i].Name)
		}
	}
}

func approxEqual(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
