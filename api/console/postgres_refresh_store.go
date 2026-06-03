package console

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PostgresRefreshTokenStore is the Postgres-backed RefreshTokenStore
// for the multi-replica production profile. It persists refresh tokens
// (by hash) in the refresh_tokens table defined in
// api/console/schema.sql, so every gateway replica behind a load
// balancer agrees on which tokens are live — the property the
// process-local MemoryRefreshTokenStore cannot offer.
//
// Like PostgresAuthStore it does not create its own schema; operators
// run schema.sql before the first query.
type PostgresRefreshTokenStore struct {
	db  *sql.DB
	cfg RefreshConfig
	ctx context.Context
}

// NewPostgresRefreshTokenStore wraps db.
func NewPostgresRefreshTokenStore(db *sql.DB, cfg RefreshConfig) (*PostgresRefreshTokenStore, error) {
	if db == nil {
		return nil, errors.New("console: postgres refresh store requires a non-nil *sql.DB")
	}
	return &PostgresRefreshTokenStore{db: db, cfg: cfg}, nil
}

// WithContext returns a copy of the store bound to ctx.
func (s *PostgresRefreshTokenStore) WithContext(ctx context.Context) *PostgresRefreshTokenStore {
	clone := *s
	clone.ctx = ctx
	return &clone
}

func (s *PostgresRefreshTokenStore) cx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// Issue implements RefreshTokenStore.
func (s *PostgresRefreshTokenStore) Issue(tenantID string) (RefreshToken, error) {
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
	expiresAt := s.cfg.now()().Add(s.cfg.ttl())
	const q = `INSERT INTO refresh_tokens (token_hash, family_id, tenant_id, expires_at, consumed)
		VALUES ($1, $2, $3, $4, FALSE)`
	if _, err := s.db.ExecContext(s.cx(), q, hashRefreshToken(raw), family, tenantID, expiresAt.UnixNano()); err != nil {
		return RefreshToken{}, fmt.Errorf("console: insert refresh token: %w", err)
	}
	return RefreshToken{Raw: raw, TenantID: tenantID, ExpiresAt: expiresAt}, nil
}

// Rotate implements RefreshTokenStore. The read-then-write runs inside
// a transaction; the SELECT takes FOR UPDATE so two concurrent refresh
// calls presenting the same token serialise rather than both minting a
// successor.
func (s *PostgresRefreshTokenStore) Rotate(rawToken string) (RefreshToken, error) {
	if rawToken == "" {
		return RefreshToken{}, errRefreshTokenInvalid
	}
	hash := hashRefreshToken(rawToken)
	now := s.cfg.now()()

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
	const sel = `SELECT family_id, tenant_id, expires_at, consumed
		FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`
	switch err := tx.QueryRowContext(s.cx(), sel, hash).Scan(&familyID, &tenantID, &expiresAt, &consumed); {
	case errors.Is(err, sql.ErrNoRows):
		return RefreshToken{}, errRefreshTokenInvalid
	case err != nil:
		return RefreshToken{}, fmt.Errorf("console: load refresh token: %w", err)
	}

	if now.UnixNano() >= expiresAt {
		if _, err := tx.ExecContext(s.cx(), `DELETE FROM refresh_tokens WHERE token_hash = $1`, hash); err != nil {
			return RefreshToken{}, fmt.Errorf("console: delete expired refresh token: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return RefreshToken{}, fmt.Errorf("console: commit expired delete: %w", err)
		}
		return RefreshToken{}, errRefreshTokenInvalid
	}

	if consumed {
		if _, err := tx.ExecContext(s.cx(), `DELETE FROM refresh_tokens WHERE family_id = $1`, familyID); err != nil {
			return RefreshToken{}, fmt.Errorf("console: revoke refresh family: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return RefreshToken{}, fmt.Errorf("console: commit family revoke: %w", err)
		}
		return RefreshToken{}, errRefreshTokenReuse
	}

	if _, err := tx.ExecContext(s.cx(), `UPDATE refresh_tokens SET consumed = TRUE WHERE token_hash = $1`, hash); err != nil {
		return RefreshToken{}, fmt.Errorf("console: consume refresh token: %w", err)
	}
	raw, err := newRawRefreshToken()
	if err != nil {
		return RefreshToken{}, err
	}
	newExpiresAt := now.Add(s.cfg.ttl())
	const ins = `INSERT INTO refresh_tokens (token_hash, family_id, tenant_id, expires_at, consumed)
		VALUES ($1, $2, $3, $4, FALSE)`
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
func (s *PostgresRefreshTokenStore) Revoke(rawToken string) error {
	if rawToken == "" {
		return nil
	}
	const q = `DELETE FROM refresh_tokens WHERE token_hash = $1`
	if _, err := s.db.ExecContext(s.cx(), q, hashRefreshToken(rawToken)); err != nil {
		return fmt.Errorf("console: revoke refresh token: %w", err)
	}
	return nil
}

// RevokeAllForTenant implements RefreshTokenStore.
func (s *PostgresRefreshTokenStore) RevokeAllForTenant(tenantID string) error {
	if tenantID == "" {
		return nil
	}
	const q = `DELETE FROM refresh_tokens WHERE tenant_id = $1`
	if _, err := s.db.ExecContext(s.cx(), q, tenantID); err != nil {
		return fmt.Errorf("console: revoke tenant refresh tokens: %w", err)
	}
	return nil
}
