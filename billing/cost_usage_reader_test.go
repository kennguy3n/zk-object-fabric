package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeWindowedUsage struct {
	totals map[string]uint64
	err    error

	gotStart time.Time
	gotEnd   time.Time
}

func (f *fakeWindowedUsage) TenantUsage(_ context.Context, _ string, start, end time.Time) (map[string]uint64, error) {
	f.gotStart = start
	f.gotEnd = end
	if f.err != nil {
		return nil, f.err
	}
	return f.totals, nil
}

func TestMeteredStorageUsageReader_ConvertsBytesSecondsToGiBMonth(t *testing.T) {
	// June 2026 has 30 days = 2_592_000 seconds. Hold exactly 4 GiB
	// for the whole month → bytes-seconds = 4 GiB * secondsInMonth,
	// so LogicalGiBMonth must come back as 4.
	const seconds = 30 * 24 * 60 * 60
	src := &fakeWindowedUsage{totals: map[string]uint64{
		string(StorageBytesSeconds): uint64(4 * gibibyte * seconds),
		string(DedupBytesSaved):     7 * gibibyte, // must NOT leak into the result
	}}
	r := NewMeteredStorageUsageReader(src)

	got, err := r.StorageUsage(context.Background(), "acme", "2026-06")
	if err != nil {
		t.Fatalf("StorageUsage: %v", err)
	}
	if got.LogicalGiBMonth < 3.999 || got.LogicalGiBMonth > 4.001 {
		t.Fatalf("LogicalGiBMonth = %v, want ~4", got.LogicalGiBMonth)
	}
	// DedupBytesSaved is a one-time byte count, not GiB-month, so the
	// reader must report zero savings rather than a fabricated figure.
	if got.DedupSavedGiBMonth != 0 {
		t.Fatalf("DedupSavedGiBMonth = %v, want 0", got.DedupSavedGiBMonth)
	}
	// The window handed to the source must be the canonical month.
	if !src.gotStart.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("start = %v, want 2026-06-01 UTC", src.gotStart)
	}
	if !src.gotEnd.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("end = %v, want 2026-07-01 UTC", src.gotEnd)
	}
}

func TestMeteredStorageUsageReader_EmptyMonthUsesClock(t *testing.T) {
	src := &fakeWindowedUsage{totals: map[string]uint64{}}
	r := NewMeteredStorageUsageReader(src)
	r.clock = func() time.Time { return time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC) }

	if _, err := r.StorageUsage(context.Background(), "acme", ""); err != nil {
		t.Fatalf("StorageUsage: %v", err)
	}
	if !src.gotStart.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("start = %v, want 2026-03-01 UTC", src.gotStart)
	}
	if !src.gotEnd.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("end = %v, want 2026-04-01 UTC", src.gotEnd)
	}
}

func TestMeteredStorageUsageReader_InvalidMonth(t *testing.T) {
	r := NewMeteredStorageUsageReader(&fakeWindowedUsage{})
	if _, err := r.StorageUsage(context.Background(), "acme", "not-a-month"); err == nil {
		t.Fatal("expected error for malformed month")
	}
}

func TestMeteredStorageUsageReader_PropagatesSourceError(t *testing.T) {
	sentinel := errors.New("clickhouse down")
	r := NewMeteredStorageUsageReader(&fakeWindowedUsage{err: sentinel})
	if _, err := r.StorageUsage(context.Background(), "acme", "2026-06"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped %v", err, sentinel)
	}
}

func TestMeteredStorageUsageReader_NilSource(t *testing.T) {
	var r *MeteredStorageUsageReader
	if _, err := r.StorageUsage(context.Background(), "acme", "2026-06"); err == nil {
		t.Fatal("expected error on nil reader")
	}
}
