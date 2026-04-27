package compliance

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PostgresAuditStore is the Postgres-backed AuditStore. The
// schema lives in compliance/schema.sql and is expected to be
// applied via the project's standard migration tooling.
type PostgresAuditStore struct {
	db *sql.DB
}

// NewPostgresAuditStore constructs a store that uses db. The
// caller owns the connection pool's lifecycle.
func NewPostgresAuditStore(db *sql.DB) *PostgresAuditStore {
	return &PostgresAuditStore{db: db}
}

// Record implements AuditStore.
func (s *PostgresAuditStore) Record(ctx context.Context, e AuditEntry) error {
	if s.db == nil {
		return fmt.Errorf("compliance: postgres audit store: nil db")
	}
	const q = `
INSERT INTO compliance_audit (
  tenant_id, operation, bucket, object_key, piece_id,
  piece_backend, backend_country, request_id, recorded_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`
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

// Query implements AuditStore.
func (s *PostgresAuditStore) Query(ctx context.Context, tenantID string, rng TimeRange) ([]AuditEntry, error) {
	if s.db == nil {
		return nil, fmt.Errorf("compliance: postgres audit store: nil db")
	}
	const q = `
SELECT tenant_id, operation, bucket, object_key, piece_id,
       piece_backend, backend_country, request_id, recorded_at
  FROM compliance_audit
 WHERE tenant_id = $1 AND recorded_at BETWEEN $2 AND $3
 ORDER BY recorded_at ASC`
	rows, err := s.db.QueryContext(ctx, q, tenantID, rng.Start.UTC(), rng.End.UTC())
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
		out = append(out, e)
	}
	return out, rows.Err()
}


