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
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/cors"
	"github.com/kennguy3n/zk-object-fabric/metadata/lifecycle"
	"github.com/kennguy3n/zk-object-fabric/metadata/notification"
	"github.com/kennguy3n/zk-object-fabric/metadata/object_lock"
)

// Config is the store wiring. Table defaults to "bucket_versioning",
// LockTable to "bucket_object_lock", CorsTable to "bucket_cors",
// LifecycleTable to "bucket_lifecycle", and NotificationTable to
// "bucket_notification".
type Config struct {
	DB                *sql.DB
	Table             string
	LockTable         string
	CorsTable         string
	LifecycleTable    string
	NotificationTable string
}

// Store is a bucket_config.Store backed by SQLite tables.
type Store struct {
	db                *sql.DB
	table             string
	lockTable         string
	corsTable         string
	lifecycleTable    string
	notificationTable string
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
	corsTable := cfg.CorsTable
	if corsTable == "" {
		corsTable = "bucket_cors"
	}
	if !isSafeIdent(corsTable) {
		return nil, fmt.Errorf("sqlite: invalid cors table name %q", corsTable)
	}
	lifecycleTable := cfg.LifecycleTable
	if lifecycleTable == "" {
		lifecycleTable = "bucket_lifecycle"
	}
	if !isSafeIdent(lifecycleTable) {
		return nil, fmt.Errorf("sqlite: invalid lifecycle table name %q", lifecycleTable)
	}
	notificationTable := cfg.NotificationTable
	if notificationTable == "" {
		notificationTable = "bucket_notification"
	}
	if !isSafeIdent(notificationTable) {
		return nil, fmt.Errorf("sqlite: invalid notification table name %q", notificationTable)
	}
	s := &Store{db: cfg.DB, table: table, lockTable: lockTable, corsTable: corsTable, lifecycleTable: lifecycleTable, notificationTable: notificationTable}
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
	cq := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		tenant_id  TEXT NOT NULL,
		bucket     TEXT NOT NULL,
		rules      TEXT NOT NULL,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (tenant_id, bucket)
	)`, s.corsTable)
	if _, err := s.db.ExecContext(ctx, cq); err != nil {
		return fmt.Errorf("sqlite: ensure bucket_cors schema: %w", err)
	}
	lcq := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		tenant_id  TEXT NOT NULL,
		bucket     TEXT NOT NULL,
		rules      TEXT NOT NULL,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (tenant_id, bucket)
	)`, s.lifecycleTable)
	if _, err := s.db.ExecContext(ctx, lcq); err != nil {
		return fmt.Errorf("sqlite: ensure bucket_lifecycle schema: %w", err)
	}
	nq := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		tenant_id  TEXT NOT NULL,
		bucket     TEXT NOT NULL,
		rules      TEXT NOT NULL,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (tenant_id, bucket)
	)`, s.notificationTable)
	if _, err := s.db.ExecContext(ctx, nq); err != nil {
		return fmt.Errorf("sqlite: ensure bucket_notification schema: %w", err)
	}
	return nil
}

// GetNotification returns the notification config for (tenantID,
// bucket) or the zero Config when no row exists.
func (s *Store) GetNotification(ctx context.Context, tenantID, bucket string) (notification.Config, error) {
	if tenantID == "" || bucket == "" {
		return notification.Config{}, errors.New("sqlite: tenant_id and bucket are required")
	}
	q := fmt.Sprintf(`SELECT rules FROM %s WHERE tenant_id = ? AND bucket = ?`, s.notificationTable)
	var raw []byte
	switch err := s.db.QueryRowContext(ctx, q, tenantID, bucket).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return notification.Config{}, nil
	case err != nil:
		return notification.Config{}, fmt.Errorf("sqlite: bucket_notification get: %w", err)
	}
	var cfg notification.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return notification.Config{}, fmt.Errorf("sqlite: bucket_notification decode: %w", err)
	}
	return cfg, nil
}

// SetNotification upserts the notification config for (tenantID,
// bucket). An empty cfg clears any existing configuration (the row is
// deleted), matching S3's PutBucketNotificationConfiguration with an
// empty body.
func (s *Store) SetNotification(ctx context.Context, tenantID, bucket string, cfg notification.Config) error {
	if tenantID == "" || bucket == "" {
		return errors.New("sqlite: tenant_id and bucket are required")
	}
	if err := cfg.Valid(); err != nil {
		return err
	}
	if cfg.Empty() {
		del := fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND bucket = ?`, s.notificationTable)
		if _, err := s.db.ExecContext(ctx, del, tenantID, bucket); err != nil {
			return fmt.Errorf("sqlite: bucket_notification clear: %w", err)
		}
		return nil
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("sqlite: bucket_notification encode: %w", err)
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, bucket, rules, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (tenant_id, bucket)
		DO UPDATE SET rules = excluded.rules, updated_at = CURRENT_TIMESTAMP
	`, s.notificationTable)
	if _, err := s.db.ExecContext(ctx, q, tenantID, bucket, raw); err != nil {
		return fmt.Errorf("sqlite: bucket_notification set: %w", err)
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

// GetCORS returns the CORS config for (tenantID, bucket) or the zero
// Config when no row exists.
func (s *Store) GetCORS(ctx context.Context, tenantID, bucket string) (cors.Config, error) {
	if tenantID == "" || bucket == "" {
		return cors.Config{}, errors.New("sqlite: tenant_id and bucket are required")
	}
	q := fmt.Sprintf(`SELECT rules FROM %s WHERE tenant_id = ? AND bucket = ?`, s.corsTable)
	var raw []byte
	switch err := s.db.QueryRowContext(ctx, q, tenantID, bucket).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return cors.Config{}, nil
	case err != nil:
		return cors.Config{}, fmt.Errorf("sqlite: bucket_cors get: %w", err)
	}
	var cfg cors.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cors.Config{}, fmt.Errorf("sqlite: bucket_cors decode: %w", err)
	}
	return cfg, nil
}

// SetCORS upserts the CORS config for (tenantID, bucket).
func (s *Store) SetCORS(ctx context.Context, tenantID, bucket string, cfg cors.Config) error {
	if tenantID == "" || bucket == "" {
		return errors.New("sqlite: tenant_id and bucket are required")
	}
	if err := cfg.Valid(); err != nil {
		return err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("sqlite: bucket_cors encode: %w", err)
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, bucket, rules, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (tenant_id, bucket)
		DO UPDATE SET rules = excluded.rules, updated_at = CURRENT_TIMESTAMP
	`, s.corsTable)
	if _, err := s.db.ExecContext(ctx, q, tenantID, bucket, raw); err != nil {
		return fmt.Errorf("sqlite: bucket_cors set: %w", err)
	}
	return nil
}

