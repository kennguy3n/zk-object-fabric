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
	"time"
)

// WasabiMinStorageDays is Wasabi's 90-day minimum storage duration.
// Objects deleted before this window still incur 90 days of billable
// storage. The data plane must not write short-TTL objects to Wasabi;
// those belong in the Linode cache (see cache/hot_object_cache).
const WasabiMinStorageDays = 90

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
