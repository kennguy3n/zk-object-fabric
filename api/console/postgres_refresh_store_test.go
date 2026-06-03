package console

import (
	"testing"
)

// postgresRefreshFactory builds a PostgresRefreshTokenStore against the
// DSN in TEST_POSTGRES_DSN (skipping when unset, like the AuthStore
// tests) and clears any rows from a prior run so the contract suite
// starts clean.
func postgresRefreshFactory(t *testing.T, cfg RefreshConfig) RefreshTokenStore {
	t.Helper()
	db := openTestPostgres(t)
	if _, err := db.Exec(`DELETE FROM refresh_tokens`); err != nil {
		t.Fatalf("truncate refresh_tokens: %v", err)
	}
	s, err := NewPostgresRefreshTokenStore(db, cfg)
	if err != nil {
		t.Fatalf("NewPostgresRefreshTokenStore: %v", err)
	}
	return s
}

// TestPostgresRefreshTokenStoreContract runs the shared behavioural
// suite against the Postgres backend. It is skipped when no
// TEST_POSTGRES_DSN is configured so CI stays green without a Postgres
// dependency, matching TestPostgresAuthStore_RoundTrip.
func TestPostgresRefreshTokenStoreContract(t *testing.T) {
	runRefreshTokenStoreContract(t, postgresRefreshFactory)
}
