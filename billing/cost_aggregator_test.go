package billing

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

type fakeUsage struct {
	usage StorageUsage
	err   error
	gotID string
	gotMo string
}

func (f *fakeUsage) StorageUsage(_ context.Context, tenantID, month string) (StorageUsage, error) {
	f.gotID = tenantID
	f.gotMo = month
	return f.usage, f.err
}

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.6f, want %.6f", name, got, want)
	}
}

func TestCostAggregator_BreaksDownComponentsAndTotal(t *testing.T) {
	// Fixture: a tenant storing 1000 GiB-month logical, of which
	// dedup avoided 200 GiB-month. Wasabi at $0.0068/GiB-month;
	// $4000/mo Linode and $1500/mo AWS amortized across 8 tenants.
	usage := &fakeUsage{usage: StorageUsage{LogicalGiBMonth: 1000, DedupSavedGiBMonth: 200}}
	a := &DefaultCostAggregator{
		Usage: usage,
		Model: CostModel{
			WasabiUSDPerGiBMonth:      0.0068,
			LinodeMonthlyUSD:          4000,
			AWSControlPlaneMonthlyUSD: 1500,
		},
		ActiveTenants: func() int { return 8 },
	}

	got, err := a.GetCostBreakdown(context.Background(), "t-1", "2026-06")
	if err != nil {
		t.Fatal(err)
	}

	wantWasabi := 1000 * 0.0068
	wantDedup := 200 * 0.0068
	wantLinode := 4000.0 / 8
	wantAWS := 1500.0 / 8
	wantTotal := wantWasabi + wantLinode + wantAWS - wantDedup

	approx(t, "WasabiStorageUSD", got.WasabiStorageUSD, wantWasabi)
	approx(t, "DedupSavingsUSD", got.DedupSavingsUSD, wantDedup)
	approx(t, "LinodeComputeUSD", got.LinodeComputeUSD, wantLinode)
	approx(t, "AWSControlPlaneUSD", got.AWSControlPlaneUSD, wantAWS)
	approx(t, "TotalUSD", got.TotalUSD, wantTotal)

	if got.TenantID != "t-1" || got.Month != "2026-06" {
		t.Errorf("identity = (%q,%q), want (t-1,2026-06)", got.TenantID, got.Month)
	}
	// The net physically-billed Wasabi cost is gross minus dedup.
	approx(t, "net wasabi", got.WasabiStorageUSD-got.DedupSavingsUSD, (1000-200)*0.0068)
	if usage.gotID != "t-1" || usage.gotMo != "2026-06" {
		t.Errorf("reader called with (%q,%q), want (t-1,2026-06)", usage.gotID, usage.gotMo)
	}
}

func TestCostAggregator_AmortizationDefaultsToSingleTenant(t *testing.T) {
	usage := &fakeUsage{usage: StorageUsage{LogicalGiBMonth: 0}}
	model := CostModel{LinodeMonthlyUSD: 4000, AWSControlPlaneMonthlyUSD: 1500}

	// nil resolver -> divisor 1: the whole fixed cost lands on the
	// single tenant rather than dividing by zero.
	a := &DefaultCostAggregator{Usage: usage, Model: model}
	got, err := a.GetCostBreakdown(context.Background(), "t-1", "2026-06")
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "LinodeComputeUSD", got.LinodeComputeUSD, 4000)
	approx(t, "AWSControlPlaneUSD", got.AWSControlPlaneUSD, 1500)
	approx(t, "TotalUSD", got.TotalUSD, 5500)

	// A non-positive count is also floored to 1.
	a.ActiveTenants = func() int { return 0 }
	got, err = a.GetCostBreakdown(context.Background(), "t-1", "2026-06")
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "LinodeComputeUSD (zero tenants)", got.LinodeComputeUSD, 4000)
}

func TestCostAggregator_NoDedupNoSavings(t *testing.T) {
	usage := &fakeUsage{usage: StorageUsage{LogicalGiBMonth: 500, DedupSavedGiBMonth: 0}}
	a := &DefaultCostAggregator{
		Usage: usage,
		Model: CostModel{WasabiUSDPerGiBMonth: 0.01},
	}
	got, err := a.GetCostBreakdown(context.Background(), "t-2", "2026-06")
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "DedupSavingsUSD", got.DedupSavingsUSD, 0)
	approx(t, "WasabiStorageUSD", got.WasabiStorageUSD, 5)
	approx(t, "TotalUSD", got.TotalUSD, 5)
}

