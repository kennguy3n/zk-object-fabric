package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PostgresLegalHoldStore is the Postgres-backed LegalHoldStore.
// The schema lives in internal/auth/legal_response_schema.sql and
// is expected to be applied via the project's standard migration
// tooling.
//
// The store is safe for concurrent use: all methods dispatch to
// the *sql.DB which is itself safe for concurrent callers.
type PostgresLegalHoldStore struct {
	db    *sql.DB
	clock func() time.Time
}

// NewPostgresLegalHoldStore wraps db. The caller is responsible
// for opening the *sql.DB and applying legal_response_schema.sql
// before issuing the first query.
func NewPostgresLegalHoldStore(db *sql.DB) (*PostgresLegalHoldStore, error) {
	if db == nil {
		return nil, errors.New("auth: postgres legal hold store requires a non-nil *sql.DB")
	}
	return &PostgresLegalHoldStore{db: db, clock: time.Now}, nil
}

// Create inserts a hold. Duplicate IDs are rejected by the
// PRIMARY KEY constraint and surface as a non-nil error.
// CreatedAt is filled from the store clock when zero.
func (s *PostgresLegalHoldStore) Create(ctx context.Context, hold LegalHold) error {
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
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`
	expires := nullableTime(hold.ExpiresAt)
	released := nullableTime(hold.ReleasedAt)
	if _, err := s.db.ExecContext(ctx, q,
		hold.ID, hold.TenantID, hold.Bucket, hold.ObjectKey,
		hold.Reason, hold.CaseID, hold.IssuedBy,
		hold.CreatedAt.UTC(), expires, hold.Released, released,
	); err != nil {
		return fmt.Errorf("legal_hold: insert: %w", err)
	}
	return nil
}

// Release marks the hold released; the row stays in place so the
// compliance audit trail is preserved. The UPDATE is tenant-scoped
// and conditional on released = FALSE, so the same statement both
// authorizes the caller (a hold owned by another tenant matches 0
// rows) and enforces idempotency (an already-released hold matches
// 0 rows) atomically — no separate Get/authorize round-trip.
func (s *PostgresLegalHoldStore) Release(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE legal_holds SET released = TRUE, released_at = $1 WHERE id = $2 AND tenant_id = $3 AND released = FALSE`
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
func (s *PostgresLegalHoldStore) Get(ctx context.Context, id string) (LegalHold, error) {
	const q = `
SELECT id, tenant_id, bucket, object_key, reason, case_id,
       issued_by, created_at, expires_at, released, released_at
  FROM legal_holds WHERE id = $1`
	row := s.db.QueryRowContext(ctx, q, id)
	hold, err := scanHold(row)
	if errors.Is(err, sql.ErrNoRows) {
		return LegalHold{}, ErrLegalHoldNotFound
	}
	if err != nil {
		return LegalHold{}, fmt.Errorf("legal_hold: get: %w", err)
	}
	return hold, nil
}

// List returns every hold for tenantID ordered by created_at DESC
// to match the per-tenant index.
func (s *PostgresLegalHoldStore) List(ctx context.Context, tenantID string) ([]LegalHold, error) {
	const q = `
SELECT id, tenant_id, bucket, object_key, reason, case_id,
       issued_by, created_at, expires_at, released, released_at
  FROM legal_holds WHERE tenant_id = $1 ORDER BY created_at DESC`
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
// empty Bucket on the hold matches any bucket, empty ObjectKey
// matches any object within the scoped bucket, and an unset
// ExpiresAt is treated as "never expires".
func (s *PostgresLegalHoldStore) Active(ctx context.Context, tenantID, bucket, objectKey string) ([]LegalHold, error) {
	const q = `
SELECT id, tenant_id, bucket, object_key, reason, case_id,
       issued_by, created_at, expires_at, released, released_at
  FROM legal_holds
 WHERE tenant_id = $1
   AND released = FALSE
   AND (expires_at IS NULL OR expires_at > $2)
   AND (bucket = '' OR bucket = $3)
   AND (object_key = '' OR object_key = $4)`
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

// rowScanner abstracts *sql.Row and *sql.Rows so scanHold can
// serve both single-row and iterator paths.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanHold(r rowScanner) (LegalHold, error) {
	var (
		hold       LegalHold
		expires    sql.NullTime
		releasedAt sql.NullTime
	)
	if err := r.Scan(
		&hold.ID, &hold.TenantID, &hold.Bucket, &hold.ObjectKey,
		&hold.Reason, &hold.CaseID, &hold.IssuedBy,
		&hold.CreatedAt, &expires, &hold.Released, &releasedAt,
	); err != nil {
		return LegalHold{}, err
	}
	if expires.Valid {
		hold.ExpiresAt = expires.Time.UTC()
	}
	if releasedAt.Valid {
		hold.ReleasedAt = releasedAt.Time.UTC()
	}
	hold.CreatedAt = hold.CreatedAt.UTC()
	return hold, nil
}

func nullableTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t.UTC(), Valid: true}
}
