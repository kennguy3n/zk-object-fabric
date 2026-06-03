// Package sqlite is the SQLite-backed ManifestStore used by the
// embedded / single-node deployment profile (docker compose up with
// no Postgres). It persists manifests to a local database file so
// dev and demo deployments get durable state without standing up a
// control-plane Postgres.
//
// The store mirrors the contract of the Postgres store
// (metadata/manifest_store/postgres) and the in-memory store
// (metadata/manifest_store/memory): a composite key of
// (tenant_id, bucket, object_key_hash, version_id), latest-version
// resolution on empty VersionID, and LIST returning only the latest
// version of each object_key_hash (S3 ListObjectsV2 semantics).
//
// LIST / latest-version ordering uses a monotonic write_seq column
// bumped on every Put, so the most recently written version of a
// key is "latest" and the LIST cursor is a stable keyset. This
// matches the in-memory store's insertion-order semantics rather
// than the Postgres store's object_key_hash keyset; both satisfy
// the opaque-cursor contract.
//
// Concurrency: a single open connection (see internal/embeddeddb)
// serialises individual statements, but database/sql still returns
// the connection to the pool between calls, so a multi-statement
// read-modify-write is NOT implicitly atomic even on this pool. Each
// mutating operation is therefore expressed as a single SQL
// statement: Put is one INSERT ... ON CONFLICT DO UPDATE whose
// write_seq is computed inline (MAX(write_seq)+1) and reused via
// excluded.write_seq, and Delete (including the empty-VersionID
// latest-version case) is one DELETE that resolves the target row in
// a subquery. No operation reads a value in one statement and writes
// it back in another, so there is no resolve-then-act window to race.
package sqlite

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
	// INSERT and opens it after SELECT. Leaving it nil stores the
	// manifest body as plaintext JSON.
	BodyEncryptor manifest_store.BodyEncryptor
}

// Store is a manifest_store.ManifestStore backed by a SQLite table.
type Store struct {
	db        *sql.DB
	table     string
	encryptor manifest_store.BodyEncryptor
}

// New returns a Store and creates the backing table if it does not
// yet exist. The embedded profile has no separate migration step,
// so the store owns its schema.
func New(cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, errors.New("sqlite: Config.DB is required")
	}
	table := cfg.Table
	if table == "" {
		table = "manifests"
	}
	if !isSafeIdent(table) {
		return nil, fmt.Errorf("sqlite: invalid table name %q", table)
	}
	s := &Store{db: cfg.DB, table: table, encryptor: cfg.BodyEncryptor}
	if err := s.ensureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureSchema(ctx context.Context) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			tenant_id        TEXT    NOT NULL,
			bucket           TEXT    NOT NULL,
			object_key_hash  TEXT    NOT NULL,
			version_id       TEXT    NOT NULL,
			body             BLOB    NOT NULL,
			write_seq        INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, bucket, object_key_hash, version_id)
		)`, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_by_tenant_bucket
			ON %s (tenant_id, bucket, object_key_hash, write_seq)`, s.table, s.table),
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("sqlite: ensure manifest schema: %w", err)
		}
	}
	return nil
}

