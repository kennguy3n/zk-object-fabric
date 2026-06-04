// Package wasabi — guardrails.
//
// This file defines the fair-use guardrail types the Phase 2 data
// plane uses to keep Wasabi usage inside its advertised envelope.
// The guardrails are declarative: the types describe the constraints
// and thresholds; the enforcement loop lives in the control plane and
// the gateway's cache/billing modules.
//
// The fair-use constraint that drives every rule below is:
//
//	per-tenant monthly Wasabi origin egress <= per-tenant active
//	storage volume on Wasabi.
//
// See docs/PROPOSAL.md §2.2, §3.11, and docs/PROGRESS.md line 55 for
// the product-level context.
package wasabi

import (
	"fmt"
	"math"
	"time"
)

// WasabiMinStorageDays is Wasabi's 90-day minimum storage duration.
// Objects deleted before this window still incur 90 days of billable
// storage. The data plane must not write short-TTL objects to Wasabi;
// those belong in the Linode cache (see cache/hot_object_cache).
const WasabiMinStorageDays = 90

// WasabiStorageUSDPerTBMonth is Wasabi's headline storage price per
// TB-month (docs/PROPOSAL.md §2.1). It is the single source of truth
// for the storage rate: CostModel() in wasabi.go reports it to the
// placement engine, and MinStorageWarning uses it to estimate the
// residual charge of an early delete. A "TB" here is 1e12 bytes and a
// "month" is 30 days, matching how Wasabi bills.
const WasabiStorageUSDPerTBMonth = 6.99

// minStorageDuration is WasabiMinStorageDays expressed as a duration.
const minStorageDuration = WasabiMinStorageDays * 24 * time.Hour

// FairUseEgressBudget describes the monthly Wasabi origin egress
// allowance for a single tenant.
//
// Wasabi's fair-use policy is interpreted here as a budget that
// replenishes at the start of each billing window, sized to the
// tenant's active stored bytes multiplied by EgressStorageRatio.
type FairUseEgressBudget struct {
	// TenantID identifies the tenant this budget applies to.
	TenantID string `json:"tenant_id"`

	// WindowStart is the inclusive start of the budget window,
	// typically the first instant of a calendar month UTC.
	WindowStart time.Time `json:"window_start"`

	// WindowDuration is the length of the budget window.
	WindowDuration time.Duration `json:"window_duration"`

	// EgressStorageRatio is the multiplier applied to the tenant's
	// average active stored bytes on Wasabi over the window. The
	// Wasabi fair-use policy treats 1.0 as the headline number; the
	// data plane may enforce a tighter ratio to leave headroom for
	// noisy-neighbour effects.
	EgressStorageRatio float64 `json:"egress_storage_ratio"`

	// SoftCapBytes is the first alert threshold. Crossing it emits a
	// warning but does not throttle traffic.
	SoftCapBytes uint64 `json:"soft_cap_bytes"`

	// HardCapBytes is the enforcement threshold. Crossing it causes
	// the gateway to throttle or reject Wasabi origin reads per the
	// ThrottlePolicy field on AlertThresholds.
	HardCapBytes uint64 `json:"hard_cap_bytes"`
}

// MinStorageTracker tracks per-piece age on Wasabi so the billing
// pipeline can charge the full 90-day minimum storage duration even
// when a piece is deleted early.
type MinStorageTracker struct {
	// TenantID identifies the tenant owning the piece.
	TenantID string `json:"tenant_id"`

	// PieceID is the opaque piece identifier.
	PieceID string `json:"piece_id"`

	// StoredAt is when the piece was written to Wasabi.
	StoredAt time.Time `json:"stored_at"`

	// DeletedAt, if non-zero, is when the piece was deleted. A
	// deletion before StoredAt + MinStorageDuration generates a
	// residual billable interval of (MinStorageDuration - age).
	DeletedAt time.Time `json:"deleted_at,omitempty"`

	// MinStorageDuration is the provider-advertised minimum storage
	// duration. For Wasabi this is 90 days; it is stored on the
	// tracker so the same type can be reused for other providers
	// with different minimums (e.g. Glacier).
	MinStorageDuration time.Duration `json:"min_storage_duration"`
}

