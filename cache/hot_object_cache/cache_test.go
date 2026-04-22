package hot_object_cache

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultPromotionPolicies_Valid(t *testing.T) {
	ps := DefaultPromotionPolicies()
	if len(ps) != 2 {
		t.Fatalf("DefaultPromotionPolicies length = %d, want 2", len(ps))
	}
	if ps[0].Tier != TierL0 || ps[1].Tier != TierL1 {
		t.Fatalf("tier ordering = [%q, %q], want [l0, l1]", ps[0].Tier, ps[1].Tier)
	}
	for _, p := range ps {
		if err := p.Validate(); err != nil {
			t.Fatalf("PromotionPolicy %q Validate: %v", p.Tier, err)
		}
	}
}

func TestPromotionPolicy_ValidateRejects(t *testing.T) {
	cases := []struct {
		name    string
		policy  PromotionPolicy
		wantSub string
	}{
		{"missing tier", PromotionPolicy{}, "tier is required"},
		{"unknown tier", PromotionPolicy{Tier: "l9"}, "unknown tier"},
		{"negative ratio", PromotionPolicy{Tier: TierL0, MonthlyEgressRatioThreshold: -1}, "monthly_egress_ratio"},
		{"negative p95", PromotionPolicy{Tier: TierL0, P95LatencyMissMs: -1}, "p95_latency_miss_ms"},
		{"min > max", PromotionPolicy{Tier: TierL0, MinPieceSizeBytes: 100, MaxPieceSizeBytes: 10}, "min_piece_size_bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
			if err == nil {
				t.Fatalf("Validate: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestDefaultEvictionPolicy_Valid(t *testing.T) {
	e := DefaultEvictionPolicy(10 * 1024 * 1024 * 1024)
	if err := e.Validate(); err != nil {
		t.Fatalf("DefaultEvictionPolicy.Validate: %v", err)
	}
	if e.Kind != EvictionLRUHotPin {
		t.Fatalf("Kind = %q, want %q", e.Kind, EvictionLRUHotPin)
	}
}

func TestEvictionPolicy_ValidateRejects(t *testing.T) {
	cases := []struct {
		name    string
		policy  EvictionPolicy
		wantSub string
	}{
		{"missing kind", EvictionPolicy{}, "kind is required"},
		{"unknown kind", EvictionPolicy{Kind: "ramdom"}, "unknown eviction kind"},
		{"negative max", EvictionPolicy{Kind: EvictionLRU, MaxBytes: -1}, "max_bytes"},
		{"bad hot fraction", EvictionPolicy{Kind: EvictionLRUHotPin, HotRegionFraction: 1.0}, "hot_region_fraction"},
		{"negative ttl", EvictionPolicy{Kind: EvictionLRU, TTL: -1 * time.Second}, "ttl"},
		{"lru with hot-pin field", EvictionPolicy{Kind: EvictionLRU, HotRegionFraction: 0.1}, "hot-pin fields"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
			if err == nil {
				t.Fatalf("Validate: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}