func TestCostAggregator_MonthlyCostUsesCurrentMonth(t *testing.T) {
	now := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	usage := &fakeUsage{usage: StorageUsage{LogicalGiBMonth: 100}}
	a := &DefaultCostAggregator{
		Usage: usage,
		Model: CostModel{WasabiUSDPerGiBMonth: 0.0068},
		Clock: func() time.Time { return now },
	}
	got, err := a.MonthlyCost(context.Background(), "t-3")
	if err != nil {
		t.Fatal(err)
	}
	if got.Month != "2026-03" {
		t.Errorf("Month = %q, want 2026-03", got.Month)
	}
	if usage.gotMo != "2026-03" {
		t.Errorf("reader month = %q, want 2026-03", usage.gotMo)
	}
}

func TestCostAggregator_EmptyMonthResolvesToCurrent(t *testing.T) {
	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	usage := &fakeUsage{}
	a := &DefaultCostAggregator{Usage: usage, Clock: func() time.Time { return now }}
	if _, err := a.GetCostBreakdown(context.Background(), "t-4", ""); err != nil {
		t.Fatal(err)
	}
	if usage.gotMo != "2026-12" {
		t.Errorf("reader month = %q, want 2026-12", usage.gotMo)
	}
}

func TestCostAggregator_Errors(t *testing.T) {
	// nil reader
	var nilAgg *DefaultCostAggregator
	if _, err := nilAgg.GetCostBreakdown(context.Background(), "t", "2026-06"); err == nil {
		t.Error("nil aggregator should error")
	}
	// missing Usage
	if _, err := (&DefaultCostAggregator{}).GetCostBreakdown(context.Background(), "t", "2026-06"); err == nil {
		t.Error("aggregator without StorageUsageReader should error")
	}
	// missing tenant ID
	a := &DefaultCostAggregator{Usage: &fakeUsage{}}
	if _, err := a.GetCostBreakdown(context.Background(), "", "2026-06"); err == nil {
		t.Error("empty tenant id should error")
	}
	// reader error propagates
	sentinel := errors.New("boom")
	a = &DefaultCostAggregator{Usage: &fakeUsage{err: sentinel}}
	if _, err := a.GetCostBreakdown(context.Background(), "t", "2026-06"); !errors.Is(err, sentinel) {
		t.Errorf("reader error not propagated, got %v", err)
	}
}

// TestForecastResult_ProjectedMonthlyCost lives here because it
// exercises the same cost-rate plumbing the aggregator uses.
func TestForecastResult_ProjectedMonthlyCost(t *testing.T) {
	const gib = uint64(1) << 30
	// 100 GiB now, growing 1 GiB/day, at $0.0068/GiB-month.
	growthPerSec := float64(gib) / float64(24*60*60)
	res := ForecastResult{
		CurrentBytes:       100 * gib,
		GrowthBytesPerSec:  growthPerSec,
		CostUSDPerGiBMonth: 0.0068,
	}
	proj := res.ProjectedMonthlyCost(3)
	if len(proj) != 3 {
		t.Fatalf("len(proj) = %d, want 3", len(proj))
	}
	// Month 1: 100 GiB + ~30 GiB growth = ~130 GiB.
	wantBytesM1 := float64(100*gib) + growthPerSec*secondsPerProjectedMonth
	approx(t, "month1 cost", proj[0].ProjectedCostUSD, wantBytesM1/bytesPerGiB*0.0068)
	if proj[0].MonthsFromNow != 1 || proj[2].MonthsFromNow != 3 {
		t.Errorf("MonthsFromNow not 1-based: %+v", proj)
	}
	// Cost must increase month over month for a growing cell.
	if !(proj[0].ProjectedCostUSD < proj[1].ProjectedCostUSD && proj[1].ProjectedCostUSD < proj[2].ProjectedCostUSD) {
		t.Errorf("cost should grow monthly, got %v", proj)
	}
}

func TestForecastResult_ProjectedMonthlyCost_Disabled(t *testing.T) {
	// Zero rate => disabled.
	res := ForecastResult{CurrentBytes: 100 << 30}
	if got := res.ProjectedMonthlyCost(3); got != nil {
		t.Errorf("zero rate should disable projection, got %v", got)
	}
	// Non-positive months => nil.
	res.CostUSDPerGiBMonth = 0.0068
	if got := res.ProjectedMonthlyCost(0); got != nil {
		t.Errorf("zero months should return nil, got %v", got)
	}
}

func TestForecastResult_ProjectedMonthlyCost_ShrinkingFloorsAtZero(t *testing.T) {
	const gib = uint64(1) << 30
	// Tiny base, steep negative slope: projection must floor at 0,
	// never go negative.
	res := ForecastResult{
		CurrentBytes:       1 * gib,
		GrowthBytesPerSec:  -float64(gib) / float64(24*60*60), // -1 GiB/day
		CostUSDPerGiBMonth: 0.0068,
	}
	proj := res.ProjectedMonthlyCost(2)
	for _, p := range proj {
		if p.ProjectedCostUSD < 0 || p.ProjectedEffectiveBytes != 0 {
			t.Errorf("shrinking projection must floor at zero, got %+v", p)
		}
	}
}