// CacheHitRatioTarget defines the per-tenant cache-hit-ratio floor
// that keeps Wasabi origin egress inside FairUseEgressBudget.
//
// The target is: observed hit ratio >= Min over the window. A lower
// observed ratio triggers an alert (and, optionally, a promotion
// sweep that pulls more objects into the Linode cache).
type CacheHitRatioTarget struct {
	// TenantID identifies the tenant.
	TenantID string `json:"tenant_id"`

	// Min is the minimum acceptable hit ratio in the range [0, 1].
	// Phase 3 default is 0.9 (see docs/PROGRESS.md "Key Metrics to
	// Track").
	Min float64 `json:"min"`

	// WindowDuration is the rolling window over which the ratio is
	// computed.
	WindowDuration time.Duration `json:"window_duration"`
}

// AlertThresholds groups the operational thresholds used by the
// alerting pipeline. A value of zero means "not configured" and the
// pipeline falls back to the global default.
type AlertThresholds struct {
	// EgressBudgetWarnRatio is the fraction of FairUseEgressBudget
	// that triggers a warning (e.g. 0.8 for 80% of the soft cap).
	EgressBudgetWarnRatio float64 `json:"egress_budget_warn_ratio"`

	// EgressBudgetCriticalRatio is the fraction of the hard cap that
	// triggers a critical alert (e.g. 0.95).
	EgressBudgetCriticalRatio float64 `json:"egress_budget_critical_ratio"`

	// CacheHitRatioAlertMin is the hit ratio below which the
	// pipeline raises an alert. Typically tracks
	// CacheHitRatioTarget.Min minus a small hysteresis band.
	CacheHitRatioAlertMin float64 `json:"cache_hit_ratio_alert_min"`

	// ThrottlePolicy names the action taken when HardCapBytes is
	// exceeded. Known values: "reject_origin_reads",
	// "slowdown_origin_reads", "notify_only".
	ThrottlePolicy string `json:"throttle_policy"`
}

// Guardrails is the full per-tenant Wasabi guardrail configuration.
// Instances are stored in the control plane and reloaded by the
// gateway on a short TTL so that operator edits take effect quickly.
type Guardrails struct {
	Budget     FairUseEgressBudget `json:"budget"`
	HitRatio   CacheHitRatioTarget `json:"hit_ratio"`
	Thresholds AlertThresholds     `json:"thresholds"`
	MinStorage time.Duration       `json:"min_storage_duration"`
}

// DefaultGuardrails returns Phase 2 default guardrails for tenantID.
// Operators may override any field.
func DefaultGuardrails(tenantID string) Guardrails {
	return Guardrails{
		Budget: FairUseEgressBudget{
			TenantID:           tenantID,
			WindowDuration:     30 * 24 * time.Hour,
			EgressStorageRatio: 1.0,
		},
		HitRatio: CacheHitRatioTarget{
			TenantID:       tenantID,
			Min:            0.9,
			WindowDuration: 30 * 24 * time.Hour,
		},
		Thresholds: AlertThresholds{
			EgressBudgetWarnRatio:     0.8,
			EgressBudgetCriticalRatio: 0.95,
			CacheHitRatioAlertMin:     0.85,
			ThrottlePolicy:            "slowdown_origin_reads",
		},
		MinStorage: WasabiMinStorageDays * 24 * time.Hour,
	}
}

// Validate performs structural checks on the guardrail configuration.
func (g Guardrails) Validate() error {
	if g.Budget.TenantID == "" {
		return fmt.Errorf("wasabi: guardrails budget.tenant_id is required")
	}
	if g.Budget.EgressStorageRatio <= 0 {
		return fmt.Errorf("wasabi: budget.egress_storage_ratio must be > 0")
	}
	if g.Budget.WindowDuration <= 0 {
		return fmt.Errorf("wasabi: budget.window_duration must be > 0")
	}
	if g.Budget.HardCapBytes != 0 && g.Budget.HardCapBytes < g.Budget.SoftCapBytes {
		return fmt.Errorf("wasabi: budget.hard_cap_bytes (%d) must be >= soft_cap_bytes (%d)", g.Budget.HardCapBytes, g.Budget.SoftCapBytes)
	}
	if g.HitRatio.Min < 0 || g.HitRatio.Min > 1 {
		return fmt.Errorf("wasabi: hit_ratio.min must be in [0, 1] (got %v)", g.HitRatio.Min)
	}
	if g.HitRatio.WindowDuration <= 0 {
		return fmt.Errorf("wasabi: hit_ratio.window_duration must be > 0")
	}
	if g.Thresholds.EgressBudgetWarnRatio < 0 || g.Thresholds.EgressBudgetWarnRatio > 1 {
		return fmt.Errorf("wasabi: thresholds.egress_budget_warn_ratio must be in [0, 1]")
	}
	if g.Thresholds.EgressBudgetCriticalRatio < 0 || g.Thresholds.EgressBudgetCriticalRatio > 1 {
		return fmt.Errorf("wasabi: thresholds.egress_budget_critical_ratio must be in [0, 1]")
	}
	if g.Thresholds.EgressBudgetCriticalRatio != 0 &&
		g.Thresholds.EgressBudgetWarnRatio > g.Thresholds.EgressBudgetCriticalRatio {
		return fmt.Errorf("wasabi: thresholds.egress_budget_warn_ratio (%v) must be <= critical_ratio (%v)",
			g.Thresholds.EgressBudgetWarnRatio, g.Thresholds.EgressBudgetCriticalRatio)
	}
	if g.MinStorage < 0 {
		return fmt.Errorf("wasabi: min_storage_duration must be non-negative")
	}
	return nil
}

