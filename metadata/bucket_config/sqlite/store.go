// Package sqlite is the SQLite-backed implementation of
// bucket_config.Store, used by the embedded / single-node deployment
// profile (docker compose up with no Postgres). It mirrors the
// Postgres store's contract: a (tenant_id, bucket) primary key and a
// CHECK that constrains state to 'Enabled'/'Suspended'. Unlike the
// Postgres store it self-creates its schema, since the embedded
// profile has no separate migration step.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/object_lock"
)

// Config is the store wiring. Table defaults to "bucket_versioning"
// and LockTable to "bucket_object_lock".
type Config struct {
	DB        *sql.DB
	Table     string
	LockTable string
}

// Store is a bucket_config.Store backed by SQLite tables.
type Store struct {
	db        *sql.DB
	table     string
	lockTable string
}

var _ bucket_config.Store = (*Store)(nil)

// New returns a Store and creates the backing tables if they do not
// yet exist.
func New(cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, errors.New("sqlite: Config.DB is required")
	}
	table := cfg.Table
	if table == "" {
		table = "bucket_versioning"
	}
	if !isSafeIdent(table) {
		return nil, fmt.Errorf("sqlite: invalid table name %q", table)
	}
	lockTable := cfg.LockTable
	if lockTable == "" {
		lockTable = "bucket_object_lock"
	}
	if !isSafeIdent(lockTable) {
		return nil, fmt.Errorf("sqlite: invalid lock table name %q", lockTable)
	}
	s := &Store{db: cfg.DB, table: table, lockTable: lockTable}
	if err := s.ensureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureSchema(ctx context.Context) error {
	q := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		tenant_id  TEXT NOT NULL,
		bucket     TEXT NOT NULL,
		state      TEXT NOT NULL CHECK (state IN ('Enabled', 'Suspended')),
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (tenant_id, bucket)
	)`, s.table)
	if _, err := s.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("sqlite: ensure bucket_versioning schema: %w", err)
	}
	lq := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		tenant_id     TEXT NOT NULL,
		bucket        TEXT NOT NULL,
		enabled       INTEGER NOT NULL,
		default_mode  TEXT NOT NULL DEFAULT '' CHECK (default_mode IN ('', 'GOVERNANCE', 'COMPLIANCE')),
		default_days  INTEGER NOT NULL DEFAULT 0,
		default_years INTEGER NOT NULL DEFAULT 0,
		updated_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (tenant_id, bucket)
	)`, s.lockTable)
	if _, err := s.db.ExecContext(ctx, lq); err != nil {
		return fmt.Errorf("sqlite: ensure bucket_object_lock schema: %w", err)
	}
	return nil
}

// GetVersioning returns the state for (tenantID, bucket) or
// VersioningUnset when no row exists.
func (s *Store) GetVersioning(ctx context.Context, tenantID, bucket string) (bucket_config.VersioningState, error) {
	if tenantID == "" || bucket == "" {
		return bucket_config.VersioningUnset, errors.New("sqlite: tenant_id and bucket are required")
	}
	q := fmt.Sprintf(`SELECT state FROM %s WHERE tenant_id = ? AND bucket = ?`, s.table)
	var state string
	switch err := s.db.QueryRowContext(ctx, q, tenantID, bucket).Scan(&state); {
	case errors.Is(err, sql.ErrNoRows):
		return bucket_config.VersioningUnset, nil
	case err != nil:
		return bucket_config.VersioningUnset, fmt.Errorf("sqlite: bucket_versioning get: %w", err)
	}
	return bucket_config.VersioningState(state), nil
}

// SetVersioning upserts the state for (tenantID, bucket).
func (s *Store) SetVersioning(ctx context.Context, tenantID, bucket string, state bucket_config.VersioningState) error {
	if tenantID == "" || bucket == "" {
		return errors.New("sqlite: tenant_id and bucket are required")
	}
	if !state.Valid() {
		return errors.New("sqlite: state must be Enabled or Suspended")
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, bucket, state, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (tenant_id, bucket)
		DO UPDATE SET state = excluded.state, updated_at = CURRENT_TIMESTAMP
	`, s.table)
	if _, err := s.db.ExecContext(ctx, q, tenantID, bucket, string(state)); err != nil {
		return fmt.Errorf("sqlite: bucket_versioning set: %w", err)
	}
	return nil
}

// GetObjectLock returns the Object Lock config for (tenantID, bucket)
// or the zero Config when no row exists.
func (s *Store) GetObjectLock(ctx context.Context, tenantID, bucket string) (object_lock.Config, error) {
	if tenantID == "" || bucket == "" {
		return object_lock.Config{}, errors.New("sqlite: tenant_id and bucket are required")
	}
	q := fmt.Sprintf(`SELECT enabled, default_mode, default_days, default_years FROM %s WHERE tenant_id = ? AND bucket = ?`, s.lockTable)
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
		return object_lock.Config{}, fmt.Errorf("sqlite: bucket_object_lock get: %w", err)
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
		return errors.New("sqlite: tenant_id and bucket are required")
	}
	if err := cfg.Valid(); err != nil {
		return err
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, bucket, enabled, default_mode, default_days, default_years, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (tenant_id, bucket)
		DO UPDATE SET enabled = excluded.enabled, default_mode = excluded.default_mode,
			default_days = excluded.default_days, default_years = excluded.default_years,
			updated_at = CURRENT_TIMESTAMP
	`, s.lockTable)
	if _, err := s.db.ExecContext(ctx, q, tenantID, bucket, cfg.Enabled, string(cfg.DefaultMode), cfg.DefaultDays, cfg.DefaultYears); err != nil {
		return fmt.Errorf("sqlite: bucket_object_lock set: %w", err)
	}
	return nil
}

// isSafeIdent guards the table name we interpolate into SQL.
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
