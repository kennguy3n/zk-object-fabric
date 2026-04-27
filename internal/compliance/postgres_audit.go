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

// Query implements AuditStore. When rng.Start and/or rng.End are
// zero the corresponding bound is omitted, matching the
// MemoryAuditStore behaviour where a zero TimeRange returns all
// entries for the tenant.
func (s *PostgresAuditStore) Query(ctx context.Context, tenantID string, rng TimeRange) ([]AuditEntry, error) {
	if s.db == nil {
		return nil, fmt.Errorf("compliance: postgres audit store: nil db")
	}

	base := `
SELECT tenant_id, operation, bucket, object_key, piece_id,
       piece_backend, backend_country, request_id, recorded_at
  FROM compliance_audit
 WHERE tenant_id = $1`
	args := []interface{}{tenantID}
	idx := 2

	if !rng.Start.IsZero() && !rng.End.IsZero() {
		base += fmt.Sprintf(" AND recorded_at BETWEEN $%d AND $%d", idx, idx+1)
		args = append(args, rng.Start.UTC(), rng.End.UTC())
	} else if !rng.Start.IsZero() {
		base += fmt.Sprintf(" AND recorded_at >= $%d", idx)
		args = append(args, rng.Start.UTC())
	} else if !rng.End.IsZero() {
		base += fmt.Sprintf(" AND recorded_at <= $%d", idx)
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
		out = append(out, e)
	}
	return out, rows.Err()
}


