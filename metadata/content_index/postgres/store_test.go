package postgres

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	_ "github.com/lib/pq"

	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
)

// requirePostgres returns an open *sql.DB or skips the test when
// METADATA_DSN is not configured. The schema is reset on each test
// so leftover rows from a previous run do not interfere.
func requirePostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("METADATA_DSN")
	if dsn == "" {
		t.Skip("METADATA_DSN not set; skipping postgres content_index tests")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS content_index_test`); err != nil {
		t.Fatalf("drop test table: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE content_index_test (
			tenant_id     TEXT        NOT NULL,
			content_hash  TEXT        NOT NULL,
			piece_id      TEXT        NOT NULL,
			backend       TEXT        NOT NULL,
			ref_count     INT         NOT NULL DEFAULT 1 CHECK (ref_count >= 0),
			size_bytes    BIGINT      NOT NULL DEFAULT 0,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (tenant_id, content_hash)
		)`); err != nil {
		t.Fatalf("create test table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TABLE IF EXISTS content_index_test`)
		_ = db.Close()
	})
	return db
}

func TestPostgresStore_FullLifecycle(t *testing.T) {
	db := requirePostgres(t)
	s, err := New(Config{DB: db, Table: "content_index_test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	if _, err := s.Lookup(ctx, "tnt", "h"); !errors.Is(err, content_index.ErrNotFound) {
		t.Fatalf("Lookup empty: got %v want ErrNotFound", err)
	}

	if err := s.Register(ctx, content_index.ContentIndexEntry{
		TenantID: "tnt", ContentHash: "h", PieceID: "p1", Backend: "wasabi", SizeBytes: 100,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Register(ctx, content_index.ContentIndexEntry{
		TenantID: "tnt", ContentHash: "h", PieceID: "p2", Backend: "wasabi",
	}); !errors.Is(err, content_index.ErrAlreadyExists) {
		t.Fatalf("Register dup: got %v want ErrAlreadyExists", err)
	}
	if err := s.IncrementRef(ctx, "tnt", "h"); err != nil {
		t.Fatalf("IncrementRef: %v", err)
	}
	n, err := s.DecrementRef(ctx, "tnt", "h")
	if err != nil {
		t.Fatalf("DecrementRef: %v", err)
	}
	if n != 1 {
		t.Fatalf("DecrementRef = %d want 1", n)
	}
	n, err = s.DecrementRef(ctx, "tnt", "h")
	if err != nil {
		t.Fatalf("DecrementRef: %v", err)
	}
	if n != 0 {
		t.Fatalf("DecrementRef final = %d want 0", n)
	}
	if _, err := s.DecrementRef(ctx, "tnt", "h"); !errors.Is(err, content_index.ErrInvalidRefCount) {
		t.Fatalf("DecrementRef below zero: got %v want ErrInvalidRefCount", err)
	}
	if err := s.Delete(ctx, "tnt", "h"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, "tnt", "h"); !errors.Is(err, content_index.ErrNotFound) {
		t.Fatalf("Delete missing: got %v want ErrNotFound", err)
	}
}

func TestPostgresStore_TenantIsolation(t *testing.T) {
	db := requirePostgres(t)
	s, err := New(Config{DB: db, Table: "content_index_test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := s.Register(ctx, content_index.ContentIndexEntry{
		TenantID: "a", ContentHash: "h", PieceID: "pa", Backend: "wasabi",
	}); err != nil {
		t.Fatalf("Register a: %v", err)
	}
	if err := s.Register(ctx, content_index.ContentIndexEntry{
		TenantID: "b", ContentHash: "h", PieceID: "pb", Backend: "wasabi",
	}); err != nil {
		t.Fatalf("Register b: %v", err)
	}
	a, err := s.Lookup(ctx, "a", "h")
	if err != nil {
		t.Fatalf("Lookup a: %v", err)
	}
	if a.PieceID != "pa" {
		t.Fatalf("tenant isolation broken: got pieceID %q", a.PieceID)
	}
	b, err := s.Lookup(ctx, "b", "h")
	if err != nil {
		t.Fatalf("Lookup b: %v", err)
	}
	if b.PieceID != "pb" {
		t.Fatalf("tenant isolation broken: got pieceID %q", b.PieceID)
	}
}