// DeleteCORS removes the CORS config for (tenantID, bucket). Deleting
// an unconfigured bucket is a no-op, matching S3's idempotent
// DeleteBucketCors.
func (s *Store) DeleteCORS(ctx context.Context, tenantID, bucket string) error {
	if tenantID == "" || bucket == "" {
		return errors.New("sqlite: tenant_id and bucket are required")
	}
	q := fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND bucket = ?`, s.corsTable)
	if _, err := s.db.ExecContext(ctx, q, tenantID, bucket); err != nil {
		return fmt.Errorf("sqlite: bucket_cors delete: %w", err)
	}
	return nil
}

// GetLifecycle returns the lifecycle config for (tenantID, bucket) or
// the zero Config when no row exists.
func (s *Store) GetLifecycle(ctx context.Context, tenantID, bucket string) (lifecycle.Config, error) {
	if tenantID == "" || bucket == "" {
		return lifecycle.Config{}, errors.New("sqlite: tenant_id and bucket are required")
	}
	q := fmt.Sprintf(`SELECT rules FROM %s WHERE tenant_id = ? AND bucket = ?`, s.lifecycleTable)
	var raw []byte
	switch err := s.db.QueryRowContext(ctx, q, tenantID, bucket).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return lifecycle.Config{}, nil
	case err != nil:
		return lifecycle.Config{}, fmt.Errorf("sqlite: bucket_lifecycle get: %w", err)
	}
	var cfg lifecycle.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return lifecycle.Config{}, fmt.Errorf("sqlite: bucket_lifecycle decode: %w", err)
	}
	return cfg, nil
}

// SetLifecycle upserts the lifecycle config for (tenantID, bucket).
func (s *Store) SetLifecycle(ctx context.Context, tenantID, bucket string, cfg lifecycle.Config) error {
	if tenantID == "" || bucket == "" {
		return errors.New("sqlite: tenant_id and bucket are required")
	}
	if err := cfg.Valid(); err != nil {
		return err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("sqlite: bucket_lifecycle encode: %w", err)
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, bucket, rules, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (tenant_id, bucket)
		DO UPDATE SET rules = excluded.rules, updated_at = CURRENT_TIMESTAMP
	`, s.lifecycleTable)
	if _, err := s.db.ExecContext(ctx, q, tenantID, bucket, raw); err != nil {
		return fmt.Errorf("sqlite: bucket_lifecycle set: %w", err)
	}
	return nil
}

// DeleteLifecycle removes the lifecycle config for (tenantID, bucket).
// Deleting an unconfigured bucket is a no-op, matching S3's idempotent
// DeleteBucketLifecycle.
func (s *Store) DeleteLifecycle(ctx context.Context, tenantID, bucket string) error {
	if tenantID == "" || bucket == "" {
		return errors.New("sqlite: tenant_id and bucket are required")
	}
	q := fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND bucket = ?`, s.lifecycleTable)
	if _, err := s.db.ExecContext(ctx, q, tenantID, bucket); err != nil {
		return fmt.Errorf("sqlite: bucket_lifecycle delete: %w", err)
	}
	return nil
}

// ListLifecycle returns every configured bucket lifecycle entry across
// all tenants, in (tenant_id, bucket) order, for the background
// evaluator.
func (s *Store) ListLifecycle(ctx context.Context) ([]bucket_config.LifecycleEntry, error) {
	q := fmt.Sprintf(`SELECT tenant_id, bucket, rules FROM %s ORDER BY tenant_id, bucket`, s.lifecycleTable)
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("sqlite: bucket_lifecycle list: %w", err)
	}
	defer rows.Close()
	var out []bucket_config.LifecycleEntry
	for rows.Next() {
		var tenantID, bucket string
		var raw []byte
		if err := rows.Scan(&tenantID, &bucket, &raw); err != nil {
			return nil, fmt.Errorf("sqlite: bucket_lifecycle list scan: %w", err)
		}
		var cfg lifecycle.Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("sqlite: bucket_lifecycle list decode: %w", err)
		}
		out = append(out, bucket_config.LifecycleEntry{TenantID: tenantID, Bucket: bucket, Config: cfg})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: bucket_lifecycle list rows: %w", err)
	}
	return out, nil
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
