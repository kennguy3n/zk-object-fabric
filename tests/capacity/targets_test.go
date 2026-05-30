package capacity

import (
	"testing"

	"github.com/kennguy3n/zk-object-fabric/tests/benchmark"
)

// These tests are deliberately literal: they pin every dossier
// constant to its expected numeric value. The point is NOT to
// duplicate the const declarations (Go's type system already does
// that); it is to make any value change land in this file as part of
// the same PR. A reviewer who sees the dossier-test diff has the
// numeric change in their face and can decide whether the change is
// intended.
//
// Equally important, the re-export aliases at the top of targets.go
// are pinned against benchmark.Target* directly so that a rename of
// the upstream constant in tests/benchmark/suite.go would break this
// test instead of silently producing a stale dossier value.

func TestPerformanceTargets_PinnedToBenchmarkSuite(t *testing.T) {
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"PutP99CacheHitMs", PutP99CacheHitMs, benchmark.TargetPutP99CacheHitMs},
		{"PutP99OriginMs", PutP99OriginMs, benchmark.TargetPutP99OriginMs},
		{"GetP99L0Ms", GetP99L0Ms, benchmark.TargetGetP99L0Ms},
		{"GetP99L1Ms", GetP99L1Ms, benchmark.TargetGetP99L1Ms},
		{"GetP99OriginMs", GetP99OriginMs, benchmark.TargetGetP99OriginMs},
		{"SustainedRPS", SustainedRPS, benchmark.TargetSustainedRPS},
		{"ErrorRateMax", ErrorRateMax, benchmark.TargetErrorRateMax},
		{"RPSEfficiencyMin", RPSEfficiencyMin, benchmark.TargetRPSEfficiencyMin},
		{"CacheHitRatioHotMin", CacheHitRatioHotMin, benchmark.TargetCacheHitRatioHotMin},
		{"WasabiOriginEgressRatioMax", WasabiOriginEgressRatioMax, benchmark.TargetWasabiOriginEgressRatioMax},
		{"PerGatewayNodeSustainedRPS", PerGatewayNodeSustainedRPS, benchmark.TargetSustainedRPS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("dossier %s drifted from benchmark suite: dossier=%v, suite=%v — update docs/CAPACITY.md alongside the value change", tc.name, tc.got, tc.want)
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
	want := 1.0 - ErrorRateMax
	if AvailabilityFractionMin != want {
		t.Fatalf("AvailabilityFractionMin = %v, want 1 - ErrorRateMax = %v", AvailabilityFractionMin, want)
	}
	// Round-trip sanity: the documented "99.9%" prose value must
	// match the constant to 6 decimal places (i.e. the rounding the
	// dossier uses).
	const documentedPct = 99.9
	gotPct := AvailabilityFractionMin * 100.0
	if !approxEqual(gotPct, documentedPct, 1e-6) {
		t.Fatalf("AvailabilityFractionMin*100 = %v, want %v (docs/CAPACITY.md states 99.9%%)", gotPct, documentedPct)
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
