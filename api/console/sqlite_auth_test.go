package console

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
)

func newSQLiteAuthStore(t *testing.T) *SQLiteAuthStore {
	t.Helper()
	db, err := embeddeddb.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open embedded db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := NewSQLiteAuthStore(db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func TestSQLiteAuth_CreateLookup(t *testing.T) {
	t.Parallel()
	s := newSQLiteAuthStore(t)

	if err := s.CreateUser("User@Example.com", "hash1", "tnt-1"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Email lookup is case-insensitive.
	hash, tenant, ok := s.LookupUser("user@example.com")
	if !ok || hash != "hash1" || tenant != "tnt-1" {
		t.Fatalf("LookupUser = (%q, %q, %v), want (hash1, tnt-1, true)", hash, tenant, ok)
	}

	// Duplicate email (any case) is rejected and classified as a
	// unique violation (PRIMARY KEY, extended code 1555) rather than
	// surfacing the raw driver error — this exercises the structured
	// isSQLiteUniqueViolation path.
	err := s.CreateUser("USER@EXAMPLE.COM", "hash2", "tnt-2")
	if err == nil {
		t.Fatal("duplicate CreateUser: want error")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate CreateUser error = %q; want it classified as 'already registered'", err.Error())
	}

	// Missing user.
	if _, _, ok := s.LookupUser("nobody@example.com"); ok {
		t.Fatal("LookupUser(missing): want ok=false")
	}
}

func TestSQLiteAuth_VerificationFlow(t *testing.T) {
	t.Parallel()
	s := newSQLiteAuthStore(t)

	if err := s.CreateUser("a@b.com", "h", "tnt-1"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	verified, tracked := s.IsVerified("tnt-1")
	if verified || !tracked {
		t.Fatalf("IsVerified initial = (%v, %v), want (false, true)", verified, tracked)
	}
	// Unknown tenant is untracked.
	if _, tracked := s.IsVerified("tnt-unknown"); tracked {
		t.Fatal("IsVerified(unknown): want tracked=false")
	}

	if err := s.SetVerificationToken("tnt-1", "tok-123"); err != nil {
		t.Fatalf("SetVerificationToken: %v", err)
	}
	// Wrong token is rejected.
	if _, err := s.ConsumeVerificationToken("wrong"); err == nil {
		t.Fatal("ConsumeVerificationToken(wrong): want error")
	}
	tenant, err := s.ConsumeVerificationToken("tok-123")
	if err != nil {
		t.Fatalf("ConsumeVerificationToken: %v", err)
	}
	if tenant != "tnt-1" {
		t.Fatalf("ConsumeVerificationToken = %q, want tnt-1", tenant)
	}
	if verified, _ := s.IsVerified("tnt-1"); !verified {
		t.Fatal("IsVerified after consume: want true")
	}
	// Token cannot be replayed.
	if _, err := s.ConsumeVerificationToken("tok-123"); err == nil {
		t.Fatal("ConsumeVerificationToken replay: want error")
	}
}

func TestSQLiteAuth_DeleteUserIdempotent(t *testing.T) {
	t.Parallel()
	s := newSQLiteAuthStore(t)
	if err := s.CreateUser("a@b.com", "h", "tnt-1"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.DeleteUser("a@b.com"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, _, ok := s.LookupUser("a@b.com"); ok {
		t.Fatal("LookupUser after delete: want ok=false")
	}
	// Deleting a missing email is a no-op.
	if err := s.DeleteUser("a@b.com"); err != nil {
		t.Fatalf("DeleteUser (missing): %v", err)
	}
}

func TestSQLiteAuth_MarkVerifiedUnknownTenant(t *testing.T) {
	t.Parallel()
	s := newSQLiteAuthStore(t)
	if err := s.MarkVerified("tnt-unknown"); err == nil {
		t.Fatal("MarkVerified(unknown): want error")
	}
}

func TestSQLiteAuth_PersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "auth.db")
	db, err := embeddeddb.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s, err := NewSQLiteAuthStore(db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.CreateUser("a@b.com", "h", "tnt-1"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_ = db.Close()

	db2, err := embeddeddb.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	s2, err := NewSQLiteAuthStore(db2)
	if err != nil {
		t.Fatalf("new (reopen): %v", err)
	}
	if _, _, ok := s2.LookupUser("a@b.com"); !ok {
		t.Fatal("user lost across reopen")
	}
}
