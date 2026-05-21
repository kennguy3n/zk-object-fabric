package placement_policy

import (
	"context"
	"io"
	"math"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// writeHeavyProviderMap is a deliberately constructed registry where
// the pre-PR-9 rank ordering puts a *more expensive* per-GB-stored
// provider first because the legacy weight × 1000 over-amortises
// egress against a tenant that almost never reads. Each test below
// uses this map and a write-heavy WorkloadProfile to prove that
// storageRank now picks the storage-cheaper provider.
func writeHeavyProviderMap() map[string]providers.StorageProvider {
	return map[string]providers.StorageProvider{
		// "cold_archive" is the right answer for write-heavy: low
		// storage cost dominates because almost nothing is read.
		"cold_archive": &writeHeavyProvider{
			name: "cold_archive",
			cost: providers.ProviderCostModel{
				StorageUSDPerTBMonth: 1.0,
				EgressUSDPerGB:       0.05,
			},
			labels: providers.PlacementLabels{Provider: "cold_archive"},
		},
		// "hot_cdn" is cheaper for read-heavy (zero egress) but
		// 20× more expensive to store. With the legacy weight
		// 1000, hot_cdn looks identical to cold_archive in rank
		// (20 + 0×1000 = 20 vs 1 + 0.05×1000 = 51, hot_cdn wins).
		// With a write-heavy hint, cold_archive wins.
		"hot_cdn": &writeHeavyProvider{
			name: "hot_cdn",
			cost: providers.ProviderCostModel{
				StorageUSDPerTBMonth: 20.0,
				EgressUSDPerGB:       0.0,
			},
			labels: providers.PlacementLabels{Provider: "hot_cdn"},
		},
	}
}

type writeHeavyProvider struct {
	name   string
	cost   providers.ProviderCostModel
	labels providers.PlacementLabels
}

func (f *writeHeavyProvider) PutPiece(context.Context, string, io.Reader, providers.PutOptions) (providers.PutResult, error) {
	return providers.PutResult{}, nil
}
func (f *writeHeavyProvider) GetPiece(context.Context, string, *providers.ByteRange) (io.ReadCloser, error) {
	return nil, nil
}
func (f *writeHeavyProvider) HeadPiece(context.Context, string) (providers.PieceMetadata, error) {
	return providers.PieceMetadata{}, nil
}
func (f *writeHeavyProvider) DeletePiece(context.Context, string) error { return nil }
func (f *writeHeavyProvider) ListPieces(context.Context, string, string) (providers.ListResult, error) {
	return providers.ListResult{}, nil
}
func (f *writeHeavyProvider) Capabilities() providers.ProviderCapabilities {
	return providers.ProviderCapabilities{SupportsRangeReads: true}
}
func (f *writeHeavyProvider) CostModel() providers.ProviderCostModel        { return f.cost }
func (f *writeHeavyProvider) PlacementLabels() providers.PlacementLabels    { return f.labels }

// TestStorageRank_LegacyEgressWeight pins the pre-PR-9 behaviour for
// the zero-hint case so an upgrade does not silently reshuffle the
// backend ordering for tenants that have not declared a
// WorkloadProfile yet.
func TestStorageRank_LegacyEgressWeight(t *testing.T) {
	p := &writeHeavyProvider{
		cost: providers.ProviderCostModel{
			StorageUSDPerTBMonth: 6.99,
			EgressUSDPerGB:       0.09,
		},
	}
	got := storageRank(p, WorkloadHint{})
	want := 6.99 + 0.09*legacyEgressWeight
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("storageRank zero-hint = %v, want %v (legacy 1000× weight)", got, want)
	}
}

