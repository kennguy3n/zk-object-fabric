// Forecasting derives "when does this cell run out of capacity"
// projections from the billing usage stream.
//
// The Forecaster is intentionally model-agnostic: it consumes a
// time series of (timestamp, stored bytes, dedup bytes saved)
// samples from a UsageQuery (typically a ClickHouse-backed
// implementation) and runs a linear regression on the
// post-dedup byte count. Operators that need an exponential or
// per-tenant model can plug their own GrowthModel through the
// Forecaster.Model field.
//
// All math runs in this package; no external dependencies are
// added. The control-plane handler at api/console/forecast_handler.go
// surfaces the result as a JSON document.
package billing

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"
)

// UsageSample is one snapshot of stored / dedup bytes for a cell
// at a given instant. Timestamps are monotonic; the Forecaster
// resorts on them defensively.
type UsageSample struct {
	Timestamp        time.Time
	StorageBytes     uint64
	DedupBytesSaved  uint64
}

// EffectiveBytes is the post-dedup ciphertext byte count the
// cell is actually carrying on disk. Forecasts run on this
// value, not on raw StorageBytes, so dedup savings are reflected
// in the projected fill date.
func (s UsageSample) EffectiveBytes() uint64 {
	if s.DedupBytesSaved >= s.StorageBytes {
		return 0
	}
	return s.StorageBytes - s.DedupBytesSaved
}

// UsageQuery is the read surface the Forecaster relies on. The
// production implementation hits ClickHouse's usage_counters
// table; tests pass an in-memory fake.
type UsageQuery interface {
	StorageHistory(ctx context.Context, cellID string) ([]UsageSample, error)
}

// GrowthModel projects the effective byte count at a future
// instant. The default implementation is a linear least-squares
// fit; deployments can swap in an exponential or smoothed
// estimator.
type GrowthModel func(samples []UsageSample, at time.Time) (predicted uint64, slopeBytesPerSec float64)

// Forecaster computes capacity forecasts for one or more cells.
type Forecaster struct {
	// Query is the time-series source. Required.
	Query UsageQuery

	// Model defaults to LinearGrowth.
	Model GrowthModel

	// AlertWindow is how far in advance of the projected fill
	// date the forecaster flags Result.Alert. Defaults to 90
	// days (the operating threshold called out in the Phase 4
	// product brief).
	AlertWindow time.Duration

	// Clock defaults to time.Now.
	Clock func() time.Time
}

// ForecastResult is the per-cell projection emitted by Forecast.
type ForecastResult struct {
	CellID                 string    `json:"cell_id"`
	CapacityBytes          uint64    `json:"capacity_bytes"`
	CurrentBytes           uint64    `json:"current_bytes"`
	UtilizationFraction    float64   `json:"utilization_fraction"`
	GrowthBytesPerSec      float64   `json:"growth_bytes_per_sec"`
	ProjectedFillAt        time.Time `json:"projected_fill_at"`
	ProjectedFillFromNow   string    `json:"projected_fill_from_now"`
	Alert                  bool      `json:"alert"`
	SampleCount            int       `json:"sample_count"`
}

// Forecast runs the projection for a single cell.
func (f *Forecaster) Forecast(ctx context.Context, cellID string, capacityBytes uint64) (ForecastResult, error) {
	if f == nil || f.Query == nil {
		return ForecastResult{}, errors.New("billing: forecaster has no UsageQuery")
	}
	samples, err := f.Query.StorageHistory(ctx, cellID)
	if err != nil {
		return ForecastResult{}, err
	}
	if len(samples) == 0 {
		return ForecastResult{
			CellID:        cellID,
			CapacityBytes: capacityBytes,
		}, nil
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].Timestamp.Before(samples[j].Timestamp) })
	model := f.Model
	if model == nil {
		model = LinearGrowth
	}
	now := f.now()
	current := samples[len(samples)-1].EffectiveBytes()
	_, slope := model(samples, now)
	res := ForecastResult{
		CellID:            cellID,
		CapacityBytes:     capacityBytes,
		CurrentBytes:      current,
		GrowthBytesPerSec: slope,
		SampleCount:       len(samples),
	}
	if capacityBytes > 0 {
		res.UtilizationFraction = float64(current) / float64(capacityBytes)
	}
	if slope <= 0 || capacityBytes == 0 || current >= capacityBytes {
		// Cell is shrinking, has no declared capacity, or is
		// already over-provisioned. Leave ProjectedFillAt zero
		// and alert if already over capacity.
		res.Alert = current >= capacityBytes && capacityBytes > 0
		if res.Alert {
			res.ProjectedFillAt = now
			res.ProjectedFillFromNow = "0s"
		}
		return res, nil
	}
	remaining := float64(capacityBytes - current)
	secondsToFill := remaining / slope
	// time.Duration is an int64 nanosecond count; multiplying a
	// float seconds value by time.Second can overflow for very
	// long fill horizons (>~292y) and wrap to a negative
	// Duration, producing a past ProjectedFillAt and a spurious
	// alert. Clamp at the int64-seconds ceiling and leave
	// ProjectedFillAt zero when the projection overflows.
	const maxSeconds = float64(math.MaxInt64 / int64(time.Second))
	// Default the alert window via a local shadow; mutating the
	// shared *Forecaster field at request time would race with
	// concurrent Forecast goroutines reading the same struct.
	alertWindow := f.AlertWindow
	if alertWindow == 0 {
		alertWindow = 90 * 24 * time.Hour
	}
	if math.IsNaN(secondsToFill) || math.IsInf(secondsToFill, 0) || secondsToFill > maxSeconds {
		// Leave ProjectedFillAt zero so consumers can render
		// the indefinite horizon as "no foreseeable fill".
		res.Alert = false
		return res, nil
	}
	res.ProjectedFillAt = now.Add(time.Duration(secondsToFill) * time.Second)
	res.ProjectedFillFromNow = res.ProjectedFillAt.Sub(now).Truncate(time.Hour).String()
	res.Alert = res.ProjectedFillAt.Sub(now) <= alertWindow
	return res, nil
}

func (f *Forecaster) now() time.Time {
	if f.Clock != nil {
		return f.Clock()
	}
	return time.Now()
}

// LinearGrowth fits a least-squares line through the post-dedup
// byte counts and projects forward to `at`.
//
// Fewer than two samples → slope = 0, predicted = last sample's
// EffectiveBytes (or zero on an empty input).
func LinearGrowth(samples []UsageSample, at time.Time) (uint64, float64) {
	n := len(samples)
	if n == 0 {
		return 0, 0
	}
	if n == 1 {
		return samples[0].EffectiveBytes(), 0
	}
	t0 := samples[0].Timestamp
	var sumX, sumY, sumXX, sumXY float64
	for _, s := range samples {
		x := s.Timestamp.Sub(t0).Seconds()
		y := float64(s.EffectiveBytes())
		sumX += x
		sumY += y
		sumXX += x * x
		sumXY += x * y
	}
	denom := float64(n)*sumXX - sumX*sumX
	if denom == 0 {
		return samples[n-1].EffectiveBytes(), 0
	}
	slope := (float64(n)*sumXY - sumX*sumY) / denom
	intercept := (sumY - slope*sumX) / float64(n)
	x := at.Sub(t0).Seconds()
	predicted := slope*x + intercept
	if predicted < 0 || math.IsNaN(predicted) || math.IsInf(predicted, 0) {
		predicted = 0
	}
	return uint64(predicted), slope
}
