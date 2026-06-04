package wasabi

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestDefaultGuardrails_Valid(t *testing.T) {
	g := DefaultGuardrails("tnt_123")
	if err := g.Validate(); err != nil {
		t.Fatalf("DefaultGuardrails.Validate: %v", err)
	}
	if g.MinStorage != WasabiMinStorageDays*24*time.Hour {
		t.Fatalf("MinStorage = %v, want %v", g.MinStorage, WasabiMinStorageDays*24*time.Hour)
	}
	if g.Budget.EgressStorageRatio != 1.0 {
		t.Fatalf("Budget.EgressStorageRatio = %v, want 1.0 (Wasabi fair-use ceiling)", g.Budget.EgressStorageRatio)
	}
}

func TestGuardrails_ValidateRejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Guardrails)
		wantSub string
	}{
		{"missing tenant", func(g *Guardrails) { g.Budget.TenantID = "" }, "tenant_id is required"},
		{"zero ratio", func(g *Guardrails) { g.Budget.EgressStorageRatio = 0 }, "egress_storage_ratio"},
		{"zero window", func(g *Guardrails) { g.Budget.WindowDuration = 0 }, "window_duration"},
		{"hard below soft", func(g *Guardrails) {
			g.Budget.SoftCapBytes = 100
			g.Budget.HardCapBytes = 50
		}, "hard_cap_bytes"},
		{"bad hit ratio", func(g *Guardrails) { g.HitRatio.Min = 1.5 }, "hit_ratio.min"},
		{"warn > critical", func(g *Guardrails) {
			g.Thresholds.EgressBudgetWarnRatio = 0.9
			g.Thresholds.EgressBudgetCriticalRatio = 0.5
		}, "warn_ratio"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := DefaultGuardrails("tnt")
			tc.mutate(&g)
			err := g.Validate()
			if err == nil {
				t.Fatalf("Validate: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestMinStorageTracker_BillableAge(t *testing.T) {
	minDur := 90 * 24 * time.Hour
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		tracker MinStorageTracker
		now     time.Time
		want    time.Duration
	}{
		{
			name:    "still stored, past min",
			tracker: MinStorageTracker{StoredAt: start, MinStorageDuration: minDur},
			now:     start.Add(100 * 24 * time.Hour),
			want:    100 * 24 * time.Hour,
		},
		{
			name:    "still stored, under min",
			tracker: MinStorageTracker{StoredAt: start, MinStorageDuration: minDur},
			now:     start.Add(10 * 24 * time.Hour),
			want:    minDur,
		},
		{
			name:    "deleted early",
			tracker: MinStorageTracker{StoredAt: start, DeletedAt: start.Add(10 * 24 * time.Hour), MinStorageDuration: minDur},
			now:     start.Add(100 * 24 * time.Hour),
			want:    minDur,
		},
		{
			name:    "deleted past min",
			tracker: MinStorageTracker{StoredAt: start, DeletedAt: start.Add(120 * 24 * time.Hour), MinStorageDuration: minDur},
			now:     start.Add(200 * 24 * time.Hour),
			want:    120 * 24 * time.Hour,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.tracker.BillableAge(tc.now)
			if got != tc.want {
				t.Fatalf("BillableAge = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMinStorageDurationWarning(t *testing.T) {
	day := 24 * time.Hour
	stored := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name          string
		storedAt      time.Time
		now           time.Time
		wantWithin    bool
		wantRemaining int
		wantRemainder time.Duration
	}{
		{
			name:          "inside window, fresh write",
			storedAt:      stored,
			now:           stored,
			wantWithin:    true,
			wantRemaining: WasabiMinStorageDays,
			wantRemainder: minStorageDuration,
		},
		{
			name:          "inside window, mid",
			storedAt:      stored,
			now:           stored.Add(10 * day),
			wantWithin:    true,
			wantRemaining: 80,
			wantRemainder: 80 * day,
		},
		{
			name:          "inside window, partial day rounds up",
			storedAt:      stored,
			now:           stored.Add(89*day + 12*time.Hour),
			wantWithin:    true,
			wantRemaining: 1,
			wantRemainder: 12 * time.Hour,
		},
		{
			name:          "boundary: exactly 90 days is outside",
			storedAt:      stored,
			now:           stored.Add(90 * day),
			wantWithin:    false,
			wantRemaining: 0,
			wantRemainder: 0,
		},
		{
			name:          "outside window",
			storedAt:      stored,
			now:           stored.Add(120 * day),
			wantWithin:    false,
			wantRemaining: 0,
			wantRemainder: 0,
		},
		{
			name:          "clock skew: now before storedAt is treated as fresh",
			storedAt:      stored,
			now:           stored.Add(-5 * day),
			wantWithin:    true,
			wantRemaining: WasabiMinStorageDays,
			wantRemainder: minStorageDuration,
		},
		{
			name:          "zero storedAt yields empty warning",
			storedAt:      time.Time{},
			now:           stored,
			wantWithin:    false,
			wantRemaining: 0,
			wantRemainder: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MinStorageDurationWarning(tc.storedAt, tc.now)
			if got.WithinMinStorageWindow != tc.wantWithin {
				t.Errorf("WithinMinStorageWindow = %v, want %v", got.WithinMinStorageWindow, tc.wantWithin)
			}
			if got.RemainingDays != tc.wantRemaining {
				t.Errorf("RemainingDays = %d, want %d", got.RemainingDays, tc.wantRemaining)
			}
			if got.BillableRemainder != tc.wantRemainder {
				t.Errorf("BillableRemainder = %v, want %v", got.BillableRemainder, tc.wantRemainder)
			}
		})
	}
}

func TestMinStorageWarning_EstimatedEarlyDeleteCostUSD(t *testing.T) {
	day := 24 * time.Hour
	stored := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Deleting a 1 TB object on day 0 still owes the full 90-day
	// minimum: 90/30 months * $6.99/TB-month = $20.97.
	w := MinStorageDurationWarning(stored, stored)
	got := w.EstimatedEarlyDeleteCostUSD(1e12)
	want := WasabiStorageUSDPerTBMonth * (90.0 / 30.0)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("cost on day 0 = %v, want %v", got, want)
	}

	// Past the window there is no residual charge.
	wOut := MinStorageDurationWarning(stored, stored.Add(120*day))
	if c := wOut.EstimatedEarlyDeleteCostUSD(1e12); c != 0 {
		t.Errorf("cost past window = %v, want 0", c)
	}

	// Non-positive size has no cost even inside the window.
	if c := w.EstimatedEarlyDeleteCostUSD(0); c != 0 {
		t.Errorf("cost for zero size = %v, want 0", c)
	}
}
