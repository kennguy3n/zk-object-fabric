// Package postgres is the Postgres-backed ManifestStore. It fulfils
// the Phase 2 control-plane gate "encrypted manifest storage in the
// AWS control plane" (docs/PROGRESS.md).
//
// The implementation uses database/sql with JSONB for the opaque
// manifest body and a composite primary key on
// (tenant_id, bucket, object_key_hash, version_id). The driver is
// the caller's responsibility — this package imports no concrete
// driver so the module graph stays small. Register a driver (e.g.
// github.com/lib/pq or github.com/jackc/pgx/v5/stdlib) in the calling
// binary and hand this package an *sql.DB.
//
// Schema (run once per cell):
//
//	CREATE TABLE IF NOT EXISTS manifests (
//	    tenant_id          TEXT  NOT NULL,
//	    bucket             TEXT  NOT NULL,
//	    object_key_hash    TEXT  NOT NULL,
//	    version_id         TEXT  NOT NULL,
//	    body               JSONB NOT NULL,
//	    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
//	    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
//	    PRIMARY KEY (tenant_id, bucket, object_key_hash, version_id)
//	);
//	CREATE INDEX IF NOT EXISTS manifests_by_tenant_bucket
//	    ON manifests (tenant_id, bucket, object_key_hash);
//
// The manifest body is stored as opaque JSONB. The control plane is
// not expected to peek at it except for forensic use; indexing lives
// on the explicit columns.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
)

// Config is the store's wiring. Table defaults to "manifests".
type Config struct {
	DB    *sql.DB
	Table string
	// BodyEncryptor, when non-nil, seals the manifest JSON before
	// INSERT and opens it after SELECT. The column in the schema
	// must be BYTEA in that case (JSONB rejects opaque bytes).
	// Leaving this nil preserves the Phase 2 JSONB layout.
	BodyEncryptor BodyEncryptor
}

// Store is a manifest_store.ManifestStore backed by a Postgres table.
type Store struct {
	db        *sql.DB
	table     string
	encryptor BodyEncryptor
}

// New returns a Store. It does not open or verify the database
// connection; callers should have already pinged the pool.
func New(cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, errors.New("postgres: Config.DB is required")
	}
	table := cfg.Table
	if table == "" {
		table = "manifests"
	}
	if !isSafeIdent(table) {
		return nil, fmt.Errorf("postgres: invalid table name %q", table)
	}
	return &Store{db: cfg.DB, table: table, encryptor: cfg.BodyEncryptor}, nil
}