// BillableAge returns the billable storage interval for a tracker,
// accounting for the 90-day minimum. For a piece still on disk it
// returns the elapsed time since StoredAt (clamped to >= MinStorage);
// for a deleted piece it returns MinStorage if the piece was deleted
// early, or the actual age otherwise.
func (m MinStorageTracker) BillableAge(now time.Time) time.Duration {
	if m.StoredAt.IsZero() {
		return 0
	}
	end := now
	if !m.DeletedAt.IsZero() {
		end = m.DeletedAt
	}
	age := end.Sub(m.StoredAt)
	if age < m.MinStorageDuration {
		return m.MinStorageDuration
	}
	return age
}

// MinStorageWarning describes the early-delete cost consequence of
// removing a Wasabi-resident object at a given instant. It is the
// public contract the placement engine, the S3 DELETE path, and the
// console use to surface Wasabi's otherwise-invisible 90-day minimum
// storage charge to operators.
type MinStorageWarning struct {
	// WithinMinStorageWindow is true when the object is younger than
	// the 90-day minimum, i.e. deleting it now would still incur
	// billable storage for the remainder of the window.
	WithinMinStorageWindow bool `json:"within_min_storage_window"`

	// RemainingDays is the whole number of days left before the
	// 90-day minimum elapses, rounded up so any partial day still
	// counts as remaining. It is 0 once the window has elapsed (and
	// whenever WithinMinStorageWindow is false). This is the value
	// surfaced as X-Zkof-Wasabi-Min-Storage-Remaining-Days and the
	// console's wasabi_min_storage_remaining_days field.
	RemainingDays int `json:"remaining_days"`

	// BillableRemainder is the residual storage time that Wasabi would
	// still bill if the object were deleted now: MinStorageDuration
	// minus the object's current age, clamped to [0, MinStorageDuration].
	// It is zero once the window has elapsed.
	BillableRemainder time.Duration `json:"billable_remainder"`
}

// MinStorageDurationWarning reports whether an object created at
// storedAt is still inside Wasabi's 90-day minimum storage window as
// of now, along with the residual billable time. Callers that know
// the object size can turn the residual into a dollar figure with
// EstimatedEarlyDeleteCostUSD.
//
// A zero storedAt yields an empty (not-within-window) warning: the
// age is unknown, so no early-delete charge can be asserted. Clock
// skew where now precedes storedAt is treated as a freshly written
// object (age 0), i.e. the full window remains.
func MinStorageDurationWarning(storedAt, now time.Time) MinStorageWarning {
	if storedAt.IsZero() {
		return MinStorageWarning{}
	}
	age := now.Sub(storedAt)
	if age < 0 {
		age = 0
	}
	if age >= minStorageDuration {
		return MinStorageWarning{}
	}
	remainder := minStorageDuration - age
	return MinStorageWarning{
		WithinMinStorageWindow: true,
		RemainingDays:          int(math.Ceil(remainder.Hours() / 24)),
		BillableRemainder:      remainder,
	}
}

// EstimatedEarlyDeleteCostUSD estimates the residual storage charge,
// in USD, of deleting an object of sizeBytes now. It is the cost of
// storing those bytes on Wasabi for BillableRemainder at the headline
// per-TB-month rate. It returns 0 once the object is past the 90-day
// minimum (BillableRemainder == 0) or for a non-positive size.
func (w MinStorageWarning) EstimatedEarlyDeleteCostUSD(sizeBytes int64) float64 {
	if sizeBytes <= 0 || w.BillableRemainder <= 0 {
		return 0
	}
	sizeTB := float64(sizeBytes) / 1e12
	months := w.BillableRemainder.Hours() / (24 * 30)
	return sizeTB * WasabiStorageUSDPerTBMonth * months
}
