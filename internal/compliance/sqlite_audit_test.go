package compliance

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
)

func newTestSQLiteAuditStore(t *testing.T) *SQLiteAuditStore {
	t.Helper()
	db, err := embeddeddb.Open(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("open embedded db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := NewSQLiteAuditStore(db)
	if err != nil {
		t.Fatalf("new sqlite audit store: %v", err)
	}
	return s
}

func TestSQLiteAuditStore_RecordQueryRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteAuditStore(t)
	ctx := context.Background()

	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	want := AuditEntry{
		TenantID:       "t1",
		Operation:      "LifecycleExpiration",
		Bucket:         "b1",
		ObjectKey:      "k1",
		PieceID:        "p1",
		PieceBackend:   "backend-eu",
		BackendCountry: "DE",
		Timestamp:      ts,
		RequestID:      "req-1",
	}
	if err := s.Record(ctx, want); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := s.Query(ctx, "t1", TimeRange{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Query returned %d entries, want 1", len(got))
	}
	if !got[0].Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v, want %v", got[0].Timestamp, ts)
	}
	// Compare the rest field-by-field with a normalized timestamp.
	g := got[0]
	g.Timestamp = ts
	if g != want {
		t.Errorf("round-trip entry = %+v, want %+v", g, want)
	}
}

func TestSQLiteAuditStore_QueryNeverNil(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteAuditStore(t)
	got, err := s.Query(context.Background(), "no-such-tenant", TimeRange{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got == nil {
		t.Fatal("Query returned nil slice, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("Query returned %d entries, want 0", len(got))
	}
}

func TestSQLiteAuditStore_TenantIsolationAndOrdering(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteAuditStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// Insert out of order; expect ascending output for t1 only.
	for _, e := range []AuditEntry{
		{TenantID: "t1", Operation: "C", Bucket: "b", Timestamp: base.Add(2 * time.Hour)},
		{TenantID: "t2", Operation: "X", Bucket: "b", Timestamp: base.Add(1 * time.Hour)},
		{TenantID: "t1", Operation: "A", Bucket: "b", Timestamp: base},
		{TenantID: "t1", Operation: "B", Bucket: "b", Timestamp: base.Add(1 * time.Hour)},
	} {
		if err := s.Record(ctx, e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	got, err := s.Query(ctx, "t1", TimeRange{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	gotOps := []string{}
	for _, e := range got {
		if e.TenantID != "t1" {
			t.Fatalf("got entry for tenant %q, want only t1", e.TenantID)
		}
		gotOps = append(gotOps, e.Operation)
	}
	want := []string{"A", "B", "C"}
	if len(gotOps) != len(want) {
		t.Fatalf("ops = %v, want %v", gotOps, want)
	}
	for i := range want {
		if gotOps[i] != want[i] {
			t.Fatalf("ops = %v, want %v (ascending by timestamp)", gotOps, want)
		}
	}
}

func TestSQLiteAuditStore_TimeRangeBoundsInclusive(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteAuditStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	times := []time.Time{base, base.Add(time.Hour), base.Add(2 * time.Hour), base.Add(3 * time.Hour)}
	for i, ts := range times {
		if err := s.Record(ctx, AuditEntry{TenantID: "t1", Operation: string(rune('A' + i)), Bucket: "b", Timestamp: ts}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	// Closed interval [base+1h, base+2h] should include exactly B and C.
	got, err := s.Query(ctx, "t1", TimeRange{Start: times[1], End: times[2]})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 || got[0].Operation != "B" || got[1].Operation != "C" {
		t.Fatalf("range query = %+v, want B,C (inclusive bounds)", got)
	}

	// Open-ended start: everything <= base+1h.
	got, err = s.Query(ctx, "t1", TimeRange{End: times[1]})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 || got[0].Operation != "A" || got[1].Operation != "B" {
		t.Fatalf("end-only query = %+v, want A,B", got)
	}
}

// TestSQLiteAuditStore_PersistsAcrossReopen is the core motivation for
// this store: the embedded profile must keep the audit trail across a
// gateway restart, unlike the in-memory fallback it replaces.
func TestSQLiteAuditStore_PersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.db")

	withDB := func(fn func(db *sql.DB)) {
		db, err := embeddeddb.Open(path)
		if err != nil {
			t.Fatalf("open embedded db: %v", err)
		}
		defer func() { _ = db.Close() }()
		fn(db)
	}

	withDB(func(db *sql.DB) {
		s, err := NewSQLiteAuditStore(db)
		if err != nil {
			t.Fatalf("new store: %v", err)
		}
		if err := s.Record(ctx, AuditEntry{TenantID: "t1", Operation: "LifecycleExpiration", Bucket: "b", ObjectKey: "k", Timestamp: time.Now().UTC()}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	})

	withDB(func(db *sql.DB) {
		s, err := NewSQLiteAuditStore(db) // re-runs ensureSchema (idempotent)
		if err != nil {
			t.Fatalf("reopen store: %v", err)
		}
		got, err := s.Query(ctx, "t1", TimeRange{})
		if err != nil {
			t.Fatalf("Query after reopen: %v", err)
		}
		if len(got) != 1 || got[0].Operation != "LifecycleExpiration" {
			t.Fatalf("after reopen got %+v, want the one persisted entry", got)
		}
	})
}

func TestSQLiteAuditStore_ConcurrentRecord(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteAuditStore(t)
	ctx := context.Background()
	const n = 50

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.Record(ctx, AuditEntry{TenantID: "t1", Operation: "Op", Bucket: "b", Timestamp: time.Now().UTC()})
			if err != nil {
				t.Errorf("concurrent Record: %v", err)
			}
		}(i)
	}
	wg.Wait()

	got, err := s.Query(ctx, "t1", TimeRange{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != n {
		t.Fatalf("after %d concurrent records, Query returned %d", n, len(got))
	}
}

func TestNewSQLiteAuditStore_NilDB(t *testing.T) {
	t.Parallel()
	if _, err := NewSQLiteAuditStore(nil); err == nil {
		t.Fatal("NewSQLiteAuditStore(nil) = nil error, want error")
	}
}
