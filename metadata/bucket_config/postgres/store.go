// Package postgres is the Postgres-backed implementation of
// bucket_config.Store. The connection/driver registration is the
// caller's responsibility (the gateway binary registers
// github.com/lib/pq). See metadata/bucket_config/schema.sql for the
// table definition; the migration runner applies it before the
// gateway starts.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
)

// Config is the store wiring. Table defaults to "bucket_versioning".
type Config struct {
	DB    *sql.DB
	Table string
}

// Store is a bucket_config.Store backed by a Postgres table.
type Store struct {
	db    *sql.DB
	table string
}

var _ bucket_config.Store = (*Store)(nil)

// New returns a Store. It does not open or verify the connection;
// callers should have already pinged the pool.
func New(cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, errors.New("postgres: Config.DB is required")
	}
	table := cfg.Table
	if table == "" {
		table = "bucket_versioning"
	}
	if !isSafeIdent(table) {
		return nil, fmt.Errorf("postgres: invalid table name %q", table)
	}
	return &Store{db: cfg.DB, table: table}, nil
}

// GetVersioning returns the state for (tenantID, bucket) or
// VersioningUnset when no row exists.
func (s *Store) GetVersioning(ctx context.Context, tenantID, bucket string) (bucket_config.VersioningState, error) {
	if tenantID == "" || bucket == "" {
		return bucket_config.VersioningUnset, errors.New("postgres: tenant_id and bucket are required")
	}
	q := fmt.Sprintf(`SELECT state FROM %s WHERE tenant_id = $1 AND bucket = $2`, s.table)
	var state string
	switch err := s.db.QueryRowContext(ctx, q, tenantID, bucket).Scan(&state); {
	case errors.Is(err, sql.ErrNoRows):
		return bucket_config.VersioningUnset, nil
	case err != nil:
		return bucket_config.VersioningUnset, fmt.Errorf("postgres: bucket_versioning get: %w", err)
	}
	return bucket_config.VersioningState(state), nil
}

// SetVersioning upserts the state for (tenantID, bucket).
func (s *Store) SetVersioning(ctx context.Context, tenantID, bucket string, state bucket_config.VersioningState) error {
	if tenantID == "" || bucket == "" {
		return errors.New("postgres: tenant_id and bucket are required")
	}
	if !state.Valid() {
		return errors.New("postgres: state must be Enabled or Suspended")
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, bucket, state, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (tenant_id, bucket)
		DO UPDATE SET state = EXCLUDED.state, updated_at = now()
	`, s.table)
	if _, err := s.db.ExecContext(ctx, q, tenantID, bucket, string(state)); err != nil {
		return fmt.Errorf("postgres: bucket_versioning set: %w", err)
	}
	return nil
}

// isSafeIdent guards the table name we interpolate into SQL (the
// driver cannot parameterise identifiers).
func isSafeIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
		isDigit := r >= '0' && r <= '9'
		switch {
		case isLetter:
		case isDigit && i > 0:
		default:
			return false
		}
	}
	return true
}
