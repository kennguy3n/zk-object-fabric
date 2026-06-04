// Cost aggregation collapses the gateway's three underlying cloud
// bills — Wasabi object storage, Linode compute, and the AWS
// control plane — into a single per-tenant monthly cost estimate.
//
// SME operators run the gateway to escape exactly this
// "three-provider billing complexity": they want one number per
// tenant, not three separate cloud invoices to reconcile. The
// CostAggregator produces that number, decomposed into a
// CostBreakdown so the console can still show where the money goes
// and how much intra-tenant dedup is saving.
//
// The storage component is metered (it scales with what the tenant
// stores); the Linode and AWS components are fixed-infrastructure
// costs the operator supplies via CostModel and the aggregator
// amortizes evenly across the active-tenant population. All math
// runs in this package; no external dependencies are added.
package billing

import (
	"context"
	"errors"
	"time"
)

// monthLayout is the year-month format used throughout the cost
// surface (CostBreakdown.Month, the StorageUsageReader month key,
// and the console handler's ?month= query parameter).
const monthLayout = "2006-01"

// CostBreakdown is the unified monthly cost estimate for a single
// tenant, decomposed into the per-provider components that make up
// the bill.
//
// TotalUSD is the net monthly cost:
//
//	TotalUSD = WasabiStorageUSD + LinodeComputeUSD +
//	           AWSControlPlaneUSD - DedupSavingsUSD
//
// WasabiStorageUSD is priced on the tenant's *logical* (pre-dedup)
// stored volume — i.e. what the tenant would pay Wasabi with dedup
// disabled — and DedupSavingsUSD is the slice of that the gateway
// avoided paying because intra-tenant dedup recognized convergent
// ciphertext. The physically-billed Wasabi cost is therefore
// WasabiStorageUSD - DedupSavingsUSD; surfacing both lets the
// console show the gross storage list price alongside the concrete
// dollar value dedup returns to the tenant.
type CostBreakdown struct {
	TenantID           string  `json:"tenant_id"`
	Month              string  `json:"month"`
	WasabiStorageUSD   float64 `json:"wasabi_storage_usd"`
	LinodeComputeUSD   float64 `json:"linode_compute_usd"`
	AWSControlPlaneUSD float64 `json:"aws_control_plane_usd"`
	DedupSavingsUSD    float64 `json:"dedup_savings_usd"`
	TotalUSD           float64 `json:"total_usd"`
}

// StorageUsage is the metered storage volume for one tenant over a
// billing month, expressed in GiB-month. It is the per-tenant
// rollup of the metering pipeline's StorageBytesSeconds and
// DedupBytesSaved dimensions (see metering.go).
type StorageUsage struct {
	// LogicalGiBMonth is the GiB-month the tenant's objects
	// represent before intra-tenant dedup — the volume the tenant
	// would store (and pay Wasabi for) if dedup were disabled.
	LogicalGiBMonth float64

	// DedupSavedGiBMonth is the GiB-month NOT written to Wasabi
	// because the gateway recognized convergent ciphertext. The
	// physically-stored volume is LogicalGiBMonth -
	// DedupSavedGiBMonth; this value must not exceed
	// LogicalGiBMonth.
	DedupSavedGiBMonth float64
}

// StorageUsageReader resolves per-tenant metered storage for a
// billing month. The production implementation aggregates the
// ClickHouse usage_counters table (StorageBytesSeconds /
// DedupBytesSaved); tests pass an in-memory fake. month is a
// year-month key ("2006-01").
//
// Implementations must be safe for concurrent use.
type StorageUsageReader interface {
	StorageUsage(ctx context.Context, tenantID, month string) (StorageUsage, error)
}

