// Package postgres is the Postgres-backed implementation of the
// intra-tenant content_index Store. See docs/PROPOSAL.md §3.14 and
// metadata/content_index/schema.sql for the table definition.
//
// The implementation uses database/sql; the driver is the caller's
// responsibility (the gateway binary registers github.com/lib/pq).
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
)

// Config is the store wiring. Table defaults to "content_index".
type Config struct {
	DB    *sql.DB
	Table string
}

// Store is a content_index.Store backed by a Postgres table.
type Store struct {
	db    *sql.DB
	table string
}

// New returns a Store. It does not open or verify the connection;
// callers should have already pinged the pool.
func New(cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, errors.New("postgres: Config.DB is required")
	}
	table := cfg.Table
	if table == "" {
		table = "content_index"
	}
	if !isSafeIdent(table) {
		return nil, fmt.Errorf("postgres: invalid table name %q", table)
	}
	return &Store{db: cfg.DB, table: table}, nil
}

// Lookup returns the entry for (tenantID, contentHash) or ErrNotFound.
func (s *Store) Lookup(ctx context.Context, tenantID, contentHash string) (*content_index.ContentIndexEntry, error) {
	if tenantID == "" || contentHash == "" {
		return nil, errors.New("postgres: tenant_id and content_hash are required")
	}
	// etag and plaintext_hash columns may be NULL on rows written
	// before they were added; coalesce to empty string so the
	// caller sees a uniform zero value. piece_ids is the
	// multi-piece extension and is NULL for single-piece entries;
	// we scan it into a []byte and unmarshal only when non-NULL.
	q := fmt.Sprintf(`
		SELECT tenant_id, content_hash, piece_id, backend, ref_count, size_bytes, COALESCE(etag, ''), piece_ids, COALESCE(plaintext_hash, ''), created_at
		FROM %s
		WHERE tenant_id = $1 AND content_hash = $2
	`, s.table)
	tx, err := s.beginTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, q, tenantID, contentHash)
	return scanEntry(row)
}

// LookupByPlaintextHash returns the entry for (tenantID,
// plaintextHash) using the plaintext_hash secondary index.
// Returns ErrNotFound when no matching row exists. Pattern C and
// pre-Phase-3.5 rows are excluded automatically by the partial
// index — empty plaintext_hash values never match.
func (s *Store) LookupByPlaintextHash(ctx context.Context, tenantID, plaintextHash string) (*content_index.ContentIndexEntry, error) {
	if tenantID == "" {
		return nil, errors.New("postgres: tenant_id is required")
	}
	if plaintextHash == "" {
		return nil, content_index.ErrNotFound
	}
	q := fmt.Sprintf(`
		SELECT tenant_id, content_hash, piece_id, backend, ref_count, size_bytes, COALESCE(etag, ''), piece_ids, COALESCE(plaintext_hash, ''), created_at
		FROM %s
		WHERE tenant_id = $1 AND plaintext_hash = $2
		LIMIT 1
	`, s.table)
	tx, err := s.beginTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, q, tenantID, plaintextHash)
	return scanEntry(row)
}

// scanEntry pulls a content_index row off either a *sql.Row or
// *sql.Rows. Centralised so Lookup, LookupByPlaintextHash, and the
// row-iterating callers stay consistent on column order and
// piece_ids unmarshaling.
func scanEntry(row interface {
	Scan(dest ...any) error
}) (*content_index.ContentIndexEntry, error) {
	var e content_index.ContentIndexEntry
	var pieceIDsRaw []byte
	if err := row.Scan(&e.TenantID, &e.ContentHash, &e.PieceID, &e.Backend, &e.RefCount, &e.SizeBytes, &e.ETag, &pieceIDsRaw, &e.PlaintextHash, &e.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, content_index.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: content_index lookup: %w", err)
	}
	if len(pieceIDsRaw) > 0 {
		if err := json.Unmarshal(pieceIDsRaw, &e.PieceIDs); err != nil {
			return nil, fmt.Errorf("postgres: content_index lookup unmarshal piece_ids: %w", err)
		}
	}
	return &e, nil
}

