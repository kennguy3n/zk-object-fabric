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
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/cors"
	"github.com/kennguy3n/zk-object-fabric/metadata/object_lock"
)

// Config is the store wiring. Table defaults to "bucket_versioning",
// LockTable to "bucket_object_lock", and CorsTable to "bucket_cors".
type Config struct {
	DB        *sql.DB
	Table     string
	LockTable string
	CorsTable string
}

// Store is a bucket_config.Store backed by Postgres tables.
type Store struct {
	db        *sql.DB
	table     string
	lockTable string
	corsTable string
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
	lockTable := cfg.LockTable
	if lockTable == "" {
		lockTable = "bucket_object_lock"
	}
	if !isSafeIdent(lockTable) {
		return nil, fmt.Errorf("postgres: invalid lock table name %q", lockTable)
	}
	corsTable := cfg.CorsTable
	if corsTable == "" {
		corsTable = "bucket_cors"
	}
	if !isSafeIdent(corsTable) {
		return nil, fmt.Errorf("postgres: invalid cors table name %q", corsTable)
	}
	return &Store{db: cfg.DB, table: table, lockTable: lockTable, corsTable: corsTable}, nil
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

// GetObjectLock returns the Object Lock config for (tenantID, bucket)
// or the zero Config when no row exists.
func (s *Store) GetObjectLock(ctx context.Context, tenantID, bucket string) (object_lock.Config, error) {
	if tenantID == "" || bucket == "" {
		return object_lock.Config{}, errors.New("postgres: tenant_id and bucket are required")
	}
	q := fmt.Sprintf(`SELECT enabled, default_mode, default_days, default_years FROM %s WHERE tenant_id = $1 AND bucket = $2`, s.lockTable)
	var (
		enabled     bool
		defaultMode string
		defaultDays int
		defaultYrs  int
	)
	switch err := s.db.QueryRowContext(ctx, q, tenantID, bucket).Scan(&enabled, &defaultMode, &defaultDays, &defaultYrs); {
	case errors.Is(err, sql.ErrNoRows):
		return object_lock.Config{}, nil
	case err != nil:
		return object_lock.Config{}, fmt.Errorf("postgres: bucket_object_lock get: %w", err)
	}
	return object_lock.Config{
		Enabled:      enabled,
		DefaultMode:  object_lock.RetentionMode(defaultMode),
		DefaultDays:  defaultDays,
		DefaultYears: defaultYrs,
	}, nil
}

// SetObjectLock upserts the Object Lock config for (tenantID, bucket).
func (s *Store) SetObjectLock(ctx context.Context, tenantID, bucket string, cfg object_lock.Config) error {
	if tenantID == "" || bucket == "" {
		return errors.New("postgres: tenant_id and bucket are required")
	}
	if err := cfg.Valid(); err != nil {
		return err
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, bucket, enabled, default_mode, default_days, default_years, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (tenant_id, bucket)
		DO UPDATE SET enabled = EXCLUDED.enabled, default_mode = EXCLUDED.default_mode,
			default_days = EXCLUDED.default_days, default_years = EXCLUDED.default_years,
			updated_at = now()
	`, s.lockTable)
	if _, err := s.db.ExecContext(ctx, q, tenantID, bucket, cfg.Enabled, string(cfg.DefaultMode), cfg.DefaultDays, cfg.DefaultYears); err != nil {
		return fmt.Errorf("postgres: bucket_object_lock set: %w", err)
	}
	return nil
}

// GetCORS returns the CORS config for (tenantID, bucket) or the zero
// Config when no row exists.
func (s *Store) GetCORS(ctx context.Context, tenantID, bucket string) (cors.Config, error) {
	if tenantID == "" || bucket == "" {
		return cors.Config{}, errors.New("postgres: tenant_id and bucket are required")
	}
	q := fmt.Sprintf(`SELECT rules FROM %s WHERE tenant_id = $1 AND bucket = $2`, s.corsTable)
	var raw []byte
	switch err := s.db.QueryRowContext(ctx, q, tenantID, bucket).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return cors.Config{}, nil
	case err != nil:
		return cors.Config{}, fmt.Errorf("postgres: bucket_cors get: %w", err)
	}
	var cfg cors.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cors.Config{}, fmt.Errorf("postgres: bucket_cors decode: %w", err)
	}
	return cfg, nil
}

// SetCORS upserts the CORS config for (tenantID, bucket).
func (s *Store) SetCORS(ctx context.Context, tenantID, bucket string, cfg cors.Config) error {
	if tenantID == "" || bucket == "" {
		return errors.New("postgres: tenant_id and bucket are required")
	}
	if err := cfg.Valid(); err != nil {
		return err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("postgres: bucket_cors encode: %w", err)
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, bucket, rules, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (tenant_id, bucket)
		DO UPDATE SET rules = EXCLUDED.rules, updated_at = now()
	`, s.corsTable)
	if _, err := s.db.ExecContext(ctx, q, tenantID, bucket, raw); err != nil {
		return fmt.Errorf("postgres: bucket_cors set: %w", err)
	}
	return nil
}

// DeleteCORS removes the CORS config for (tenantID, bucket). Deleting
// an unconfigured bucket is a no-op (DELETE affecting zero rows is not
// an error), matching S3's idempotent DeleteBucketCors.
func (s *Store) DeleteCORS(ctx context.Context, tenantID, bucket string) error {
	if tenantID == "" || bucket == "" {
		return errors.New("postgres: tenant_id and bucket are required")
	}
	q := fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND bucket = $2`, s.corsTable)
	if _, err := s.db.ExecContext(ctx, q, tenantID, bucket); err != nil {
		return fmt.Errorf("postgres: bucket_cors delete: %w", err)
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
