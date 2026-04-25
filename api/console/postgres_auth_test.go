package console

import (
	"database/sql"
	"errors"
	"os"
	"testing"
)

// openTestPostgres opens a *sql.DB against the dsn supplied via
// the TEST_POSTGRES_DSN environment variable and applies
// schema.sql. The test is skipped — not failed — when no DSN is
// configured so CI stays green without a Postgres dependency.
func openTestPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping Postgres-backed AuthStore tests")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("sql.Open(postgres): %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("ping postgres: %v", err)
	}
	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPostgresAuthStore_RoundTrip(t *testing.T) {
	db := openTestPostgres(t)
	store, err := NewPostgresAuthStore(db)
	if err != nil {
		t.Fatalf("NewPostgresAuthStore: %v", err)
	}
	const tenantID = "t-pg-1"
	const email = "pg-roundtrip@example.com"

	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM auth_users WHERE tenant_id = $1`, tenantID) })

	if err := store.CreateUser(email, "hash", tenantID); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.CreateUser(email, "hash", tenantID); err == nil {
		t.Fatalf("expected duplicate-email error, got nil")
	}
	hash, gotTenant, ok := store.LookupUser(email)
	if !ok || hash != "hash" || gotTenant != tenantID {
		t.Fatalf("LookupUser = (%q, %q, %v)", hash, gotTenant, ok)
	}
	verified, tracked := store.IsVerified(tenantID)
	if !tracked || verified {
		t.Fatalf("IsVerified = (%v, %v); want (false, true)", verified, tracked)
	}
	if err := store.SetVerificationToken(tenantID, "token-abc"); err != nil {
		t.Fatalf("SetVerificationToken: %v", err)
	}
	got, err := store.ConsumeVerificationToken("token-abc")
	if err != nil {
		t.Fatalf("ConsumeVerificationToken: %v", err)
	}
	if got != tenantID {
		t.Fatalf("ConsumeVerificationToken tenant = %q want %q", got, tenantID)
	}
	verified, tracked = store.IsVerified(tenantID)
	if !verified || !tracked {
		t.Fatalf("IsVerified after consume = (%v, %v); want (true, true)", verified, tracked)
	}
	if _, err := store.ConsumeVerificationToken("token-abc"); err == nil {
		t.Fatalf("expected error replaying consumed token")
	}
	if err := store.DeleteUser(email); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, _, ok := store.LookupUser(email); ok {
		t.Fatalf("LookupUser still returns row after DeleteUser")
	}
	// Idempotent delete.
	if err := store.DeleteUser(email); err != nil {
		t.Fatalf("idempotent DeleteUser: %v", err)
	}
}

func TestPostgresAuthStore_NilDB(t *testing.T) {
	if _, err := NewPostgresAuthStore(nil); err == nil {
		t.Fatalf("expected error from NewPostgresAuthStore(nil)")
	}
}

func TestPostgresAuthStore_ConsumeMissingToken(t *testing.T) {
	db := openTestPostgres(t)
	store, err := NewPostgresAuthStore(db)
	if err != nil {
		t.Fatalf("NewPostgresAuthStore: %v", err)
	}
	if _, err := store.ConsumeVerificationToken("nope"); err == nil {
		t.Fatalf("expected error for unknown token")
	}
	if _, err := store.ConsumeVerificationToken(""); err == nil {
		t.Fatalf("expected error for empty token")
	}
}

func TestIsUniqueViolationDetection(t *testing.T) {
	if !isUniqueViolation(errors.New("pq: duplicate key value violates unique constraint \"auth_users_pkey\" (SQLSTATE 23505)")) {
		t.Fatalf("did not detect 23505")
	}
	if isUniqueViolation(errors.New("connection refused")) {
		t.Fatalf("false positive on non-unique error")
	}
}