// TestStorageRank_WriteHeavyDownweightsEgress confirms that a
// write-heavy WorkloadHint (RW < 1) collapses the egress term so
// the rank approaches the bare storage price.
func TestStorageRank_WriteHeavyDownweightsEgress(t *testing.T) {
	p := &writeHeavyProvider{
		cost: providers.ProviderCostModel{
			StorageUSDPerTBMonth: 6.99,
			EgressUSDPerGB:       0.09,
		},
	}
	hint := WorkloadHint{ReadWriteRatio: 0.01, AvgObjectSizeMB: 10}
	got := storageRank(p, hint)
	// egressWeight = 0.01 * 10 / 1024 ≈ 0.0000977
	wantEgress := 0.09 * (0.01 * 10.0 / 1024.0)
	want := 6.99 + wantEgress
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("storageRank write-heavy = %v, want %v", got, want)
	}
	// Sanity: the write-heavy rank must be far below the legacy
	// rank so the rest of the algorithm gets to see the
	// difference.
	if legacy := storageRank(p, WorkloadHint{}); legacy <= got*10 {
		t.Fatalf("write-heavy rank %v did not collapse vs legacy %v", got, legacy)
	}
}

// TestStorageRank_ReadHeavyAmpEgress confirms that a heavy
// reads-per-write hint inflates the egress term so providers with
// zero egress beat providers with expensive egress even when the
// storage-only column is more expensive. The cheap-storage
// provider charges $0.09/GB egress and the expensive-storage
// provider has zero egress; with a 10000:1 read:write ratio on
// 1024 MiB objects the egress amortisation must dominate.
func TestStorageRank_ReadHeavyAmpEgress(t *testing.T) {
	cheapStorageExpensiveEgress := &writeHeavyProvider{
		cost: providers.ProviderCostModel{StorageUSDPerTBMonth: 1.0, EgressUSDPerGB: 0.09},
	}
	expensiveStorageZeroEgress := &writeHeavyProvider{
		cost: providers.ProviderCostModel{StorageUSDPerTBMonth: 20.0, EgressUSDPerGB: 0.0},
	}
	hint := WorkloadHint{ReadWriteRatio: 10000, AvgObjectSizeMB: 1024}
	// egressWeight = 10000 * 1024 / 1024 = 10000
	// cheap: 1 + 0.09 * 10000 = 901
	// expensive: 20 + 0 = 20
	a := storageRank(cheapStorageExpensiveEgress, hint)
	b := storageRank(expensiveStorageZeroEgress, hint)
	if !(b < a) {
		t.Fatalf("expected expensive-storage-zero-egress (%v) to rank below cheap-storage-expensive-egress (%v) under read-heavy hint", b, a)
	}
}

// TestEngine_WriteHeavyProfile_PicksColdArchive is the end-to-end
// promise: a tenant declaring a write-heavy WorkloadProfile gets
// placed on the storage-cheap backend, even though the legacy
// formula would have ranked the egress-free hot tier first.
func TestEngine_WriteHeavyProfile_PicksColdArchive(t *testing.T) {
	e := NewEngine("cold_archive", writeHeavyProviderMap(), map[string]*Policy{
		"audit-log-ingest": {
			Tenant: "audit-log-ingest",
			Spec: PolicySpec{
				Encryption: EncryptionSpec{Mode: "client_side"},
				Placement: PlacementSpec{
					Provider: []string{"cold_archive", "hot_cdn"},
					WorkloadProfile: &WorkloadProfile{
						ReadWriteRatio:  0.01,
						AvgObjectSizeMB: 10,
					},
				},
			},
		},
	})
	got, _, err := e.ResolveBackend("audit-log-ingest", "b", "k")
	if err != nil {
		t.Fatalf("ResolveBackend: %v", err)
	}
	if got != "cold_archive" {
		t.Fatalf("write-heavy tenant placed on %q, want cold_archive", got)
	}
}

