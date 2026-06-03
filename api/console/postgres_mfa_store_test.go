package console

import (
	"testing"
)

// postgresMFAFactory builds a PostgresMFAStore against the DSN in
// TEST_POSTGRES_DSN (skipping when unset, like the AuthStore tests) and
// clears any rows from a prior run so the contract suite starts clean.
func postgresMFAFactory(t *testing.T) MFAStore {
	t.Helper()
	db := openTestPostgres(t)
	if _, err := db.Exec(`DELETE FROM mfa_recovery_codes`); err != nil {
		t.Fatalf("truncate mfa_recovery_codes: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM mfa_credentials`); err != nil {
		t.Fatalf("truncate mfa_credentials: %v", err)
	}
	s, err := NewPostgresMFAStore(db)
	if err != nil {
		t.Fatalf("NewPostgresMFAStore: %v", err)
	}
	return s
}

// TestPostgresMFAStoreContract runs the shared behavioural suite against
// the Postgres backend. It is skipped when no TEST_POSTGRES_DSN is
// configured so CI stays green without a Postgres dependency, matching
// TestPostgresRefreshTokenStoreContract.
func TestPostgresMFAStoreContract(t *testing.T) {
	runMFAStoreContract(t, postgresMFAFactory)
}
