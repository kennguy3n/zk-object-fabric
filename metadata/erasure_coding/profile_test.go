package erasure_coding

import (
	"math"
	"strings"
	"testing"
)

func TestStandardProfiles_Shapes(t *testing.T) {
	cases := []struct {
		profile ErasureCodingProfile
		k, m    int
	}{
		{Profile6Plus2, 6, 2},
		{Profile8Plus3, 8, 3},
		{Profile10Plus4, 10, 4},
		{Profile12Plus4, 12, 4},
		{Profile16Plus4, 16, 4},
	}
	for _, tc := range cases {
		t.Run(tc.profile.Name, func(t *testing.T) {
			if tc.profile.DataShards != tc.k {
				t.Fatalf("DataShards = %d, want %d", tc.profile.DataShards, tc.k)
			}
			if tc.profile.ParityShards != tc.m {
				t.Fatalf("ParityShards = %d, want %d", tc.profile.ParityShards, tc.m)
			}
			if tc.profile.TotalShards() != tc.k+tc.m {
				t.Fatalf("TotalShards = %d, want %d", tc.profile.TotalShards(), tc.k+tc.m)
			}
			if err := tc.profile.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestStandardProfiles_Overhead(t *testing.T) {
	cases := []struct {
		profile ErasureCodingProfile
		want    float64
	}{
		{Profile6Plus2, 8.0 / 6.0},
		{Profile8Plus3, 11.0 / 8.0},    // 1.375
		{Profile10Plus4, 14.0 / 10.0},  // 1.4
		{Profile12Plus4, 16.0 / 12.0},  // 1.333
		{Profile16Plus4, 20.0 / 16.0},  // 1.25
	}
	for _, tc := range cases {
		t.Run(tc.profile.Name, func(t *testing.T) {
			got := tc.profile.StorageOverhead()
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("StorageOverhead = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStandardProfiles_Slice(t *testing.T) {
	profiles := StandardProfiles()
	wantNames := []string{"6+2", "8+3", "10+4", "12+4", "16+4"}
	if len(profiles) != len(wantNames) {
		t.Fatalf("StandardProfiles length = %d, want %d", len(profiles), len(wantNames))
	}
	for i, want := range wantNames {
		if profiles[i].Name != want {
			t.Fatalf("StandardProfiles[%d].Name = %q, want %q", i, profiles[i].Name, want)
		}
	}
	// Mutating the returned slice must not affect later callers.
	profiles[0].Name = "mutated"
	if StandardProfiles()[0].Name != "6+2" {
		t.Fatal("StandardProfiles() must return a fresh slice")
	}
}

func TestValidate_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		profile ErasureCodingProfile
		wantSub string
	}{
		{"missing name", ErasureCodingProfile{DataShards: 8, ParityShards: 3, StripeSize: 4096}, "name is required"},
		{"zero data shards", ErasureCodingProfile{Name: "bad", DataShards: 0, ParityShards: 3, StripeSize: 4096}, "data_shards must be > 0"},
		{"zero parity shards", ErasureCodingProfile{Name: "bad", DataShards: 8, ParityShards: 0, StripeSize: 4096}, "parity_shards must be > 0"},
		{"zero stripe", ErasureCodingProfile{Name: "bad", DataShards: 8, ParityShards: 3, StripeSize: 0}, "stripe_size must be > 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.profile.Validate()
			if err == nil {
				t.Fatalf("Validate: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}
