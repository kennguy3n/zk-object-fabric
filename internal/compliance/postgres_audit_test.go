package compliance

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// requirePostgresAudit opens a *sql.DB against METADATA_DSN and
// bootstraps the compliance_audit table. Skips when the env var is
// not configured.
func requirePostgresAudit(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("METADATA_DSN")
	if dsn == "" {
		t.Skip("METADATA_DSN not set; skipping Postgres audit tests")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS compliance_audit`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE compliance_audit (
			tenant_id       TEXT        NOT NULL,
			operation       TEXT        NOT NULL,
			bucket          TEXT        NOT NULL DEFAULT '',
			object_key      TEXT        NOT NULL DEFAULT '',
			piece_id        TEXT        NOT NULL DEFAULT '',
			piece_backend   TEXT        NOT NULL DEFAULT '',
			backend_country TEXT        NOT NULL DEFAULT '',
			request_id      TEXT        NOT NULL DEFAULT '',
			recorded_at     TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TABLE IF EXISTS compliance_audit`)
		_ = db.Close()
	})
	return db
}

func TestPostgresAuditStore_ZeroTimeRange(t *testing.T) {
	db := requirePostgresAudit(t)
	s := NewPostgresAuditStore(db)
	ctx := context.Background()
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	for i, op := range []string{"PUT", "GET", "DELETE"} {
		if err := s.Record(ctx, AuditEntry{
			TenantID:  "tenant-A",
			Operation: op,
			Bucket:    "b",
			ObjectKey: "k",
			Timestamp: t0.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatalf("record %s: %v", op, err)
		}
	}
	// Foreign tenant row.
	_ = s.Record(ctx, AuditEntry{TenantID: "tenant-B", Operation: "PUT", Timestamp: t0})

	// Zero TimeRange should return all entries for tenant-A,
	// matching MemoryAuditStore behaviour.
	got, err := s.Query(ctx, "tenant-A", TimeRange{})
	if err != nil {
		t.Fatalf("query zero: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("zero range: got %d rows, want 3", len(got))
	}

	// Start-only: should return entries at or after t0 + 1h.
	got, err = s.Query(ctx, "tenant-A", TimeRange{Start: t0.Add(time.Hour)})
	if err != nil {
		t.Fatalf("query start-only: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("start-only: got %d rows, want 2", len(got))
	}

	// End-only: should return entries at or before t0 + 1h.
	got, err = s.Query(ctx, "tenant-A", TimeRange{End: t0.Add(time.Hour)})
	if err != nil {
		t.Fatalf("query end-only: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("end-only: got %d rows, want 2", len(got))
	}

	// Both bounds: should return the middle entry only.
	got, err = s.Query(ctx, "tenant-A", TimeRange{
		Start: t0.Add(30 * time.Minute),
		End:   t0.Add(90 * time.Minute),
	})
	if err != nil {
		t.Fatalf("query both: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("both bounds: got %d rows, want 1", len(got))
	}
}
