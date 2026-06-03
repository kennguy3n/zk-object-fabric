package billing

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
)

func newTestSink(t *testing.T) *SQLiteSink {
	t.Helper()
	db, err := embeddeddb.Open(filepath.Join(t.TempDir(), "billing.db"))
	if err != nil {
		t.Fatalf("open embedded db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := NewSQLiteSink(SQLiteSinkConfig{DB: db})
	if err != nil {
		t.Fatalf("new sink: %v", err)
	}
	return s
}

func TestSQLiteSink_EmitAggregates(t *testing.T) {
	t.Parallel()
	s := newTestSink(t)
	ctx := context.Background()

	s.Emit(UsageEvent{TenantID: "t1", Bucket: "b1", Dimension: PutRequests, Delta: 1, ObservedAt: time.Unix(1, 0)})
	s.Emit(UsageEvent{TenantID: "t1", Bucket: "b1", Dimension: PutRequests, Delta: 4, ObservedAt: time.Unix(2, 0)})
	s.Emit(UsageEvent{TenantID: "t1", Bucket: "b1", Dimension: EgressBytes, Delta: 100, ObservedAt: time.Unix(3, 0)})
	s.Emit(UsageEvent{TenantID: "t1", Bucket: "b2", Dimension: PutRequests, Delta: 2, ObservedAt: time.Unix(4, 0)})
	s.Flush(ctx)

	got, err := s.Total(ctx, "t1", "b1", PutRequests)
	if err != nil {
		t.Fatalf("Total: %v", err)
	}
	if got != 5 {
		t.Fatalf("Total(t1,b1,put) = %d, want 5", got)
	}
	if got, _ := s.Total(ctx, "t1", "b1", EgressBytes); got != 100 {
		t.Fatalf("Total(t1,b1,egress) = %d, want 100", got)
	}

	// Tenant-wide aggregation across buckets.
	tot, err := s.TenantTotal(ctx, "t1", PutRequests)
	if err != nil {
		t.Fatalf("TenantTotal: %v", err)
	}
	if tot != 7 {
		t.Fatalf("TenantTotal(t1,put) = %d, want 7", tot)
	}

	// Unknown dimension is zero, not an error.
	if got, err := s.Total(ctx, "t1", "b1", GetRequests); err != nil || got != 0 {
		t.Fatalf("Total(unknown) = (%d, %v), want (0, nil)", got, err)
	}
}

func TestSQLiteSink_EmitAfterCloseIsNoop(t *testing.T) {
	t.Parallel()
	s := newTestSink(t)
	ctx := context.Background()
	s.Emit(UsageEvent{TenantID: "t1", Bucket: "b1", Dimension: PutRequests, Delta: 1})
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s.Emit(UsageEvent{TenantID: "t1", Bucket: "b1", Dimension: PutRequests, Delta: 99})
	if got, _ := s.Total(ctx, "t1", "b1", PutRequests); got != 1 {
		t.Fatalf("Total after close = %d, want 1 (post-close Emit dropped)", got)
	}
}

func TestSQLiteSink_PersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "billing.db")
	db, err := embeddeddb.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s, err := NewSQLiteSink(SQLiteSinkConfig{DB: db})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	s.Emit(UsageEvent{TenantID: "t1", Bucket: "b1", Dimension: StorageBytesSeconds, Delta: 4096, ObservedAt: time.Unix(1, 0)})
	_ = s.Close(context.Background())
	_ = db.Close()

	db2, err := embeddeddb.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	s2, err := NewSQLiteSink(SQLiteSinkConfig{DB: db2})
	if err != nil {
		t.Fatalf("new (reopen): %v", err)
	}
	if got, _ := s2.Total(context.Background(), "t1", "b1", StorageBytesSeconds); got != 4096 {
		t.Fatalf("Total after reopen = %d, want 4096", got)
	}
}
