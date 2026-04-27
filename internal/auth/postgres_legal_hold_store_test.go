package auth

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// requirePostgresLegalHold opens a *sql.DB against METADATA_DSN and
// bootstraps the legal_holds table. Skips when the env var is not
// configured.
func requirePostgresLegalHold(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("METADATA_DSN")
	if dsn == "" {
		t.Skip("METADATA_DSN not set; skipping Postgres legal hold tests")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS legal_holds`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE legal_holds (
			id          TEXT PRIMARY KEY,
			tenant_id   TEXT NOT NULL,
			bucket      TEXT NOT NULL DEFAULT '',
			object_key  TEXT NOT NULL DEFAULT '',
			reason      TEXT NOT NULL,
			case_id     TEXT NOT NULL DEFAULT '',
			issued_by   TEXT NOT NULL,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			expires_at  TIMESTAMPTZ,
			released    BOOLEAN NOT NULL DEFAULT FALSE,
			released_at TIMESTAMPTZ
		)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TABLE IF EXISTS legal_holds`)
		_ = db.Close()
	})
	return db
}

func TestPostgresLegalHoldStore_CreateGetReleaseList(t *testing.T) {
	db := requirePostgresLegalHold(t)
	s, err := NewPostgresLegalHoldStore(db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	hold := LegalHold{
		ID:        "h-1",
		TenantID:  "T",
		Bucket:    "b",
		ObjectKey: "k",
		Reason:    "case-123",
		IssuedBy:  "ops",
	}
	if err := s.Create(ctx, hold); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Create(ctx, hold); err == nil {
		t.Error("duplicate id must error")
	}

	got, err := s.Get(ctx, "h-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TenantID != "T" || got.Bucket != "b" || got.ObjectKey != "k" {
		t.Errorf("get returned wrong record: %+v", got)
	}

	if _, err := s.Get(ctx, "missing"); !errors.Is(err, ErrLegalHoldNotFound) {
		t.Errorf("missing get = %v, want ErrLegalHoldNotFound", err)
	}

	listed, err := s.List(ctx, "T")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %v, err=%v", listed, err)
	}

	if err := s.Release(ctx, "h-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	got, _ = s.Get(ctx, "h-1")
	if !got.Released || got.ReleasedAt.IsZero() {
		t.Errorf("post-release record missing flags: %+v", got)
	}
	if err := s.Release(ctx, "h-1"); !errors.Is(err, ErrLegalHoldNotFound) {
		t.Errorf("re-release = %v, want ErrLegalHoldNotFound", err)
	}

	active, err := s.Active(ctx, "T", "b", "k")
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("released hold must be inactive, got %v", active)
	}
}

func TestPostgresLegalHoldStore_ActiveScopes(t *testing.T) {
	db := requirePostgresLegalHold(t)
	s, err := NewPostgresLegalHoldStore(db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	holds := []LegalHold{
		{ID: "tenant-wide", TenantID: "T", Reason: "r", IssuedBy: "ops"},
		{ID: "bucket-scoped", TenantID: "T", Bucket: "b", Reason: "r", IssuedBy: "ops"},
		{ID: "object-scoped", TenantID: "T", Bucket: "b", ObjectKey: "k", Reason: "r", IssuedBy: "ops"},
		{ID: "expired", TenantID: "T", Reason: "r", IssuedBy: "ops", ExpiresAt: now.Add(-time.Hour)},
		{ID: "other-tenant", TenantID: "U", Reason: "r", IssuedBy: "ops"},
	}
	for _, h := range holds {
		if err := s.Create(ctx, h); err != nil {
			t.Fatalf("create %s: %v", h.ID, err)
		}
	}

	got, err := s.Active(ctx, "T", "b", "k")
	if err != nil {
		t.Fatalf("active(T,b,k): %v", err)
	}
	ids := map[string]bool{}
	for _, h := range got {
		ids[h.ID] = true
	}
	for _, want := range []string{"tenant-wide", "bucket-scoped", "object-scoped"} {
		if !ids[want] {
			t.Errorf("active missing %s", want)
		}
	}
	if ids["expired"] || ids["other-tenant"] {
		t.Errorf("active leaked unscoped/expired holds: %v", ids)
	}

	// Object-scoped hold must not match a different key.
	got, err = s.Active(ctx, "T", "b", "other")
	if err != nil {
		t.Fatalf("active(T,b,other): %v", err)
	}
	for _, h := range got {
		if h.ID == "object-scoped" {
			t.Error("object-scoped hold leaked into other key")
		}
	}

	// Bucket-scoped hold must not match a different bucket; the
	// tenant-wide hold still does.
	got, err = s.Active(ctx, "T", "other", "k")
	if err != nil {
		t.Fatalf("active(T,other,k): %v", err)
	}
	for _, h := range got {
		if h.ID == "bucket-scoped" || h.ID == "object-scoped" {
			t.Errorf("bucket-/object-scoped hold leaked: %s", h.ID)
		}
	}
}

func TestPostgresLegalHoldStore_RejectsInvalidInput(t *testing.T) {
	// Validation runs before any DB call; a nil DSN test is fine.
	s := &PostgresLegalHoldStore{db: nil, clock: time.Now}
	ctx := context.Background()
	if err := s.Create(ctx, LegalHold{TenantID: "T", Reason: "r", IssuedBy: "ops"}); err == nil {
		t.Error("missing id must error")
	}
	if err := s.Create(ctx, LegalHold{ID: "h", Reason: "r", IssuedBy: "ops"}); err == nil {
		t.Error("missing tenant_id must error")
	}
	if err := s.Create(ctx, LegalHold{ID: "h", TenantID: "T", IssuedBy: "ops"}); err == nil {
		t.Error("missing reason must error")
	}
	if err := s.Create(ctx, LegalHold{ID: "h", TenantID: "T", Reason: "r"}); err == nil {
		t.Error("missing issued_by must error")
	}
}

func TestNewPostgresLegalHoldStore_RejectsNilDB(t *testing.T) {
	if _, err := NewPostgresLegalHoldStore(nil); err == nil {
		t.Error("nil db must error")
	}
}
