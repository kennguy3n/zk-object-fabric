package compliance

import (
	"context"
	"testing"
	"time"
)

func TestMemoryAuditStore_RoundTrip(t *testing.T) {
	s := NewMemoryAuditStore()
	ctx := context.Background()
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	for i, op := range []string{"PUT", "GET", "DELETE"} {
		if err := s.Record(ctx, AuditEntry{
			TenantID:       "tenant-A",
			Operation:      op,
			Bucket:         "b",
			ObjectKey:      "k",
			PieceBackend:   "wasabi",
			BackendCountry: "US",
			Timestamp:      t0.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	// Foreign tenant row that must not show up in the query.
	_ = s.Record(ctx, AuditEntry{TenantID: "tenant-B", Operation: "PUT"})

	got, err := s.Query(ctx, "tenant-A", TimeRange{Start: t0, End: t0.Add(2 * time.Hour)})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	for i, op := range []string{"PUT", "GET", "DELETE"} {
		if got[i].Operation != op {
			t.Errorf("row %d Operation = %q, want %q", i, got[i].Operation, op)
		}
	}
}

func TestMemoryAuditStore_RangeFilter(t *testing.T) {
	s := NewMemoryAuditStore()
	ctx := context.Background()
	t0 := time.Now().UTC()
	for i := 0; i < 5; i++ {
		_ = s.Record(ctx, AuditEntry{TenantID: "T", Timestamp: t0.Add(time.Duration(i) * time.Minute)})
	}
	got, err := s.Query(ctx, "T", TimeRange{Start: t0.Add(2 * time.Minute), End: t0.Add(3 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}

func TestMemoryAuditStore_DefaultsTimestamp(t *testing.T) {
	s := NewMemoryAuditStore()
	if err := s.Record(context.Background(), AuditEntry{TenantID: "X"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Query(context.Background(), "X", TimeRange{})
	if len(got) != 1 || got[0].Timestamp.IsZero() {
		t.Errorf("expected one row with non-zero timestamp, got %+v", got)
	}
}
