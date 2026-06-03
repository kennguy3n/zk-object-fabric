package console

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SQLiteRefreshTokenStore is the SQLite-backed RefreshTokenStore for
// the embedded / single-node profile. It persists refresh tokens (by
// hash) so a returning SPA survives a gateway restart, mirroring the
// SQLiteAuthStore that lives in the same embedded database.
//
// Concurrency: Rotate reads a row and then updates / inserts based on
// what it found, so it runs inside an explicit transaction. A
// database/sql Tx pins one connection for its lifetime, which makes the
// read-then-write atomic even though the embedded pool would otherwise
// hand the connection back between statements.
type SQLiteRefreshTokenStore struct {
	db  *sql.DB
	cfg RefreshConfig
	now func() time.Time
	ctx context.Context
}

// NewSQLiteRefreshTokenStore wraps db and creates the refresh_tokens
// table if it does not yet exist.
func NewSQLiteRefreshTokenStore(db *sql.DB, cfg RefreshConfig) (*SQLiteRefreshTokenStore, error) {
	if db == nil {
		return nil, errors.New("console: sqlite refresh store requires a non-nil *sql.DB")
	}
	s := &SQLiteRefreshTokenStore{db: db, cfg: cfg, now: cfg.resolveClock()}
	if err := s.ensureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteRefreshTokenStore) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS refresh_tokens (
			token_hash  TEXT    PRIMARY KEY,
			family_id   TEXT    NOT NULL,
			tenant_id   TEXT    NOT NULL,
			expires_at  INTEGER NOT NULL,
			consumed    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_tokens_family ON refresh_tokens(family_id)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_tokens_tenant ON refresh_tokens(tenant_id)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("console: ensure refresh_tokens schema: %w", err)
		}
	}
	return nil
}

// WithContext returns a copy of the store bound to ctx.
func (s *SQLiteRefreshTokenStore) WithContext(ctx context.Context) *SQLiteRefreshTokenStore {
	clone := *s
	clone.ctx = ctx
	return &clone
}

func (s *SQLiteRefreshTokenStore) cx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// Issue implements RefreshTokenStore.
func (s *SQLiteRefreshTokenStore) Issue(tenantID string) (RefreshToken, error) {
	if tenantID == "" {
		return RefreshToken{}, errors.New("console: tenant_id is required to issue a refresh token")
	}
	family, err := newRefreshFamilyID()
	if err != nil {
		return RefreshToken{}, err
	}
	raw, err := newRawRefreshToken()
	if err != nil {
		return RefreshToken{}, err
	}
	expiresAt := s.now().Add(s.cfg.ttl())
	const q = `INSERT INTO refresh_tokens (token_hash, family_id, tenant_id, expires_at, consumed)
		VALUES (?, ?, ?, ?, 0)`
	if _, err := s.db.ExecContext(s.cx(), q, hashRefreshToken(raw), family, tenantID, expiresAt.UnixNano()); err != nil {
		return RefreshToken{}, fmt.Errorf("console: insert refresh token: %w", err)
	}
	return RefreshToken{Raw: raw, TenantID: tenantID, ExpiresAt: expiresAt}, nil
}

// Rotate implements RefreshTokenStore.
func (s *SQLiteRefreshTokenStore) Rotate(rawToken string) (RefreshToken, error) {
	if rawToken == "" {
		return RefreshToken{}, errRefreshTokenInvalid
	}
	hash := hashRefreshToken(rawToken)
	now := s.now()

	tx, err := s.db.BeginTx(s.cx(), nil)
	if err != nil {
		return RefreshToken{}, fmt.Errorf("console: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		familyID  string
		tenantID  string
		expiresAt int64
		consumed  bool
	)
	const sel = `SELECT family_id, tenant_id, expires_at, consumed FROM refresh_tokens WHERE token_hash = ?`
	switch err := tx.QueryRowContext(s.cx(), sel, hash).Scan(&familyID, &tenantID, &expiresAt, &consumed); {
	case errors.Is(err, sql.ErrNoRows):
		return RefreshToken{}, errRefreshTokenInvalid
	case err != nil:
		return RefreshToken{}, fmt.Errorf("console: load refresh token: %w", err)
	}

	if now.UnixNano() >= expiresAt {
		if _, err := tx.ExecContext(s.cx(), `DELETE FROM refresh_tokens WHERE token_hash = ?`, hash); err != nil {
			return RefreshToken{}, fmt.Errorf("console: delete expired refresh token: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return RefreshToken{}, fmt.Errorf("console: commit expired delete: %w", err)
		}
		return RefreshToken{}, errRefreshTokenInvalid
	}

	if consumed {
		// Reuse of a rotated token: revoke the whole family.
		if _, err := tx.ExecContext(s.cx(), `DELETE FROM refresh_tokens WHERE family_id = ?`, familyID); err != nil {
			return RefreshToken{}, fmt.Errorf("console: revoke refresh family: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return RefreshToken{}, fmt.Errorf("console: commit family revoke: %w", err)
		}
		return RefreshToken{}, errRefreshTokenReuse
	}

	if _, err := tx.ExecContext(s.cx(), `UPDATE refresh_tokens SET consumed = 1 WHERE token_hash = ?`, hash); err != nil {
		return RefreshToken{}, fmt.Errorf("console: consume refresh token: %w", err)
	}
	raw, err := newRawRefreshToken()
	if err != nil {
		return RefreshToken{}, err
	}
	newExpiresAt := now.Add(s.cfg.ttl())
	const ins = `INSERT INTO refresh_tokens (token_hash, family_id, tenant_id, expires_at, consumed)
		VALUES (?, ?, ?, ?, 0)`
	if _, err := tx.ExecContext(s.cx(), ins, hashRefreshToken(raw), familyID, tenantID, newExpiresAt.UnixNano()); err != nil {
		return RefreshToken{}, fmt.Errorf("console: insert rotated refresh token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RefreshToken{}, fmt.Errorf("console: commit rotation: %w", err)
	}
	return RefreshToken{Raw: raw, TenantID: tenantID, ExpiresAt: newExpiresAt}, nil
}

// Revoke implements RefreshTokenStore. Idempotent: a missing token is
// not an error.
func (s *SQLiteRefreshTokenStore) Revoke(rawToken string) error {
	if rawToken == "" {
		return nil
	}
	const q = `DELETE FROM refresh_tokens WHERE token_hash = ?`
	if _, err := s.db.ExecContext(s.cx(), q, hashRefreshToken(rawToken)); err != nil {
		return fmt.Errorf("console: revoke refresh token: %w", err)
	}
	return nil
}

// RevokeAllForTenant implements RefreshTokenStore.
func (s *SQLiteRefreshTokenStore) RevokeAllForTenant(tenantID string) error {
	if tenantID == "" {
		return nil
	}
	const q = `DELETE FROM refresh_tokens WHERE tenant_id = ?`
	if _, err := s.db.ExecContext(s.cx(), q, tenantID); err != nil {
		return fmt.Errorf("console: revoke tenant refresh tokens: %w", err)
	}
	return nil
}
