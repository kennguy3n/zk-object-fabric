package wasabi

import (
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
