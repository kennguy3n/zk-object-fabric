package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SQLiteLegalHoldStore is the SQLite-backed LegalHoldStore used by
// the embedded / single-node deployment profile (docker compose up
// with no Postgres). It mirrors PostgresLegalHoldStore's contract —
// append-only rows (a release flips released/released_at, the row is
// never deleted), created_at ordering on List, and the same
// Matches-equivalent Active query — but, like the other embedded
// stores, self-creates its schema since the embedded profile has no
// separate migration step.
//
// Before this store existed the embedded profile fell back to the
// in-memory legal-hold store, which lost every hold on restart —
// dropping a WORM/compliance guarantee. Persisting to the local
// SQLite file means active holds survive a gateway restart and keep
// blocking deletes across the interactive DELETE, lifecycle expiry,
// and orphan-GC paths.
type SQLiteLegalHoldStore struct {
	db    *sql.DB
	clock func() time.Time
}

var _ LegalHoldStore = (*SQLiteLegalHoldStore)(nil)

// NewSQLiteLegalHoldStore returns a store backed by db and creates
// the backing table + indexes if they do not yet exist. The caller
// owns the connection pool's lifecycle (it is shared with the other
// embedded stores via internal/embeddeddb).
func NewSQLiteLegalHoldStore(db *sql.DB) (*SQLiteLegalHoldStore, error) {
	if db == nil {
		return nil, errors.New("auth: sqlite legal hold store requires a non-nil *sql.DB")
	}
	s := &SQLiteLegalHoldStore{db: db, clock: time.Now}
	if err := s.ensureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteLegalHoldStore) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS legal_holds (
			id           TEXT PRIMARY KEY,
			tenant_id    TEXT NOT NULL,
			bucket       TEXT NOT NULL DEFAULT '',
			object_key   TEXT NOT NULL DEFAULT '',
			reason       TEXT NOT NULL,
			case_id      TEXT NOT NULL DEFAULT '',
			issued_by    TEXT NOT NULL,
			created_at   TIMESTAMP NOT NULL,
			expires_at   TIMESTAMP,
			released     INTEGER NOT NULL DEFAULT 0,
			released_at  TIMESTAMP
		)`,
		// Hot-path lookup used by Active on every DELETE. Partial
		// index on the unreleased rows mirrors the Postgres schema.
		`CREATE INDEX IF NOT EXISTS idx_legal_holds_tenant_bucket_key
			ON legal_holds (tenant_id, bucket, object_key)
			WHERE released = 0`,
		// Per-tenant listing for the console handler.
		`CREATE INDEX IF NOT EXISTS idx_legal_holds_tenant
			ON legal_holds (tenant_id, created_at DESC)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("auth: ensure sqlite legal hold schema: %w", err)
		}
	}
	return nil
}

// Create inserts a hold. Duplicate IDs are rejected by the PRIMARY
// KEY constraint and surface as a non-nil error. CreatedAt is filled
// from the store clock when zero.
func (s *SQLiteLegalHoldStore) Create(ctx context.Context, hold LegalHold) error {
	if hold.ID == "" || hold.TenantID == "" {
		return errors.New("legal_hold: id and tenant_id are required")
	}
	if hold.IssuedBy == "" || hold.Reason == "" {
		return errors.New("legal_hold: issued_by and reason are required")
	}
	if hold.CreatedAt.IsZero() {
		hold.CreatedAt = s.clock().UTC()
	}
	const q = `
INSERT INTO legal_holds (
  id, tenant_id, bucket, object_key, reason, case_id,
  issued_by, created_at, expires_at, released, released_at
) VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	if _, err := s.db.ExecContext(ctx, q,
		hold.ID, hold.TenantID, hold.Bucket, hold.ObjectKey,
		hold.Reason, hold.CaseID, hold.IssuedBy,
		hold.CreatedAt.UTC(), nullableTime(hold.ExpiresAt),
		hold.Released, nullableTime(hold.ReleasedAt),
	); err != nil {
		return fmt.Errorf("legal_hold: insert: %w", err)
	}
	return nil
}

// Release marks the hold released; the row stays in place so the
// compliance audit trail is preserved. The UPDATE is tenant-scoped
// and conditional on released = 0, so the same statement both
// authorizes the caller (a hold owned by another tenant matches 0
// rows) and enforces idempotency (an already-released hold matches
// 0 rows) atomically — no separate Get/authorize round-trip.
func (s *SQLiteLegalHoldStore) Release(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE legal_holds SET released = 1, released_at = ? WHERE id = ? AND tenant_id = ? AND released = 0`
	res, err := s.db.ExecContext(ctx, q, s.clock().UTC(), id, tenantID)
	if err != nil {
		return fmt.Errorf("legal_hold: release: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("legal_hold: release rows: %w", err)
	}
	if n == 0 {
		return ErrLegalHoldNotFound
	}
	return nil
}

// Get returns the hold with the given id, or ErrLegalHoldNotFound.
func (s *SQLiteLegalHoldStore) Get(ctx context.Context, id string) (LegalHold, error) {
	const q = `
SELECT id, tenant_id, bucket, object_key, reason, case_id,
       issued_by, created_at, expires_at, released, released_at
  FROM legal_holds WHERE id = ?`
	hold, err := scanHold(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return LegalHold{}, ErrLegalHoldNotFound
	}
	if err != nil {
		return LegalHold{}, fmt.Errorf("legal_hold: get: %w", err)
	}
	return hold, nil
}

// List returns every hold for tenantID ordered by created_at DESC to
// match the per-tenant index.
func (s *SQLiteLegalHoldStore) List(ctx context.Context, tenantID string) ([]LegalHold, error) {
	const q = `
SELECT id, tenant_id, bucket, object_key, reason, case_id,
       issued_by, created_at, expires_at, released, released_at
  FROM legal_holds WHERE tenant_id = ? ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("legal_hold: list: %w", err)
	}
	defer rows.Close()
	out := make([]LegalHold, 0)
	for rows.Next() {
		hold, err := scanHold(rows)
		if err != nil {
			return nil, fmt.Errorf("legal_hold: list scan: %w", err)
		}
		out = append(out, hold)
	}
	return out, rows.Err()
}

// Active returns the subset of holds that currently apply to
// (tenantID, bucket, objectKey). The query mirrors LegalHold.Matches:
// empty Bucket on the hold matches any bucket, empty ObjectKey matches
// any object within the scoped bucket, and an unset ExpiresAt (NULL)
// is treated as "never expires".
func (s *SQLiteLegalHoldStore) Active(ctx context.Context, tenantID, bucket, objectKey string) ([]LegalHold, error) {
	const q = `
SELECT id, tenant_id, bucket, object_key, reason, case_id,
       issued_by, created_at, expires_at, released, released_at
  FROM legal_holds
 WHERE tenant_id = ?
   AND released = 0
   AND (expires_at IS NULL OR expires_at > ?)
   AND (bucket = '' OR bucket = ?)
   AND (object_key = '' OR object_key = ?)`
	rows, err := s.db.QueryContext(ctx, q, tenantID, s.clock().UTC(), bucket, objectKey)
	if err != nil {
		return nil, fmt.Errorf("legal_hold: active: %w", err)
	}
	defer rows.Close()
	out := make([]LegalHold, 0)
	for rows.Next() {
		hold, err := scanHold(rows)
		if err != nil {
			return nil, fmt.Errorf("legal_hold: active scan: %w", err)
		}
		out = append(out, hold)
	}
	return out, rows.Err()
}