// Put writes or replaces a manifest row. The write_seq is bumped to
// MAX(write_seq)+1 so the row becomes the latest version of its key,
// matching the in-memory store where every Put (including an
// overwrite of the same version) re-stamps insertion order.
func (s *Store) Put(ctx context.Context, key manifest_store.ManifestKey, m *metadata.ObjectManifest) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if m == nil {
		return errors.New("sqlite: manifest is nil")
	}
	body, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("sqlite: marshal manifest: %w", err)
	}
	if s.encryptor != nil {
		sealed, eerr := s.encryptor.Encrypt(body, manifest_store.BodyContext{
			TenantID:      key.TenantID,
			Bucket:        key.Bucket,
			ObjectKeyHash: key.ObjectKeyHash,
		})
		if eerr != nil {
			return fmt.Errorf("sqlite: encrypt manifest body: %w", eerr)
		}
		body = sealed
	}
	// write_seq is computed once in the INSERT's VALUES clause; the
	// conflict path reuses that same value via excluded.write_seq
	// rather than re-evaluating the MAX(write_seq)+1 subquery (the
	// row being upserted does not change MAX, so both forms yield an
	// identical value).
	q := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, bucket, object_key_hash, version_id, body, write_seq)
		VALUES (?, ?, ?, ?, ?, (SELECT COALESCE(MAX(write_seq), 0) + 1 FROM %s))
		ON CONFLICT (tenant_id, bucket, object_key_hash, version_id)
		DO UPDATE SET body = excluded.body,
		              write_seq = excluded.write_seq
	`, s.table, s.table)
	if _, err := s.db.ExecContext(ctx, q, key.TenantID, key.Bucket, key.ObjectKeyHash, key.VersionID, body); err != nil {
		return fmt.Errorf("sqlite: put manifest: %w", err)
	}
	return nil
}

// Get reads a manifest by exact key. If VersionID is empty, Get
// returns the most recently written version for the (tenant,
// bucket, object_key_hash) triple.
func (s *Store) Get(ctx context.Context, key manifest_store.ManifestKey) (*metadata.ObjectManifest, error) {
	if key.TenantID == "" || key.Bucket == "" || key.ObjectKeyHash == "" {
		return nil, errors.New("sqlite: tenant_id, bucket, and object_key_hash are required")
	}
	var row *sql.Row
	if key.VersionID != "" {
		q := fmt.Sprintf(`
			SELECT body FROM %s
			WHERE tenant_id = ? AND bucket = ? AND object_key_hash = ? AND version_id = ?
		`, s.table)
		row = s.db.QueryRowContext(ctx, q, key.TenantID, key.Bucket, key.ObjectKeyHash, key.VersionID)
	} else {
		q := fmt.Sprintf(`
			SELECT body FROM %s
			WHERE tenant_id = ? AND bucket = ? AND object_key_hash = ?
			ORDER BY write_seq DESC
			LIMIT 1
		`, s.table)
		row = s.db.QueryRowContext(ctx, q, key.TenantID, key.Bucket, key.ObjectKeyHash)
	}

	var body []byte
	switch err := row.Scan(&body); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, manifest_store.ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("sqlite: get manifest: %w", err)
	}
	return s.decodeBody(body, key.TenantID, key.Bucket, key.ObjectKeyHash)
}

// Delete removes the manifest row. An explicit VersionID deletes
// that exact version; an empty VersionID deletes the latest
// version. The latest-version case resolves and deletes in a single
// statement (DELETE ... WHERE rowid = (SELECT ... ORDER BY write_seq
// DESC LIMIT 1)) rather than a SELECT followed by a DELETE, so a
// concurrent Put/Delete cannot shift which version is latest between
// the read and the write — database/sql returns the connection to
// the pool between calls even on the single-connection embedded
// pool, so a two-statement resolve-then-delete would not be atomic.
// RowsAffected == 0 means no matching row existed and maps to
// ErrNotFound.
func (s *Store) Delete(ctx context.Context, key manifest_store.ManifestKey) error {
	if key.TenantID == "" || key.Bucket == "" || key.ObjectKeyHash == "" {
		return errors.New("sqlite: tenant_id, bucket, and object_key_hash are required")
	}
	var (
		q    string
		args []any
	)
	if key.VersionID == "" {
		q = fmt.Sprintf(`
			DELETE FROM %s
			WHERE rowid = (
				SELECT rowid FROM %s
				WHERE tenant_id = ? AND bucket = ? AND object_key_hash = ?
				ORDER BY write_seq DESC
				LIMIT 1
			)
		`, s.table, s.table)
		args = []any{key.TenantID, key.Bucket, key.ObjectKeyHash}
	} else {
		q = fmt.Sprintf(`
			DELETE FROM %s
			WHERE tenant_id = ? AND bucket = ? AND object_key_hash = ? AND version_id = ?
		`, s.table)
		args = []any{key.TenantID, key.Bucket, key.ObjectKeyHash, key.VersionID}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("sqlite: delete manifest: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: delete rows-affected: %w", err)
	}
	if n == 0 {
		return manifest_store.ErrNotFound
	}
	return nil
}

// List paginates manifests under (tenantID, bucket). Only the
// latest version of each object_key_hash is returned. The cursor is
// the decimal write_seq of the last returned manifest's latest
// version; pages are ordered by that write_seq so the cursor is a
// stable keyset.
func (s *Store) List(ctx context.Context, tenantID, bucket, cursor string, limit int) (manifest_store.ListResult, error) {
	if tenantID == "" || bucket == "" {
		return manifest_store.ListResult{}, errors.New("sqlite: tenant_id and bucket are required")
	}
	if limit <= 0 {
		limit = 1000
	}
	var after int64
	if cursor != "" {
		n, err := parseCursor(cursor)
		if err != nil {
			return manifest_store.ListResult{}, err
		}
		after = n
	}
	// latest_seq is the highest write_seq per object_key_hash; the
	// outer query selects rows at that write_seq, paginates with
	// write_seq > cursor, and orders by write_seq for a stable
	// keyset cursor.
	q := fmt.Sprintf(`
		SELECT m.object_key_hash, m.body, m.write_seq
		FROM %s AS m
		WHERE m.tenant_id = ? AND m.bucket = ?
		  AND m.write_seq = (
			SELECT MAX(m2.write_seq) FROM %s AS m2
			WHERE m2.tenant_id = m.tenant_id
			  AND m2.bucket = m.bucket
			  AND m2.object_key_hash = m.object_key_hash
		  )
		  AND m.write_seq > ?
		ORDER BY m.write_seq ASC
		LIMIT ?
	`, s.table, s.table)
	rows, err := s.db.QueryContext(ctx, q, tenantID, bucket, after, limit+1)
	if err != nil {
		return manifest_store.ListResult{}, fmt.Errorf("sqlite: list manifests: %w", err)
	}
	defer rows.Close()

	out := manifest_store.ListResult{}
	count := 0
	var lastSeq int64
	for rows.Next() {
		count++
		if count > limit {
			out.NextCursor = formatCursor(lastSeq)
			break
		}
		var (
			hash string
			body []byte
			seq  int64
		)
		if err := rows.Scan(&hash, &body, &seq); err != nil {
			return manifest_store.ListResult{}, fmt.Errorf("sqlite: scan manifest: %w", err)
		}
		m, derr := s.decodeBody(body, tenantID, bucket, hash)
		if derr != nil {
			return manifest_store.ListResult{}, derr
		}
		out.Manifests = append(out.Manifests, m)
		lastSeq = seq
	}
	if err := rows.Err(); err != nil {
		return manifest_store.ListResult{}, fmt.Errorf("sqlite: list iter: %w", err)
	}
	return out, nil
}

// HasManifestWithPieceID reports whether the given tenant has at
// least one manifest whose pieces[].piece_id equals pieceID. Used
// by the orphan GC worker. The body may be opaque ciphertext when a
// BodyEncryptor is configured, so this scans and decodes the
// tenant's manifests rather than relying on a JSON query.
func (s *Store) HasManifestWithPieceID(ctx context.Context, tenantID, pieceID string) (bool, error) {
	if tenantID == "" || pieceID == "" {
		return false, errors.New("sqlite: tenant_id and piece_id are required")
	}
	q := fmt.Sprintf(`SELECT bucket, object_key_hash, body FROM %s WHERE tenant_id = ?`, s.table)
	rows, err := s.db.QueryContext(ctx, q, tenantID)
	if err != nil {
		return false, fmt.Errorf("sqlite: has manifest with piece_id: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			bucket string
			hash   string
			body   []byte
		)
		if err := rows.Scan(&bucket, &hash, &body); err != nil {
			return false, fmt.Errorf("sqlite: has manifest scan: %w", err)
		}
		m, derr := s.decodeBody(body, tenantID, bucket, hash)
		if derr != nil {
			return false, derr
		}
		for _, p := range m.Pieces {
			if p.PieceID == pieceID {
				return true, nil
			}
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("sqlite: has manifest iter: %w", err)
	}
	return false, nil
}

// ListVersions returns every version of (tenantID, bucket,
// objectKeyHash), most-recently-written first.
func (s *Store) ListVersions(ctx context.Context, tenantID, bucket, objectKeyHash string) ([]*metadata.ObjectManifest, error) {
	if tenantID == "" || bucket == "" || objectKeyHash == "" {
		return nil, errors.New("sqlite: tenant_id, bucket, and object_key_hash are required")
	}
	q := fmt.Sprintf(`
		SELECT body FROM %s
		WHERE tenant_id = ? AND bucket = ? AND object_key_hash = ?
		ORDER BY write_seq DESC
	`, s.table)
	rows, err := s.db.QueryContext(ctx, q, tenantID, bucket, objectKeyHash)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list versions: %w", err)
	}
	defer rows.Close()
	out := make([]*metadata.ObjectManifest, 0)
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, fmt.Errorf("sqlite: list versions scan: %w", err)
		}
		m, derr := s.decodeBody(body, tenantID, bucket, objectKeyHash)
		if derr != nil {
			return nil, derr
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list versions iter: %w", err)
	}
	return out, nil
}

// decodeBody opens the stored body (decrypting when an encryptor is
// configured) and unmarshals the manifest JSON.
func (s *Store) decodeBody(body []byte, tenantID, bucket, objectKeyHash string) (*metadata.ObjectManifest, error) {
	if s.encryptor != nil {
		opened, derr := s.encryptor.Decrypt(body, manifest_store.BodyContext{
			TenantID:      tenantID,
			Bucket:        bucket,
			ObjectKeyHash: objectKeyHash,
		})
		if derr != nil {
			return nil, fmt.Errorf("sqlite: decrypt manifest body: %w", derr)
		}
		body = opened
	}
	var m metadata.ObjectManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("sqlite: unmarshal manifest: %w", err)
	}
	return &m, nil
}

var _ manifest_store.ManifestStore = (*Store)(nil)
