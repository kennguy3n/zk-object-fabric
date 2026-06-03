package console

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SQLiteAuthStore is the SQLite-backed AuthStore for the embedded /
// single-node deployment profile (docker compose up with no
// Postgres). It mirrors the contract of MemoryAuthStore and
// PostgresAuthStore (single row per email; tenant_id, password hash,
// verified flag, optional verification token) but persists the rows
// to a local SQLite database file so signups survive a gateway
// restart.
//
// The store satisfies AuthStore in api/console/auth_handler.go.
//
// Concurrency: the embedded DB is opened with a single connection
// (see internal/embeddeddb), so ConsumeVerificationToken's
// scan-then-update runs without interleaving; the explicit
// transaction is retained for durability and to match the Postgres
// store's semantics.
type SQLiteAuthStore struct {
	db  *sql.DB
	ctx context.Context
}

// NewSQLiteAuthStore wraps db and creates the auth_users table if it
// does not yet exist. Callers open the *sql.DB via
// internal/embeddeddb.
func NewSQLiteAuthStore(db *sql.DB) (*SQLiteAuthStore, error) {
	if db == nil {
		return nil, errors.New("console: sqlite auth store requires a non-nil *sql.DB")
	}
	s := &SQLiteAuthStore{db: db}
	if err := s.ensureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteAuthStore) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS auth_users (
			email         TEXT    PRIMARY KEY,
			password_hash TEXT    NOT NULL,
			tenant_id     TEXT    NOT NULL,
			verified      INTEGER NOT NULL DEFAULT 0,
			verify_token  TEXT,
			created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_auth_users_tenant ON auth_users(tenant_id)`,
		`CREATE INDEX IF NOT EXISTS idx_auth_users_verify_token
			ON auth_users(verify_token) WHERE verify_token IS NOT NULL`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("console: ensure auth_users schema: %w", err)
		}
	}
	return nil
}

// WithContext returns a copy of the store bound to ctx. The returned
// store reuses the underlying *sql.DB so concurrent callers can each
// derive their own context-bound view.
func (s *SQLiteAuthStore) WithContext(ctx context.Context) *SQLiteAuthStore {
	clone := *s
	clone.ctx = ctx
	return &clone
}

func (s *SQLiteAuthStore) cx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// CreateUser implements AuthStore.
func (s *SQLiteAuthStore) CreateUser(email, passwordHash, tenantID string) error {
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
		VALUES (?, ?, ?, 0, NULL)`
	_, err := s.db.ExecContext(s.cx(), q, normalizeEmail(email), passwordHash, tenantID)
	if err != nil {
		if isSQLiteUniqueViolation(err) {
			return fmt.Errorf("console: email %q is already registered", email)
		}
		return fmt.Errorf("console: insert auth user: %w", err)
	}
	return nil
}

// LookupUser implements AuthStore.
func (s *SQLiteAuthStore) LookupUser(email string) (string, string, bool) {
	if email == "" {
		return "", "", false
	}
	const q = `SELECT password_hash, tenant_id FROM auth_users WHERE email = ?`
	var hash, tenantID string
	err := s.db.QueryRowContext(s.cx(), q, normalizeEmail(email)).Scan(&hash, &tenantID)
	if err != nil {
		return "", "", false
	}
	return hash, tenantID, true
}

// DeleteUser implements AuthStore. Idempotent: a missing email is
// not an error.
func (s *SQLiteAuthStore) DeleteUser(email string) error {
	if email == "" {
		return errors.New("console: email is required")
	}
	const q = `DELETE FROM auth_users WHERE email = ?`
	if _, err := s.db.ExecContext(s.cx(), q, normalizeEmail(email)); err != nil {
		return fmt.Errorf("console: delete auth user: %w", err)
	}
	return nil
}

// IsVerified implements AuthStore. Returns (verified, true) when a
// row exists for tenantID, (false, false) otherwise.
func (s *SQLiteAuthStore) IsVerified(tenantID string) (bool, bool) {
	if tenantID == "" {
		return false, false
	}
	const q = `SELECT verified FROM auth_users WHERE tenant_id = ? LIMIT 1`
	var verified bool
	err := s.db.QueryRowContext(s.cx(), q, tenantID).Scan(&verified)
	if err != nil {
		return false, false
	}
	return verified, true
}

// MarkVerified implements AuthStore.
func (s *SQLiteAuthStore) MarkVerified(tenantID string) error {
	if tenantID == "" {
		return errors.New("console: tenant_id is required")
	}
	const q = `UPDATE auth_users SET verified = 1, verify_token = NULL WHERE tenant_id = ?`
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
func (s *SQLiteAuthStore) SetVerificationToken(tenantID, token string) error {
	if tenantID == "" {
		return errors.New("console: tenant_id is required")
	}
	if token == "" {
		return errors.New("console: verification token is required")
	}
	const q = `UPDATE auth_users SET verify_token = ? WHERE tenant_id = ?`
	res, err := s.db.ExecContext(s.cx(), q, token, tenantID)
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
// flip-to-verified pair runs inside a transaction. The stored token
// is compared against the supplied token in constant time so a
// probing caller cannot enumerate which tenants are pending
// verification.
func (s *SQLiteAuthStore) ConsumeVerificationToken(token string) (string, error) {
	if token == "" {
		return "", errors.New("console: verification token is required")
	}
	tx, err := s.db.BeginTx(s.cx(), nil)
	if err != nil {
		return "", fmt.Errorf("console: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

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
			// Keep draining so the loop's timing is independent
			// of which row matched.
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("console: iterate pending verifications: %w", err)
	}
	if matchedTenant == "" {
		return "", errors.New("console: verification token invalid or expired")
	}
	const update = `UPDATE auth_users SET verified = 1, verify_token = NULL WHERE tenant_id = ?`
	if _, err := tx.ExecContext(s.cx(), update, matchedTenant); err != nil {
		return "", fmt.Errorf("console: clear verification token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("console: commit verification: %w", err)
	}
	return matchedTenant, nil
}

// isSQLiteUniqueViolation reports whether err is a SQLite unique-
// constraint violation. modernc.org/sqlite surfaces these as
// "constraint failed: UNIQUE constraint failed: ...".
func isSQLiteUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT FAILED")
}

var _ AuthStore = (*SQLiteAuthStore)(nil)