// Register inserts a new entry with RefCount = 1. Uses INSERT ...
// ON CONFLICT DO NOTHING and inspects RowsAffected so a concurrent
// inserter racing two PUTs of the same (tenant, content_hash)
// produces ErrAlreadyExists on exactly one path.
func (s *Store) Register(ctx context.Context, entry content_index.ContentIndexEntry) error {
	if entry.TenantID == "" || entry.ContentHash == "" {
		return errors.New("postgres: tenant_id and content_hash are required")
	}
	if entry.PieceID == "" {
		return errors.New("postgres: piece_id is required")
	}
	if entry.Backend == "" {
		return errors.New("postgres: backend is required")
	}
	// Marshal piece_ids when present. Single-piece entries leave
	// it nil; the column accepts a SQL NULL via a typed-nil
	// []byte so we don't need a separate INSERT branch.
	var pieceIDsJSON []byte
	if len(entry.PieceIDs) > 0 {
		var err error
		pieceIDsJSON, err = json.Marshal(entry.PieceIDs)
		if err != nil {
			return fmt.Errorf("postgres: content_index register marshal piece_ids: %w", err)
		}
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, content_hash, piece_id, backend, ref_count, size_bytes, etag, piece_ids, plaintext_hash)
		VALUES ($1, $2, $3, $4, 1, $5, NULLIF($6, ''), $7, NULLIF($8, ''))
		ON CONFLICT (tenant_id, content_hash) DO NOTHING
	`, s.table)
	tx, err := s.beginTenant(ctx, entry.TenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, q, entry.TenantID, entry.ContentHash, entry.PieceID, entry.Backend, entry.SizeBytes, entry.ETag, nullableJSON(pieceIDsJSON), entry.PlaintextHash)
	if err != nil {
		return fmt.Errorf("postgres: content_index register: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: content_index register rows affected: %w", err)
	}
	if n == 0 {
		return content_index.ErrAlreadyExists
	}
	return tx.Commit()
}

// IncrementRef atomically bumps RefCount on an existing row.
func (s *Store) IncrementRef(ctx context.Context, tenantID, contentHash string) error {
	if tenantID == "" || contentHash == "" {
		return errors.New("postgres: tenant_id and content_hash are required")
	}
	q := fmt.Sprintf(`
		UPDATE %s
		SET ref_count = ref_count + 1
		WHERE tenant_id = $1 AND content_hash = $2
	`, s.table)
	tx, err := s.beginTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, q, tenantID, contentHash)
	if err != nil {
		return fmt.Errorf("postgres: content_index increment: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: content_index increment rows affected: %w", err)
	}
	if n == 0 {
		return content_index.ErrNotFound
	}
	return tx.Commit()
}

// DecrementRef atomically decrements RefCount and returns the new
// count. Uses RETURNING so the read is in the same SQL statement
// as the write — no surrounding transaction needed.
func (s *Store) DecrementRef(ctx context.Context, tenantID, contentHash string) (int, error) {
	if tenantID == "" || contentHash == "" {
		return 0, errors.New("postgres: tenant_id and content_hash are required")
	}
	// The CHECK constraint on ref_count >= 0 fires on the UPDATE
	// when the existing count is already 0. We translate that
	// into ErrInvalidRefCount so callers get a typed error.
	q := fmt.Sprintf(`
		UPDATE %s
		SET ref_count = ref_count - 1
		WHERE tenant_id = $1 AND content_hash = $2
		RETURNING ref_count
	`, s.table)
	tx, err := s.beginTenant(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var newCount int
	if err := tx.QueryRowContext(ctx, q, tenantID, contentHash).Scan(&newCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, content_index.ErrNotFound
		}
		// Postgres error codes: 23514 = check_violation. We
		// avoid pulling lib/pq's pgconn type into this package
		// by string-matching the error's SQLSTATE-bearing
		// surface; the manifest_store/postgres package follows
		// the same convention.
		if isCheckViolation(err) {
			return 0, content_index.ErrInvalidRefCount
		}
		return 0, fmt.Errorf("postgres: content_index decrement: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("postgres: content_index decrement commit: %w", err)
	}
	return newCount, nil
}

// Delete removes the row for (tenantID, contentHash) only when
// ref_count is zero. The DELETE is conditional on ref_count = 0 so
// a concurrent PUT that IncrementRef'd between our caller's
// DecrementRef returning 0 and this Delete is preserved. We then
// disambiguate "row gone" from "row bumped" with a follow-up
// SELECT so the caller can choose to skip the backend piece
// deletion in the bumped case.
func (s *Store) Delete(ctx context.Context, tenantID, contentHash string) error {
	if tenantID == "" || contentHash == "" {
		return errors.New("postgres: tenant_id and content_hash are required")
	}
	tx, err := s.beginTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := fmt.Sprintf(`
		DELETE FROM %s WHERE tenant_id = $1 AND content_hash = $2 AND ref_count = 0
	`, s.table)
	res, err := tx.ExecContext(ctx, q, tenantID, contentHash)
	if err != nil {
		return fmt.Errorf("postgres: content_index delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: content_index delete rows affected: %w", err)
	}
	if n > 0 {
		return tx.Commit()
	}
	// Row was either gone already or had a non-zero ref_count.
	// Cheap probe to surface the difference; it runs in the same
	// tenant-bound transaction so RLS admits the row.
	probe := fmt.Sprintf(`SELECT 1 FROM %s WHERE tenant_id = $1 AND content_hash = $2`, s.table)
	var exists int
	if err := tx.QueryRowContext(ctx, probe, tenantID, contentHash).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return content_index.ErrNotFound
		}
		return fmt.Errorf("postgres: content_index delete probe: %w", err)
	}
	return content_index.ErrRefCountNonZero
}

// ScanAll returns every content_index row for the given tenant.
// The orphan GC sweep uses this to walk all live entries; the
// caller drives a separate manifest probe per row.
func (s *Store) ScanAll(ctx context.Context, tenantID string) ([]content_index.ContentIndexEntry, error) {
	if tenantID == "" {
		return nil, errors.New("postgres: tenant_id is required")
	}
	q := fmt.Sprintf(`
		SELECT tenant_id, content_hash, piece_id, backend, ref_count, size_bytes, COALESCE(etag, ''), piece_ids, COALESCE(plaintext_hash, ''), created_at
		FROM %s
		WHERE tenant_id = $1
	`, s.table)
	tx, err := s.beginTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: content_index scan: %w", err)
	}
	defer rows.Close()
	out := make([]content_index.ContentIndexEntry, 0)
	for rows.Next() {
		var e content_index.ContentIndexEntry
		var pieceIDsRaw []byte
		if err := rows.Scan(&e.TenantID, &e.ContentHash, &e.PieceID, &e.Backend, &e.RefCount, &e.SizeBytes, &e.ETag, &pieceIDsRaw, &e.PlaintextHash, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: content_index scan row: %w", err)
		}
		if len(pieceIDsRaw) > 0 {
			if err := json.Unmarshal(pieceIDsRaw, &e.PieceIDs); err != nil {
				return nil, fmt.Errorf("postgres: content_index scan unmarshal piece_ids: %w", err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: content_index scan iter: %w", err)
	}
	return out, nil
}

// ListTenants returns the distinct tenant_ids that have at least
// one content_index row. Used by the orphan GC sweep to enumerate
// per-tenant work.
func (s *Store) ListTenants(ctx context.Context) ([]string, error) {
	q := fmt.Sprintf(`SELECT DISTINCT tenant_id FROM %s ORDER BY tenant_id`, s.table)
	// ListTenants is the audited cross-tenant enumerator for the orphan
	// GC sweep, so it binds the scan_all GUC rather than a single tenant.
	tx, err := s.beginScanAll(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("postgres: content_index list tenants: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("postgres: content_index list tenants row: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: content_index list tenants iter: %w", err)
	}
	return out, nil
}

// nullableJSON returns nil for an empty/nil byte slice so the
// JSONB column receives a SQL NULL on INSERT. lib/pq treats a
// typed-nil []byte as NULL but a non-nil empty slice as the
// literal string "" which Postgres rejects for JSONB; this helper
// makes the intent explicit.
func nullableJSON(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	return b
}

// isCheckViolation reports whether err looks like a Postgres CHECK
// constraint failure. It does string matching to avoid importing a
// concrete driver type.
func isCheckViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// "pq: new row for relation ... violates check constraint"
	// "ERROR: ... (SQLSTATE 23514)"
	for _, needle := range []string{"check constraint", "23514"} {
		if containsFold(msg, needle) {
			return true
		}
	}
	return false
}

func containsFold(s, needle string) bool {
	if len(needle) > len(s) {
		return false
	}
	for i := 0; i+len(needle) <= len(s); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			a := s[i+j]
			b := needle[j]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}
			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// isSafeIdent validates that s is a plausible SQL identifier:
// ASCII letters, digits, and underscore only.
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

var _ content_index.Store = (*Store)(nil)