// TestEngine_ReadHeavyProfile_PicksHotCDN exercises the other
// direction: a CDN-fronted tenant declaring heavy reads-per-write
// gets placed on the zero-egress backend even though it costs 20×
// more per TB stored. The hint must be strong enough that the
// egress amortisation (cold_archive's $0.05/GB × egressWeight)
// exceeds hot_cdn's $19/TB-month storage premium — at 10000:1
// reads-per-write on 1 GiB objects, egressWeight is 10000 and
// cold_archive's rank is 1 + 0.05*10000 = 501, dwarfing hot_cdn's
// 20.
func TestEngine_ReadHeavyProfile_PicksHotCDN(t *testing.T) {
	e := NewEngine("cold_archive", writeHeavyProviderMap(), map[string]*Policy{
		"cdn-frontend": {
			Tenant: "cdn-frontend",
			Spec: PolicySpec{
				Encryption: EncryptionSpec{Mode: "client_side"},
				Placement: PlacementSpec{
					Provider: []string{"cold_archive", "hot_cdn"},
					WorkloadProfile: &WorkloadProfile{
						ReadWriteRatio:  10000,
						AvgObjectSizeMB: 1024,
					},
				},
			},
		},
	})
	got, _, err := e.ResolveBackend("cdn-frontend", "b", "k")
	if err != nil {
		t.Fatalf("ResolveBackend: %v", err)
	}
	if got != "hot_cdn" {
		t.Fatalf("read-heavy tenant placed on %q, want hot_cdn", got)
	}
}

// TestEngine_NoProfile_PreservesLegacyOrdering protects the
// non-breaking-upgrade promise: a policy without a WorkloadProfile
// must produce the same ranking as the pre-PR-9 storageRank.
func TestEngine_NoProfile_PreservesLegacyOrdering(t *testing.T) {
	e := NewEngine("cold_archive", writeHeavyProviderMap(), map[string]*Policy{
		"legacy-tenant": {
			Tenant: "legacy-tenant",
			Spec: PolicySpec{
				Encryption: EncryptionSpec{Mode: "client_side"},
				Placement:  PlacementSpec{Provider: []string{"cold_archive", "hot_cdn"}},
			},
		},
	})
	got, _, err := e.ResolveBackend("legacy-tenant", "b", "k")
	if err != nil {
		t.Fatalf("ResolveBackend: %v", err)
	}
	// Legacy weight: cold_archive rank = 1 + 0.05*1000 = 51,
	// hot_cdn rank = 20 + 0*1000 = 20 — hot_cdn wins.
	if got != "hot_cdn" {
		t.Fatalf("legacy tenant placed on %q, want hot_cdn (pre-PR-9 ordering)", got)
	}
}

// TestWorkloadHint_EgressWeight_NegativeValuesFallBack guards
// against operator typos: a negative ratio or size must not
// produce a negative cost rank (which would push that provider to
// the top of the list). The fallback drops to the legacy weight.
func TestWorkloadHint_EgressWeight_NegativeValuesFallBack(t *testing.T) {
	cases := []WorkloadHint{
		{ReadWriteRatio: -1, AvgObjectSizeMB: 10},
		{ReadWriteRatio: 1, AvgObjectSizeMB: -10},
		{ReadWriteRatio: 0, AvgObjectSizeMB: 10},
		{ReadWriteRatio: 1, AvgObjectSizeMB: 0},
	}
	for i, c := range cases {
		if got := c.egressWeight(); got != legacyEgressWeight {
			t.Errorf("case %d hint=%+v egressWeight=%v, want %v (legacy fallback)", i, c, got, legacyEgressWeight)
		}
	}
}

// TestWorkloadHintFromPolicy_NilSafety pins the projection
// function's behaviour on edge cases the engine actually hits:
// nil policy, nil profile, zero-valued profile.
func TestWorkloadHintFromPolicy_NilSafety(t *testing.T) {
	if got := workloadHintFromPolicy(nil); got != (WorkloadHint{}) {
		t.Errorf("nil policy returned %+v, want zero hint", got)
	}
	if got := workloadHintFromPolicy(&Policy{}); got != (WorkloadHint{}) {
		t.Errorf("empty policy returned %+v, want zero hint", got)
	}
	if got := workloadHintFromPolicy(&Policy{
		Spec: PolicySpec{Placement: PlacementSpec{WorkloadProfile: &WorkloadProfile{ReadWriteRatio: 5, AvgObjectSizeMB: 50}}},
	}); got != (WorkloadHint{ReadWriteRatio: 5, AvgObjectSizeMB: 50}) {
		t.Errorf("policy with profile returned %+v, want {5 50}", got)
	}
}
