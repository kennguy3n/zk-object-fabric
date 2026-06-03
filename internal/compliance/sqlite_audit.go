package compliance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SQLiteAuditStore is the SQLite-backed AuditStore used by the
// embedded / single-node deployment profile (docker compose up with
// no Postgres). It mirrors PostgresAuditStore's contract — append-only
// rows, ascending-timestamp Query, zero-bound TimeRange means "no
// bound" — but, like the other embedded stores, self-creates its
// schema since the embedded profile has no separate migration step.
//
// Before this store existed the embedded profile fell back to the
// in-memory audit trail, which lost every entry on restart; persisting
// to the local SQLite file means the compliance trail (now fed by both
// the interactive handler and the lifecycle evaluator) survives a
// gateway restart.
type SQLiteAuditStore struct {
	db *sql.DB
}

var _ AuditStore = (*SQLiteAuditStore)(nil)

// NewSQLiteAuditStore returns a store backed by db and creates the
// backing table + index if they do not yet exist. The caller owns the
// connection pool's lifecycle (it is shared with the other embedded
// stores via internal/embeddeddb).
func NewSQLiteAuditStore(db *sql.DB) (*SQLiteAuditStore, error) {
	if db == nil {
		return nil, errors.New("compliance: sqlite audit store requires a non-nil *sql.DB")
	}
	s := &SQLiteAuditStore{db: db}
	if err := s.ensureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteAuditStore) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS compliance_audit (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			tenant_id       TEXT NOT NULL,
			operation       TEXT NOT NULL,
			bucket          TEXT NOT NULL,
			object_key      TEXT NOT NULL,
			piece_id        TEXT NOT NULL,
			piece_backend   TEXT NOT NULL,
			backend_country TEXT NOT NULL,
			request_id      TEXT NOT NULL,
			recorded_at     TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_compliance_audit_tenant_time
			ON compliance_audit (tenant_id, recorded_at)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("compliance: ensure sqlite audit schema: %w", err)
		}
	}
	return nil
}

// Record implements AuditStore. Append-only, mirroring the Postgres
// store; a zero Timestamp is stamped with the current UTC time.
func (s *SQLiteAuditStore) Record(ctx context.Context, e AuditEntry) error {
	const q = `
INSERT INTO compliance_audit (
  tenant_id, operation, bucket, object_key, piece_id,
  piece_backend, backend_country, request_id, recorded_at
) VALUES (?,?,?,?,?,?,?,?,?)`
	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	if _, err := s.db.ExecContext(ctx, q,
		e.TenantID, e.Operation, e.Bucket, e.ObjectKey, e.PieceID,
		e.PieceBackend, e.BackendCountry, e.RequestID, ts.UTC(),
	); err != nil {
		return fmt.Errorf("compliance: insert audit row: %w", err)
	}
	return nil
}

// Query implements AuditStore. When rng.Start and/or rng.End are zero
// the corresponding bound is omitted, matching MemoryAuditStore and
// PostgresAuditStore. Entries are returned in ascending timestamp
// order; the slice may be empty but is never nil.
func (s *SQLiteAuditStore) Query(ctx context.Context, tenantID string, rng TimeRange) ([]AuditEntry, error) {
	base := `
SELECT tenant_id, operation, bucket, object_key, piece_id,
       piece_backend, backend_country, request_id, recorded_at
  FROM compliance_audit
 WHERE tenant_id = ?`
	args := []interface{}{tenantID}

	if !rng.Start.IsZero() {
		base += " AND recorded_at >= ?"
		args = append(args, rng.Start.UTC())
	}
	if !rng.End.IsZero() {
		base += " AND recorded_at <= ?"
		args = append(args, rng.End.UTC())
	}
	base += " ORDER BY recorded_at ASC"

	rows, err := s.db.QueryContext(ctx, base, args...)
	if err != nil {
		return nil, fmt.Errorf("compliance: query audit: %w", err)
	}
	defer rows.Close()
	out := make([]AuditEntry, 0)
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(
			&e.TenantID, &e.Operation, &e.Bucket, &e.ObjectKey, &e.PieceID,
			&e.PieceBackend, &e.BackendCountry, &e.RequestID, &e.Timestamp,
		); err != nil {
			return nil, fmt.Errorf("compliance: scan audit row: %w", err)
		}
		e.Timestamp = e.Timestamp.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
