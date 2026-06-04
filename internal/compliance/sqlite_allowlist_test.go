package compliance

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
)

func newTestSQLiteAllowlistStore(t *testing.T) *SQLiteAllowlistStore {
	t.Helper()
	db, err := embeddeddb.Open(filepath.Join(t.TempDir(), "allowlist.db"))
	if err != nil {
		t.Fatalf("open embedded db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := NewSQLiteAllowlistStore(db)
	if err != nil {
		t.Fatalf("new sqlite allowlist store: %v", err)
	}
	return s
}

func TestSQLiteAllowlistStore_ReplaceLookupRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteAllowlistStore(t)
	ctx := context.Background()

	// Unknown tenant → empty (allow-all), no error.
	got, err := s.Lookup("t1")
	if err != nil {
		t.Fatalf("Lookup(unknown): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Lookup(unknown) = %v, want empty", got)
	}

	// Replace normalizes (trim/upper) and de-duplicates; Lookup
	// returns the stable, country-sorted set.
	if err := s.Replace(ctx, "t1", []string{" us ", "de", "US", "fr"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err = s.Lookup("t1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if want := []string{"DE", "FR", "US"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup = %v, want %v", got, want)
	}

	// A second Replace fully supersedes the prior set (not a merge).
	if err := s.Replace(ctx, "t1", []string{"ca"}); err != nil {
		t.Fatalf("Replace (supersede): %v", err)
	}
	got, _ = s.Lookup("t1")
	if want := []string{"CA"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after supersede Lookup = %v, want %v", got, want)
	}

	// An empty/all-blank list clears the tenant → back to allow-all.
	if err := s.Replace(ctx, "t1", []string{"  ", ""}); err != nil {
		t.Fatalf("Replace (clear): %v", err)
	}
	got, _ = s.Lookup("t1")
	if len(got) != 0 {
		t.Fatalf("after clear Lookup = %v, want empty", got)
	}
}

func TestSQLiteAllowlistStore_TenantIsolation(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteAllowlistStore(t)
	ctx := context.Background()

	if err := s.Replace(ctx, "t1", []string{"US"}); err != nil {
		t.Fatalf("Replace t1: %v", err)
	}
	if err := s.Replace(ctx, "t2", []string{"DE"}); err != nil {
		t.Fatalf("Replace t2: %v", err)
	}
	// Replacing t1 must not touch t2's rows.
	if err := s.Replace(ctx, "t1", []string{"FR"}); err != nil {
		t.Fatalf("Replace t1 again: %v", err)
	}
	got1, _ := s.Lookup("t1")
	got2, _ := s.Lookup("t2")
	if want := []string{"FR"}; !reflect.DeepEqual(got1, want) {
		t.Fatalf("t1 = %v, want %v", got1, want)
	}
	if want := []string{"DE"}; !reflect.DeepEqual(got2, want) {
		t.Fatalf("t2 = %v, want %v", got2, want)
	}
}

func TestSQLiteAllowlistStore_ReplaceRequiresTenant(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteAllowlistStore(t)
	if err := s.Replace(context.Background(), "  ", []string{"US"}); err == nil {
		t.Fatal("Replace(empty tenant) = nil, want error")
	}
}

// TestSQLiteAllowlistStore_PersistsAcrossReopen pins the embedded-profile
// guarantee: a seeded allowlist must survive a gateway restart, unlike
// the nil (allow-all) lookup it replaces.
func TestSQLiteAllowlistStore_PersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "allowlist.db")

	withDB := func(fn func(db *sql.DB)) {
		db, err := embeddeddb.Open(path)
		if err != nil {
			t.Fatalf("open embedded db: %v", err)
		}
		defer func() { _ = db.Close() }()
		fn(db)
	}

	withDB(func(db *sql.DB) {
		s, err := NewSQLiteAllowlistStore(db)
		if err != nil {
			t.Fatalf("new store: %v", err)
		}
		if err := s.Replace(ctx, "t1", []string{"US", "DE"}); err != nil {
			t.Fatalf("Replace: %v", err)
		}
	})

	withDB(func(db *sql.DB) {
		s, err := NewSQLiteAllowlistStore(db) // re-runs ensureSchema (idempotent)
		if err != nil {
			t.Fatalf("reopen store: %v", err)
		}
		got, err := s.Lookup("t1")
		if err != nil {
			t.Fatalf("Lookup after reopen: %v", err)
		}
		if want := []string{"DE", "US"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("after reopen Lookup = %v, want %v", got, want)
		}
	})
}

// TestSQLiteAllowlistStore_EnforcerIntegration proves the store's
// Lookup drops straight into ResidencyEnforcer (it satisfies
// AllowlistLookup) and gates a backend country end-to-end.
func TestSQLiteAllowlistStore_EnforcerIntegration(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteAllowlistStore(t)
	ctx := context.Background()
	if err := s.Replace(ctx, "t1", []string{"US"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	enf := NewResidencyEnforcer(s.Lookup)

	if err := enf.Check("t1", "US", nil); err != nil {
		t.Errorf("Check(US) = %v, want nil (permitted)", err)
	}
	if err := enf.Check("t1", "DE", nil); err != ErrResidencyViolation {
		t.Errorf("Check(DE) = %v, want ErrResidencyViolation", err)
	}
	// Tenant with no rows → allow-all.
	if err := enf.Check("t2", "DE", nil); err != nil {
		t.Errorf("Check(unseeded tenant) = %v, want nil (allow-all)", err)
	}
}
