package billing

import (
	"context"
	"testing"
	"time"
)

type stubQuery struct {
	samples []UsageSample
	err     error
}

func (s *stubQuery) StorageHistory(_ context.Context, _ string) ([]UsageSample, error) {
	return s.samples, s.err
}

func samplesAt(start time.Time, perDay uint64, days int) []UsageSample {
	out := make([]UsageSample, 0, days)
	for i := 0; i < days; i++ {
		out = append(out, UsageSample{
			Timestamp:    start.Add(time.Duration(i) * 24 * time.Hour),
			StorageBytes: perDay * uint64(i+1),
		})
	}
	return out
}

func TestForecaster_LinearGrowthHitsCapacityWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 100 GB/day for 30 days => 3 TB at start; capacity = 4 TB =>
	// fills in ~10 days.
	const gb = uint64(1) << 30
	const tb = uint64(1) << 40
	samples := samplesAt(now.Add(-30*24*time.Hour), 100*gb, 30)
	f := &Forecaster{
		Query: &stubQuery{samples: samples},
		Clock: func() time.Time { return now },
	}
	res, err := f.Forecast(context.Background(), "cell-1", 4*tb)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Alert {
		t.Errorf("expected alert when projected fill < 90d; got %+v", res)
	}
	if res.ProjectedFillAt.Before(now) || res.ProjectedFillAt.After(now.Add(60*24*time.Hour)) {
		t.Errorf("ProjectedFillAt = %v, want within ~60d of now", res.ProjectedFillAt)
	}
	if res.GrowthBytesPerSec <= 0 {
		t.Errorf("GrowthBytesPerSec = %f, want > 0", res.GrowthBytesPerSec)
	}
}

func TestForecaster_DedupShrinksEffectiveBytes(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const gb = uint64(1) << 30
	samples := []UsageSample{
		{Timestamp: now.Add(-2 * 24 * time.Hour), StorageBytes: 1000 * gb, DedupBytesSaved: 800 * gb},
		{Timestamp: now.Add(-1 * 24 * time.Hour), StorageBytes: 1100 * gb, DedupBytesSaved: 900 * gb},
		{Timestamp: now, StorageBytes: 1200 * gb, DedupBytesSaved: 1000 * gb},
	}
	f := &Forecaster{Query: &stubQuery{samples: samples}, Clock: func() time.Time { return now }}
	res, err := f.Forecast(context.Background(), "cell-2", 1000*gb)
	if err != nil {
		t.Fatal(err)
	}
	// After dedup, current effective bytes = 200 GB, well under 1 TB capacity.
	if res.CurrentBytes != 200*gb {
		t.Errorf("CurrentBytes = %d, want %d (post-dedup)", res.CurrentBytes, 200*gb)
	}
	if res.Alert {
		t.Errorf("dedup-suppressed cell should not alert")
	}
}

func TestForecaster_NoSamplesIsNoError(t *testing.T) {
	f := &Forecaster{Query: &stubQuery{}}
	res, err := f.Forecast(context.Background(), "cell-3", 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if res.SampleCount != 0 || res.GrowthBytesPerSec != 0 || res.Alert {
		t.Errorf("empty history must produce empty result, got %+v", res)
	}
}

func TestForecaster_ShrinkingCellNeverAlerts(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const gb = uint64(1) << 30
	samples := []UsageSample{
		{Timestamp: now.Add(-3 * 24 * time.Hour), StorageBytes: 1000 * gb},
		{Timestamp: now.Add(-2 * 24 * time.Hour), StorageBytes: 900 * gb},
		{Timestamp: now.Add(-1 * 24 * time.Hour), StorageBytes: 800 * gb},
		{Timestamp: now, StorageBytes: 700 * gb},
	}
	f := &Forecaster{Query: &stubQuery{samples: samples}, Clock: func() time.Time { return now }}
	res, err := f.Forecast(context.Background(), "cell-4", 1000*gb)
	if err != nil {
		t.Fatal(err)
	}
	if res.Alert {
		t.Errorf("shrinking cell must not alert")
	}
	if !res.ProjectedFillAt.IsZero() {
		t.Errorf("shrinking cell must leave ProjectedFillAt zero, got %v", res.ProjectedFillAt)
	}
}

func TestForecaster_OverCapacityAlertsImmediately(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const gb = uint64(1) << 30
	samples := []UsageSample{
		{Timestamp: now.Add(-1 * 24 * time.Hour), StorageBytes: 1100 * gb},
		{Timestamp: now, StorageBytes: 1200 * gb},
	}
	f := &Forecaster{Query: &stubQuery{samples: samples}, Clock: func() time.Time { return now }}
	res, err := f.Forecast(context.Background(), "cell-5", 1000*gb)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Alert {
		t.Errorf("over-capacity cell must alert")
	}
}
