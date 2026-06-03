package auth

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
)

func newTestSQLiteLegalHoldStore(t *testing.T) *SQLiteLegalHoldStore {
	t.Helper()
	db, err := embeddeddb.Open(filepath.Join(t.TempDir(), "lh.db"))
	if err != nil {
		t.Fatalf("open embedded db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := NewSQLiteLegalHoldStore(db)
	if err != nil {
		t.Fatalf("new sqlite legal hold store: %v", err)
	}
	return s
}

func sampleHold(id, tenant string) LegalHold {
	return LegalHold{
		ID:        id,
		TenantID:  tenant,
		Bucket:    "b1",
		ObjectKey: "k1",
		Reason:    "litigation hold",
		CaseID:    "CASE-1",
		IssuedBy:  "legal@example.com",
	}
}

func TestSQLiteLegalHoldStore_CreateGetRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteLegalHoldStore(t)
	ctx := context.Background()

	want := sampleHold("h1", "t1")
	want.ExpiresAt = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Create(ctx, want); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "h1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != want.ID || got.TenantID != want.TenantID || got.Bucket != want.Bucket ||
		got.ObjectKey != want.ObjectKey || got.Reason != want.Reason || got.CaseID != want.CaseID ||
		got.IssuedBy != want.IssuedBy || got.Released {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should be stamped from the clock when zero")
	}
}

func TestSQLiteLegalHoldStore_GetNotFound(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteLegalHoldStore(t)
	_, err := s.Get(context.Background(), "missing")
	if !errors.Is(err, ErrLegalHoldNotFound) {
		t.Fatalf("Get(missing) err = %v, want ErrLegalHoldNotFound", err)
	}
}

func TestSQLiteLegalHoldStore_CreateRejectsDuplicateAndInvalid(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteLegalHoldStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, sampleHold("dup", "t1")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := s.Create(ctx, sampleHold("dup", "t1")); err == nil {
		t.Fatal("duplicate Create = nil error, want PK violation")
	}
	if err := s.Create(ctx, LegalHold{ID: "", TenantID: "t1", Reason: "r", IssuedBy: "x"}); err == nil {
		t.Fatal("Create with empty id = nil error, want validation error")
	}
	if err := s.Create(ctx, LegalHold{ID: "x", TenantID: "t1"}); err == nil {
		t.Fatal("Create without reason/issued_by = nil error, want validation error")
	}
}

func TestSQLiteLegalHoldStore_ReleaseMarksReleasedAndIsIdempotentlyGuarded(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteLegalHoldStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, sampleHold("h1", "t1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Release(ctx, "h1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got, err := s.Get(ctx, "h1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Released || got.ReleasedAt.IsZero() {
		t.Fatalf("after Release: Released=%v ReleasedAt=%v, want released with timestamp", got.Released, got.ReleasedAt)
	}
	// A second release affects no rows (already released).
	if err := s.Release(ctx, "h1"); !errors.Is(err, ErrLegalHoldNotFound) {
		t.Fatalf("second Release err = %v, want ErrLegalHoldNotFound", err)
	}
	// Releasing an unknown id also reports not found.
	if err := s.Release(ctx, "nope"); !errors.Is(err, ErrLegalHoldNotFound) {
		t.Fatalf("Release(unknown) err = %v, want ErrLegalHoldNotFound", err)
	}
}

func TestSQLiteLegalHoldStore_ListByTenantDescending(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteLegalHoldStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	mk := func(id string, created time.Time) LegalHold {
		h := sampleHold(id, "t1")
		h.CreatedAt = created
		return h
	}
	if err := s.Create(ctx, mk("old", base)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create(ctx, mk("new", base.Add(time.Hour))); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create(ctx, sampleHold("other", "t2")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.List(ctx, "t1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].ID != "new" || got[1].ID != "old" {
		t.Fatalf("List = %+v, want [new, old] (created_at DESC, tenant-scoped)", got)
	}
}

func TestSQLiteLegalHoldStore_ActiveScopingAndExpiryAndRelease(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteLegalHoldStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Tenant-wide hold (empty bucket/key) matches any object.
	tenantWide := LegalHold{ID: "tw", TenantID: "t1", Reason: "r", IssuedBy: "x"}
	// Bucket-scoped hold matches only b1.
	bucketScoped := LegalHold{ID: "bs", TenantID: "t1", Bucket: "b1", Reason: "r", IssuedBy: "x"}
	// Expired hold must not be returned.
	expired := LegalHold{ID: "ex", TenantID: "t1", Reason: "r", IssuedBy: "x", ExpiresAt: now.Add(-time.Hour)}
	// Released hold must not be returned.
	released := LegalHold{ID: "rel", TenantID: "t1", Reason: "r", IssuedBy: "x"}
	for _, h := range []LegalHold{tenantWide, bucketScoped, expired, released} {
		if err := s.Create(ctx, h); err != nil {
			t.Fatalf("Create %s: %v", h.ID, err)
		}
	}
	if err := s.Release(ctx, "rel"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// (t1, b1, k1): tenant-wide + bucket-scoped apply; expired/released excluded.
	active, err := s.Active(ctx, "t1", "b1", "k1")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	ids := map[string]bool{}
	for _, h := range active {
		ids[h.ID] = true
	}
	if len(active) != 2 || !ids["tw"] || !ids["bs"] {
		t.Fatalf("Active(t1,b1,k1) = %v, want {tw, bs}", ids)
	}

	// (t1, b2, k1): only the tenant-wide hold applies.
	active, err = s.Active(ctx, "t1", "b2", "k1")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(active) != 1 || active[0].ID != "tw" {
		t.Fatalf("Active(t1,b2,k1) = %+v, want only tw", active)
	}

	// Different tenant sees nothing.
	active, err = s.Active(ctx, "t2", "b1", "k1")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("Active(t2,...) = %+v, want none", active)
	}
}

// TestSQLiteLegalHoldStore_PersistsAcrossReopen is the core motivation:
// the embedded profile must keep active holds across a gateway restart,
// unlike the in-memory fallback it replaces — otherwise a restart would
// silently drop a WORM/legal-hold guarantee.
func TestSQLiteLegalHoldStore_PersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lh.db")

	withDB := func(fn func(db *sql.DB)) {
		db, err := embeddeddb.Open(path)
		if err != nil {
			t.Fatalf("open embedded db: %v", err)
		}
		defer func() { _ = db.Close() }()
		fn(db)
	}

	withDB(func(db *sql.DB) {
		s, err := NewSQLiteLegalHoldStore(db)
		if err != nil {
			t.Fatalf("new store: %v", err)
		}
		if err := s.Create(ctx, sampleHold("h1", "t1")); err != nil {
			t.Fatalf("Create: %v", err)
		}
	})

	withDB(func(db *sql.DB) {
		s, err := NewSQLiteLegalHoldStore(db) // re-runs ensureSchema (idempotent)
		if err != nil {
			t.Fatalf("reopen store: %v", err)
		}
		active, err := s.Active(ctx, "t1", "b1", "k1")
		if err != nil {
			t.Fatalf("Active after reopen: %v", err)
		}
		if len(active) != 1 || active[0].ID != "h1" {
			t.Fatalf("after reopen Active = %+v, want the persisted hold h1", active)
		}
	})
}

func TestSQLiteLegalHoldStore_ConcurrentCreate(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteLegalHoldStore(t)
	ctx := context.Background()
	const n = 50

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := sampleHold(string(rune('A'+i%26))+string(rune('0'+i/26)), "t1")
			if err := s.Create(ctx, h); err != nil {
				t.Errorf("concurrent Create: %v", err)
			}
		}(i)
	}
	wg.Wait()

	got, err := s.List(ctx, "t1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != n {
		t.Fatalf("after %d concurrent creates, List returned %d", n, len(got))
	}
}

func TestNewSQLiteLegalHoldStore_NilDB(t *testing.T) {
	t.Parallel()
	if _, err := NewSQLiteLegalHoldStore(nil); err == nil {
		t.Fatal("NewSQLiteLegalHoldStore(nil) = nil error, want error")
	}
}
