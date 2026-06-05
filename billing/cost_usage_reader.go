package billing

import (
	"context"
	"fmt"
	"time"
)

// gibibyte is 1 GiB in bytes and the package's single source of truth
// for that magnitude; storage volumes are priced per GiB-month so the
// reader divides the metered byte figures by this. forecasting.go's
// bytesPerGiB is derived from it so the two cannot drift apart.
const gibibyte = 1 << 30

// WindowedUsageSource is the metering read-side the cost reader
// aggregates over: the summed counter Delta per dimension for a
// tenant over the half-open period [start, end). The embedded
// SQLiteSink satisfies it directly (it is the same shape the console
// uses as UsageQuery), so the cost surface reuses the metering store
// the gateway already wires rather than opening a second one.
//
// The returned map is keyed by the Dimension string and holds the
// total bytes / bytes-seconds for that dimension over the window.
type WindowedUsageSource interface {
	TenantUsage(ctx context.Context, tenantID string, start, end time.Time) (map[string]uint64, error)
}

// MeteredStorageUsageReader is the production StorageUsageReader. It
// turns the metering pipeline's per-month counter rollup into the
// GiB-month figures the CostAggregator prices.
//
// LogicalGiBMonth is derived from the StorageBytesSeconds dimension
// (the time-integral of stored ciphertext bytes): dividing the
// month's bytes-seconds by the number of seconds in that month
// yields the average bytes held, and dividing by 1 GiB expresses it
// as GiB-month. This is exactly the volume Wasabi bills for.
//
// DedupSavedGiBMonth is intentionally left at zero. The only metered
// dedup signal today is DedupBytesSaved, a one-time byte count of
// avoided writes (see metering.go) — NOT a time-integrated
// bytes-seconds figure — so it cannot be converted to GiB-month
// without assuming how long the saved bytes would have been retained.
// Reporting it as a fabricated GiB-month would put a wrong dollar
// figure on the customer-facing breakdown, so the reader reports the
// (correct) billed Wasabi cost with zero dedup savings until a
// time-integrated dedup dimension (e.g. dedup_bytes_seconds_saved)
// exists. The aggregator's clamp keeps this consistent.
type MeteredStorageUsageReader struct {
	src   WindowedUsageSource
	clock func() time.Time
}

// NewMeteredStorageUsageReader returns a StorageUsageReader backed by
// src. src must be non-nil.
func NewMeteredStorageUsageReader(src WindowedUsageSource) *MeteredStorageUsageReader {
	return &MeteredStorageUsageReader{src: src}
}

// StorageUsage implements StorageUsageReader. month is a year-month
// key ("2006-01"); an empty month resolves to the current month so
// the reader matches the aggregator's own current-month behavior.
func (r *MeteredStorageUsageReader) StorageUsage(ctx context.Context, tenantID, month string) (StorageUsage, error) {
	if r == nil || r.src == nil {
		return StorageUsage{}, fmt.Errorf("billing: metered storage usage reader has no source")
	}
	start, end, err := monthWindow(month, r.now())
	if err != nil {
		return StorageUsage{}, err
	}
	totals, err := r.src.TenantUsage(ctx, tenantID, start, end)
	if err != nil {
		return StorageUsage{}, err
	}
	seconds := end.Sub(start).Seconds()
	if seconds <= 0 {
		return StorageUsage{}, fmt.Errorf("billing: non-positive month window for %q", month)
	}
	bytesSeconds := totals[string(StorageBytesSeconds)]
	logicalGiBMonth := float64(bytesSeconds) / seconds / gibibyte
	return StorageUsage{
		LogicalGiBMonth: logicalGiBMonth,
		// DedupSavedGiBMonth left zero; see type doc.
	}, nil
}

func (r *MeteredStorageUsageReader) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// monthWindow returns the half-open [start, end) UTC instants for the
// year-month key ("2006-01"). An empty month resolves to the current
// month relative to now.
func monthWindow(month string, now time.Time) (start, end time.Time, err error) {
	if month == "" {
		month = now.UTC().Format(monthLayout)
	}
	start, err = time.ParseInLocation(monthLayout, month, time.UTC)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("billing: invalid month %q: %w", month, err)
	}
	end = start.AddDate(0, 1, 0)
	return start, end, nil
}

// static interface check
var _ StorageUsageReader = (*MeteredStorageUsageReader)(nil)