// CostModel carries the operator-supplied cost inputs the
// CostAggregator needs that are NOT derived from the metering
// pipeline: the Wasabi storage price and the fixed Linode / AWS
// spend that is amortized across tenants.
//
// These live here as a billing-local config struct rather than in
// internal/config.BillingConfig so the cost model stays colocated
// with the aggregator that consumes it; the gateway wiring maps its
// configuration onto this struct. Values are dollars (USD).
type CostModel struct {
	// WasabiUSDPerGiBMonth is the Wasabi storage price per
	// GiB-month. Used to price both the storage component and the
	// dedup savings.
	WasabiUSDPerGiBMonth float64

	// LinodeMonthlyUSD is the total monthly Linode compute spend
	// for the gateway fleet, amortized evenly across the active
	// tenants.
	LinodeMonthlyUSD float64

	// AWSControlPlaneMonthlyUSD is the total monthly AWS
	// control-plane spend (Route 53, KMS, SES, …), amortized evenly
	// across the active tenants.
	AWSControlPlaneMonthlyUSD float64
}

// CostAggregator computes a single monthly cost estimate per
// tenant. MonthlyCost reports the breakdown for the current month;
// implementations also satisfy CostReporter for an explicit month.
type CostAggregator interface {
	// MonthlyCost returns the unified cost breakdown for tenantID
	// for the current calendar month.
	MonthlyCost(ctx context.Context, tenantID string) (CostBreakdown, error)
}

// DefaultCostAggregator is the metering-backed CostAggregator.
type DefaultCostAggregator struct {
	// Usage is the metered storage source. Required.
	Usage StorageUsageReader

	// Model carries the storage price and amortized fixed costs.
	Model CostModel

	// ActiveTenants resolves the divisor for the amortized
	// (Linode / AWS) components — typically a live count from the
	// tenant store. A nil resolver, or one returning a value <= 0,
	// is treated as 1 so a single-tenant / bootstrap deployment
	// does not divide by zero (the whole fixed cost lands on the
	// one tenant rather than blowing up).
	ActiveTenants func() int

	// Clock defaults to time.Now. Used to resolve the current month
	// for MonthlyCost.
	Clock func() time.Time
}

// MonthlyCost implements CostAggregator.
func (a *DefaultCostAggregator) MonthlyCost(ctx context.Context, tenantID string) (CostBreakdown, error) {
	return a.GetCostBreakdown(ctx, tenantID, a.now().Format(monthLayout))
}

// GetCostBreakdown implements CostReporter. An empty month resolves
// to the current month.
func (a *DefaultCostAggregator) GetCostBreakdown(ctx context.Context, tenantID, month string) (CostBreakdown, error) {
	if a == nil || a.Usage == nil {
		return CostBreakdown{}, errors.New("billing: cost aggregator has no StorageUsageReader")
	}
	if tenantID == "" {
		return CostBreakdown{}, errors.New("billing: cost aggregator: tenant_id is required")
	}
	if month == "" {
		month = a.now().Format(monthLayout)
	}

	usage, err := a.Usage.StorageUsage(ctx, tenantID, month)
	if err != nil {
		return CostBreakdown{}, err
	}

	rate := a.Model.WasabiUSDPerGiBMonth
	wasabi := usage.LogicalGiBMonth * rate
	dedup := usage.DedupSavedGiBMonth * rate

	n := float64(a.activeTenants())
	linode := a.Model.LinodeMonthlyUSD / n
	aws := a.Model.AWSControlPlaneMonthlyUSD / n

	return CostBreakdown{
		TenantID:           tenantID,
		Month:              month,
		WasabiStorageUSD:   wasabi,
		LinodeComputeUSD:   linode,
		AWSControlPlaneUSD: aws,
		DedupSavingsUSD:    dedup,
		TotalUSD:           wasabi + linode + aws - dedup,
	}, nil
}

// activeTenants returns the amortization divisor, flooring at 1.
func (a *DefaultCostAggregator) activeTenants() int {
	if a.ActiveTenants != nil {
		if n := a.ActiveTenants(); n > 0 {
			return n
		}
	}
	return 1
}

func (a *DefaultCostAggregator) now() time.Time {
	if a.Clock != nil {
		return a.Clock()
	}
	return time.Now()
}

// static interface checks
var (
	_ CostAggregator = (*DefaultCostAggregator)(nil)
	_ CostReporter   = (*DefaultCostAggregator)(nil)
)
