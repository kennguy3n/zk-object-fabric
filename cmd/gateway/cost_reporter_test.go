// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/internal/metrics"
)

// readCapableSink satisfies billing.WindowedUsageSource (the metering
// read-side) in addition to BillingSink.
type readCapableSink struct{}

func (readCapableSink) Emit(billing.UsageEvent) {}
func (readCapableSink) TenantUsage(context.Context, string, time.Time, time.Time) (map[string]uint64, error) {
	return map[string]uint64{}, nil
}

// emitOnlySink is a write-only sink (e.g. a ClickHouse-only deploy
// without windowed reads): it has no TenantUsage.
type emitOnlySink struct{}

func (emitOnlySink) Emit(billing.UsageEvent) {}

func TestWindowedReadSide(t *testing.T) {
	reg := metrics.NewRegistry()
	tests := []struct {
		name    string
		sink    billing.BillingSink
		wantOK  bool
	}{
		{"unwrapped read-capable", readCapableSink{}, true},
		{"unwrapped emit-only", emitOnlySink{}, false},
		{
			// The metrics wrapper exposes its own TenantUsage, so a
			// naive assert would say true; unwrapping must see the
			// read-less inner and return false (no $0 route).
			name:   "metrics-wrapped emit-only",
			sink:   metrics.NewMetricsBillingSink(emitOnlySink{}, reg),
			wantOK: false,
		},
		{
			name:   "metrics-wrapped read-capable",
			sink:   metrics.NewMetricsBillingSink(readCapableSink{}, reg),
			wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := windowedReadSide(tc.sink)
			if ok != tc.wantOK {
				t.Fatalf("windowedReadSide ok = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

func TestParseCostModel_RejectsNonFiniteAndNegative(t *testing.T) {
	for _, raw := range []string{"-1", "Inf", "+Inf", "-Inf", "NaN", "abc"} {
		if _, ok := parseCostModel(map[string]string{costKeyWasabiUSDPerGiBMonth: raw}); ok {
			t.Errorf("parseCostModel accepted invalid wasabi rate %q", raw)
		}
	}
	if _, ok := parseCostModel(map[string]string{}); ok {
		t.Error("parseCostModel accepted a missing wasabi rate")
	}
	m, ok := parseCostModel(map[string]string{costKeyWasabiUSDPerGiBMonth: "0.021"})
	if !ok || m.WasabiUSDPerGiBMonth != 0.021 {
		t.Fatalf("parseCostModel valid rate: ok=%v model=%+v", ok, m)
	}
	// A zero rate is a deliberate free-tier opt-in and stays valid.
	if _, ok := parseCostModel(map[string]string{costKeyWasabiUSDPerGiBMonth: "0"}); !ok {
		t.Error("parseCostModel rejected a zero (free-tier) rate")
	}
}

func TestParseOptionalUSD_TreatsNonFiniteAsZero(t *testing.T) {
	const key = costKeyLinodeMonthlyUSD
	for _, raw := range []string{"Inf", "NaN", "-5", "junk"} {
		if got := parseOptionalUSD(map[string]string{key: raw}, key); got != 0 {
			t.Errorf("parseOptionalUSD(%q) = %v, want 0", raw, got)
		}
	}
	if got := parseOptionalUSD(map[string]string{key: "12.5"}, key); got != 12.5 {
		t.Errorf("parseOptionalUSD(valid) = %v, want 12.5", got)
	}
}

func TestIsFiniteNonNegative(t *testing.T) {
	for _, v := range []float64{0, 0.5, 1e9} {
		if !isFiniteNonNegative(v) {
			t.Errorf("isFiniteNonNegative(%v) = false, want true", v)
		}
	}
	for _, v := range []float64{-1, math.Inf(1), math.Inf(-1), math.NaN()} {
		if isFiniteNonNegative(v) {
			t.Errorf("isFiniteNonNegative(%v) = true, want false", v)
		}
	}
}