// Put writes or replaces a manifest row. It uses an UPSERT so the
// store is safe to call on the read-path of a dual-write migration.
func (s *Store) Put(ctx context.Context, key manifest_store.ManifestKey, m *metadata.ObjectManifest) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if m == nil {
		return errors.New("postgres: manifest is nil")
	}
	body, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("postgres: marshal manifest: %w", err)
	}
	if s.encryptor != nil {
		sealed, eerr := s.encryptor.Encrypt(body)
		if eerr != nil {
			return fmt.Errorf("postgres: encrypt manifest body: %w", eerr)
		}
		body = sealed
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, bucket, object_key_hash, version_id, body)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, bucket, object_key_hash, version_id)
		DO UPDATE SET body = EXCLUDED.body, updated_at = now()
	`, s.table)
	if _, err := s.db.ExecContext(ctx, q, key.TenantID, key.Bucket, key.ObjectKeyHash, key.VersionID, body); err != nil {
		return fmt.Errorf("postgres: put manifest: %w", err)
	}
	return nil
}

// Get reads a manifest by exact key. If VersionID is empty, Get
// returns the most recently updated version for the (tenant, bucket,
// object_key_hash) triple.
func (s *Store) Get(ctx context.Context, key manifest_store.ManifestKey) (*metadata.ObjectManifest, error) {
	if key.TenantID == "" || key.Bucket == "" || key.ObjectKeyHash == "" {
		return nil, errors.New("postgres: tenant_id, bucket, and object_key_hash are required")
	}
	var row *sql.Row
	if key.VersionID != "" {
		q := fmt.Sprintf(`
			SELECT body FROM %s
			WHERE tenant_id = $1 AND bucket = $2 AND object_key_hash = $3 AND version_id = $4
		`, s.table)
		row = s.db.QueryRowContext(ctx, q, key.TenantID, key.Bucket, key.ObjectKeyHash, key.VersionID)
	} else {
		q := fmt.Sprintf(`
			SELECT body FROM %s
			WHERE tenant_id = $1 AND bucket = $2 AND object_key_hash = $3
			ORDER BY updated_at DESC
			LIMIT 1
		`, s.table)
		row = s.db.QueryRowContext(ctx, q, key.TenantID, key.Bucket, key.ObjectKeyHash)
	}

	var body []byte
	switch err := row.Scan(&body); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, manifest_store.ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("postgres: get manifest: %w", err)
	}
	if s.encryptor != nil {
		opened, derr := s.encryptor.Decrypt(body)
		if derr != nil {
			return nil, fmt.Errorf("postgres: decrypt manifest body: %w", derr)
		}
		body = opened
	}
	var m metadata.ObjectManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal manifest: %w", err)
	}
	return &m, nil
}

// Delete removes the manifest row.
func (s *Store) Delete(ctx context.Context, key manifest_store.ManifestKey) error {
	if err := validateKey(key); err != nil {
		return err
	}
	q := fmt.Sprintf(`
		DELETE FROM %s
		WHERE tenant_id = $1 AND bucket = $2 AND object_key_hash = $3 AND version_id = $4
	`, s.table)
	res, err := s.db.ExecContext(ctx, q, key.TenantID, key.Bucket, key.ObjectKeyHash, key.VersionID)
	if err != nil {
		return fmt.Errorf("postgres: delete manifest: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: delete rows-affected: %w", err)
	}
	if n == 0 {
		return manifest_store.ErrNotFound
	}
	return nil
}

// List paginates manifests under (tenantID, bucket). The cursor is
// the last object_key_hash observed, encoded verbatim. Pages are
// ordered by object_key_hash so the cursor is a stable keyset.
//
// Only the latest version of each object_key_hash is returned —
// older versions created by overwrite PUTs remain addressable by
// explicit VersionID via Get, but do not appear in LIST, matching
// S3 ListObjectsV2 semantics.
func (s *Store) List(ctx context.Context, tenantID, bucket, cursor string, limit int) (manifest_store.ListResult, error) {
	if tenantID == "" || bucket == "" {
		return manifest_store.ListResult{}, errors.New("postgres: tenant_id and bucket are required")
	}
	if limit <= 0 {
		limit = 1000
	}
	cursorHash, _ := splitCursor(cursor)

	q := fmt.Sprintf(`
		SELECT DISTINCT ON (object_key_hash) object_key_hash, version_id, body
		FROM %s
		WHERE tenant_id = $1 AND bucket = $2
		  AND object_key_hash > $3
		ORDER BY object_key_hash, updated_at DESC
		LIMIT $4
	`, s.table)
	rows, err := s.db.QueryContext(ctx, q, tenantID, bucket, cursorHash, limit+1)
	if err != nil {
		return manifest_store.ListResult{}, fmt.Errorf("postgres: list manifests: %w", err)
	}
	defer rows.Close()

	out := manifest_store.ListResult{}
	var lastHash, lastVersion string
	count := 0
	for rows.Next() {
		count++
		if count > limit {
			out.NextCursor = joinCursor(lastHash, lastVersion)
			break
		}
		var (
			hash    string
			version string
			body    []byte
		)
		if err := rows.Scan(&hash, &version, &body); err != nil {
			return manifest_store.ListResult{}, fmt.Errorf("postgres: scan manifest: %w", err)
		}
		if s.encryptor != nil {
			opened, derr := s.encryptor.Decrypt(body)
			if derr != nil {
				return manifest_store.ListResult{}, fmt.Errorf("postgres: decrypt manifest body: %w", derr)
			}
			body = opened
		}
		var m metadata.ObjectManifest
		if err := json.Unmarshal(body, &m); err != nil {
			return manifest_store.ListResult{}, fmt.Errorf("postgres: unmarshal manifest: %w", err)
		}
		out.Manifests = append(out.Manifests, &m)
		lastHash, lastVersion = hash, version
	}
	if err := rows.Err(); err != nil {
		return manifest_store.ListResult{}, fmt.Errorf("postgres: list iter: %w", err)
	}
	return out, nil
}

func validateKey(key manifest_store.ManifestKey) error {
	if key.TenantID == "" {
		return errors.New("postgres: tenant_id is required")
	}
	if key.Bucket == "" {
		return errors.New("postgres: bucket is required")
	}
	if key.ObjectKeyHash == "" {
		return errors.New("postgres: object_key_hash is required")
	}
	if key.VersionID == "" {
		return errors.New("postgres: version_id is required")
	}
	return nil
}

// isSafeIdent validates that s is a plausible SQL identifier: ASCII
// letters, digits, and underscore only. This is a tighter check than
// Postgres allows, but it keeps the table name safe for fmt.Sprintf
// interpolation without pulling in a full quoting routine.
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

func splitCursor(c string) (string, string) {
	for i := 0; i < len(c); i++ {
		if c[i] == '/' {
			return c[:i], c[i+1:]
		}
	}
	return c, ""
}

func joinCursor(hash, version string) string {
	return hash + "/" + version
}

var _ manifest_store.ManifestStore = (*Store)(nil)
