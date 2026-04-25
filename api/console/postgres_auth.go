package console

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// PostgresAuthStore is the Phase 3 Postgres-backed AuthStore for
// the B2C self-service signup / login flow. It mirrors the contract
// of MemoryAuthStore (single row per email; tenant_id, password
// hash, verified flag, optional verification token) but persists
// the rows in the auth_users table defined in
// api/console/schema.sql.
//
// The store satisfies AuthStore in api/console/auth_handler.go.
type PostgresAuthStore struct {
	db  *sql.DB
	ctx context.Context
}

// NewPostgresAuthStore wraps db. Callers are responsible for
// opening the *sql.DB with a registered driver (lib/pq or
// jackc/pgx/v5/stdlib) and for running the migration in
// schema.sql before issuing the first query.
func NewPostgresAuthStore(db *sql.DB) (*PostgresAuthStore, error) {
	if db == nil {
		return nil, errors.New("console: postgres auth store requires a non-nil *sql.DB")
	}
	return &PostgresAuthStore{db: db}, nil
}

// WithContext returns a copy of the store bound to ctx. The
// returned store reuses the underlying *sql.DB so concurrent
// callers can safely each derive their own context-bound view.
func (s *PostgresAuthStore) WithContext(ctx context.Context) *PostgresAuthStore {
	clone := *s
	clone.ctx = ctx
	return &clone
}

func (s *PostgresAuthStore) cx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// CreateUser implements AuthStore.
func (s *PostgresAuthStore) CreateUser(email, passwordHash, tenantID string) error {
	if email == "" {
		return errors.New("console: email is required")
	}
	if passwordHash == "" {
		return errors.New("console: password hash is required")
	}
	if tenantID == "" {
		return errors.New("console: tenant_id is required")
	}
	const q = `
		INSERT INTO auth_users (email, password_hash, tenant_id, verified, verify_token)
		VALUES ($1, $2, $3, FALSE, NULL)`
	_, err := s.db.ExecContext(s.cx(), q, normalizeEmail(email), passwordHash, tenantID)
	if err != nil {
		// The email PRIMARY KEY constraint surfaces as a
		// unique-violation; surface a stable error so the
		// signup handler returns 409 instead of 500.
		if isUniqueViolation(err) {
			return fmt.Errorf("console: email %q is already registered", email)
		}
		return fmt.Errorf("console: insert auth user: %w", err)
	}
	return nil
}

// LookupUser implements AuthStore.
func (s *PostgresAuthStore) LookupUser(email string) (string, string, bool) {
	if email == "" {
		return "", "", false
	}
	const q = `SELECT password_hash, tenant_id FROM auth_users WHERE email = $1`
	var hash, tenantID string
	err := s.db.QueryRowContext(s.cx(), q, normalizeEmail(email)).Scan(&hash, &tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false
	}
	if err != nil {
		return "", "", false
	}
	return hash, tenantID, true
}

// DeleteUser implements AuthStore. Idempotent: a missing email is
// not an error so signup can clean up half-finished rows without
// special-casing the not-found path.
func (s *PostgresAuthStore) DeleteUser(email string) error {
	if email == "" {
		return errors.New("console: email is required")
	}
	const q = `DELETE FROM auth_users WHERE email = $1`
	if _, err := s.db.ExecContext(s.cx(), q, normalizeEmail(email)); err != nil {
		return fmt.Errorf("console: delete auth user: %w", err)
	}
	return nil
}

// IsVerified implements AuthStore. Returns (verified, true) when a
// row exists for tenantID, (false, false) otherwise. The latter
// signals "out of scope" so the S3 VerifiedCheck gate lets the
// request through for tenants that never signed up via the
// console (e.g. HMAC-only bindings loaded from JSON).
func (s *PostgresAuthStore) IsVerified(tenantID string) (bool, bool) {
	if tenantID == "" {
		return false, false
	}
	const q = `SELECT verified FROM auth_users WHERE tenant_id = $1 LIMIT 1`
	var verified bool
	err := s.db.QueryRowContext(s.cx(), q, tenantID).Scan(&verified)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false
	}
	if err != nil {
		return false, false
	}
	return verified, true
}

// MarkVerified implements AuthStore.
func (s *PostgresAuthStore) MarkVerified(tenantID string) error {
	if tenantID == "" {
		return errors.New("console: tenant_id is required")
	}
	const q = `UPDATE auth_users SET verified = TRUE, verify_token = NULL WHERE tenant_id = $1`
	res, err := s.db.ExecContext(s.cx(), q, tenantID)
	if err != nil {
		return fmt.Errorf("console: mark verified: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("console: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("console: tenant %q not found", tenantID)
	}
	return nil
}

// SetVerificationToken implements AuthStore.
func (s *PostgresAuthStore) SetVerificationToken(tenantID, token string) error {
	if tenantID == "" {
		return errors.New("console: tenant_id is required")
	}
	if token == "" {
		return errors.New("console: verification token is required")
	}
	const q = `UPDATE auth_users SET verify_token = $2 WHERE tenant_id = $1`
	res, err := s.db.ExecContext(s.cx(), q, tenantID, token)
	if err != nil {
		return fmt.Errorf("console: set verification token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("console: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("console: tenant %q not found", tenantID)
	}
	return nil
}

// ConsumeVerificationToken implements AuthStore. The lookup +
// flip-to-verified pair runs inside a transaction so two
// simultaneous /verify calls cannot both flip the same row. The
// stored token is compared against the supplied token in constant
// time so a probing caller cannot enumerate which tenants are
// pending verification.
func (s *PostgresAuthStore) ConsumeVerificationToken(token string) (string, error) {
	if token == "" {
		return "", errors.New("console: verification token is required")
	}
	tx, err := s.db.BeginTx(s.cx(), nil)
	if err != nil {
		return "", fmt.Errorf("console: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Fetch every pending row and compare the supplied token
	// against each one in constant time. The total number of
	// pending tokens is bounded (one per pending signup), so
	// the scan is acceptable; the alternative SELECT WHERE
	// verify_token = $1 would short-circuit for a missing
	// token and leak the existence of valid tokens via timing.
	const selectPending = `SELECT tenant_id, verify_token FROM auth_users WHERE verify_token IS NOT NULL`
	rows, err := tx.QueryContext(s.cx(), selectPending)
	if err != nil {
		return "", fmt.Errorf("console: load pending verifications: %w", err)
	}
	supplied := []byte(token)
	var matchedTenant string
	for rows.Next() {
		var tenantID, stored string
		if err := rows.Scan(&tenantID, &stored); err != nil {
			rows.Close()
			return "", fmt.Errorf("console: scan pending verification: %w", err)
		}
		if subtle.ConstantTimeCompare(supplied, []byte(stored)) == 1 {
			matchedTenant = tenantID
			// Continue draining rows so the comparison runs
			// against every candidate; this keeps the loop's
			// timing independent of which row matched.
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("console: iterate pending verifications: %w", err)
	}
	if matchedTenant == "" {
		return "", errors.New("console: verification token invalid or expired")
	}
	const update = `UPDATE auth_users SET verified = TRUE, verify_token = NULL WHERE tenant_id = $1`
	if _, err := tx.ExecContext(s.cx(), update, matchedTenant); err != nil {
		return "", fmt.Errorf("console: clear verification token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("console: commit verification: %w", err)
	}
	return matchedTenant, nil
}

// isUniqueViolation reports whether err is a Postgres unique-
// constraint violation. The check is loose on purpose — both
// lib/pq and jackc/pgx surface the SQLSTATE in the error string
// (23505), so a substring match keeps the store driver-agnostic.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "23505") ||
		strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}
