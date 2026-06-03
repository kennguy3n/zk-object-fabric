// Package sqlite is the SQLite-backed implementation of the
// intra-tenant content_index Store, used by the embedded /
// single-node deployment profile (docker compose up with no
// Postgres). It mirrors the contract of the Postgres store
// (metadata/content_index/postgres): a (tenant_id, content_hash)
// primary key, ref-counting with a CHECK(ref_count >= 0) guard, and
// the same typed sentinel errors (ErrNotFound, ErrAlreadyExists,
// ErrInvalidRefCount, ErrRefCountNonZero).
//
// Concurrency: the embedded DB is opened with a single connection
// (see internal/embeddeddb), so the read-modify-write sequences here
// (DecrementRef's select-then-update, Delete's delete-then-probe)
// execute without interleaving and need no explicit transaction.
package sqlite

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

// Store is a content_index.Store backed by a SQLite table.
type Store struct {
	db    *sql.DB
	table string
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
		table = "content_index"
	}
	if !isSafeIdent(table) {
		return nil, fmt.Errorf("sqlite: invalid table name %q", table)
	}
	s := &Store{db: cfg.DB, table: table}
	if err := s.ensureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureSchema(ctx context.Context) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			tenant_id      TEXT    NOT NULL,
			content_hash   TEXT    NOT NULL,
			piece_id       TEXT    NOT NULL,
			backend        TEXT    NOT NULL,
			ref_count      INTEGER NOT NULL DEFAULT 1 CHECK (ref_count >= 0),
			size_bytes     INTEGER NOT NULL DEFAULT 0,
			etag           TEXT,
			piece_ids      TEXT,
			plaintext_hash TEXT,
			created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (tenant_id, content_hash)
		)`, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_piece_id ON %s (piece_id)`, s.table, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_plaintext_hash
			ON %s (tenant_id, plaintext_hash) WHERE plaintext_hash IS NOT NULL`, s.table, s.table),
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("sqlite: ensure content_index schema: %w", err)
		}
	}
	return nil
}

const selectColumns = `tenant_id, content_hash, piece_id, backend, ref_count, size_bytes, COALESCE(etag, ''), piece_ids, COALESCE(plaintext_hash, ''), created_at`

// Lookup returns the entry for (tenantID, contentHash) or ErrNotFound.
func (s *Store) Lookup(ctx context.Context, tenantID, contentHash string) (*content_index.ContentIndexEntry, error) {
	if tenantID == "" || contentHash == "" {
		return nil, errors.New("sqlite: tenant_id and content_hash are required")
	}
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id = ? AND content_hash = ?`, selectColumns, s.table)
	return scanEntry(s.db.QueryRowContext(ctx, q, tenantID, contentHash))
}

// LookupByPlaintextHash returns the entry for (tenantID,
// plaintextHash) using the plaintext_hash secondary index.
func (s *Store) LookupByPlaintextHash(ctx context.Context, tenantID, plaintextHash string) (*content_index.ContentIndexEntry, error) {
	if tenantID == "" {
		return nil, errors.New("sqlite: tenant_id is required")
	}
	if plaintextHash == "" {
		return nil, content_index.ErrNotFound
	}
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id = ? AND plaintext_hash = ? LIMIT 1`, selectColumns, s.table)
	return scanEntry(s.db.QueryRowContext(ctx, q, tenantID, plaintextHash))
}

func scanEntry(row interface {
	Scan(dest ...any) error
}) (*content_index.ContentIndexEntry, error) {
	var e content_index.ContentIndexEntry
	var pieceIDsRaw []byte
	if err := row.Scan(&e.TenantID, &e.ContentHash, &e.PieceID, &e.Backend, &e.RefCount, &e.SizeBytes, &e.ETag, &pieceIDsRaw, &e.PlaintextHash, &e.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, content_index.ErrNotFound
		}
		return nil, fmt.Errorf("sqlite: content_index lookup: %w", err)
	}
	if len(pieceIDsRaw) > 0 {
		if err := json.Unmarshal(pieceIDsRaw, &e.PieceIDs); err != nil {
			return nil, fmt.Errorf("sqlite: content_index lookup unmarshal piece_ids: %w", err)
		}
	}
	return &e, nil
}

// Register inserts a new entry with RefCount = 1. A conflicting row
// (same tenant_id, content_hash) leaves the existing row untouched
// and returns ErrAlreadyExists so the PUT path can fall back to
// IncrementRef.
func (s *Store) Register(ctx context.Context, entry content_index.ContentIndexEntry) error {
	if entry.TenantID == "" || entry.ContentHash == "" {
		return errors.New("sqlite: tenant_id and content_hash are required")
	}
	if entry.PieceID == "" {
		return errors.New("sqlite: piece_id is required")
	}
	if entry.Backend == "" {
		return errors.New("sqlite: backend is required")
	}
	var pieceIDsJSON []byte
	if len(entry.PieceIDs) > 0 {
		var err error
		pieceIDsJSON, err = json.Marshal(entry.PieceIDs)
		if err != nil {
			return fmt.Errorf("sqlite: content_index register marshal piece_ids: %w", err)
		}
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, content_hash, piece_id, backend, ref_count, size_bytes, etag, piece_ids, plaintext_hash)
		VALUES (?, ?, ?, ?, 1, ?, NULLIF(?, ''), ?, NULLIF(?, ''))
		ON CONFLICT (tenant_id, content_hash) DO NOTHING
	`, s.table)
	res, err := s.db.ExecContext(ctx, q, entry.TenantID, entry.ContentHash, entry.PieceID, entry.Backend, entry.SizeBytes, entry.ETag, nullableJSON(pieceIDsJSON), entry.PlaintextHash)
	if err != nil {
		return fmt.Errorf("sqlite: content_index register: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: content_index register rows affected: %w", err)
	}
	if n == 0 {
		return content_index.ErrAlreadyExists
	}
	return nil
}

// IncrementRef atomically bumps RefCount on an existing row.
func (s *Store) IncrementRef(ctx context.Context, tenantID, contentHash string) error {
	if tenantID == "" || contentHash == "" {
		return errors.New("sqlite: tenant_id and content_hash are required")
	}
	q := fmt.Sprintf(`UPDATE %s SET ref_count = ref_count + 1 WHERE tenant_id = ? AND content_hash = ?`, s.table)
	res, err := s.db.ExecContext(ctx, q, tenantID, contentHash)
	if err != nil {
		return fmt.Errorf("sqlite: content_index increment: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: content_index increment rows affected: %w", err)
	}
	if n == 0 {
		return content_index.ErrNotFound
	}
	return nil
}

// DecrementRef decrements RefCount and returns the new count. The
// single-connection embedded pool serialises the SELECT + UPDATE so
// no other writer can interleave. A row already at zero returns
// ErrInvalidRefCount (the CHECK constraint would otherwise reject
// the UPDATE); a missing row returns ErrNotFound.
func (s *Store) DecrementRef(ctx context.Context, tenantID, contentHash string) (int, error) {
	if tenantID == "" || contentHash == "" {
		return 0, errors.New("sqlite: tenant_id and content_hash are required")
	}
	var current int
	sel := fmt.Sprintf(`SELECT ref_count FROM %s WHERE tenant_id = ? AND content_hash = ?`, s.table)
	if err := s.db.QueryRowContext(ctx, sel, tenantID, contentHash).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, content_index.ErrNotFound
		}
		return 0, fmt.Errorf("sqlite: content_index decrement select: %w", err)
	}
	if current <= 0 {
		return 0, content_index.ErrInvalidRefCount
	}
	upd := fmt.Sprintf(`UPDATE %s SET ref_count = ref_count - 1 WHERE tenant_id = ? AND content_hash = ?`, s.table)
	if _, err := s.db.ExecContext(ctx, upd, tenantID, contentHash); err != nil {
		return 0, fmt.Errorf("sqlite: content_index decrement: %w", err)
	}
	return current - 1, nil
}

// Delete removes the row for (tenantID, contentHash) only when
// ref_count is zero, mirroring the Postgres store. A row that was
// IncrementRef'd back above zero between the caller's DecrementRef
// and this Delete is preserved and surfaced as ErrRefCountNonZero;
// a missing row returns ErrNotFound.
func (s *Store) Delete(ctx context.Context, tenantID, contentHash string) error {
	if tenantID == "" || contentHash == "" {
		return errors.New("sqlite: tenant_id and content_hash are required")
	}
	q := fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND content_hash = ? AND ref_count = 0`, s.table)
	res, err := s.db.ExecContext(ctx, q, tenantID, contentHash)
	if err != nil {
		return fmt.Errorf("sqlite: content_index delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: content_index delete rows affected: %w", err)
	}
	if n > 0 {
		return nil
	}
	probe := fmt.Sprintf(`SELECT 1 FROM %s WHERE tenant_id = ? AND content_hash = ?`, s.table)
	var exists int
	if err := s.db.QueryRowContext(ctx, probe, tenantID, contentHash).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return content_index.ErrNotFound
		}
		return fmt.Errorf("sqlite: content_index delete probe: %w", err)
	}
	return content_index.ErrRefCountNonZero
}

// ScanAll returns every content_index row for the given tenant.
func (s *Store) ScanAll(ctx context.Context, tenantID string) ([]content_index.ContentIndexEntry, error) {
	if tenantID == "" {
		return nil, errors.New("sqlite: tenant_id is required")
	}
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id = ?`, selectColumns, s.table)
	rows, err := s.db.QueryContext(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: content_index scan: %w", err)
	}
	defer rows.Close()
	out := make([]content_index.ContentIndexEntry, 0)
	for rows.Next() {
		var e content_index.ContentIndexEntry
		var pieceIDsRaw []byte
		if err := rows.Scan(&e.TenantID, &e.ContentHash, &e.PieceID, &e.Backend, &e.RefCount, &e.SizeBytes, &e.ETag, &pieceIDsRaw, &e.PlaintextHash, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: content_index scan row: %w", err)
		}
		if len(pieceIDsRaw) > 0 {
			if err := json.Unmarshal(pieceIDsRaw, &e.PieceIDs); err != nil {
				return nil, fmt.Errorf("sqlite: content_index scan unmarshal piece_ids: %w", err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: content_index scan iter: %w", err)
	}
	return out, nil
}

// ListTenants returns the distinct tenant_ids with at least one row.
func (s *Store) ListTenants(ctx context.Context) ([]string, error) {
	q := fmt.Sprintf(`SELECT DISTINCT tenant_id FROM %s ORDER BY tenant_id`, s.table)
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("sqlite: content_index list tenants: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("sqlite: content_index list tenants row: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: content_index list tenants iter: %w", err)
	}
	return out, nil
}

// nullableJSON returns nil for an empty/nil byte slice so the column
// receives a SQL NULL on INSERT.
func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// isSafeIdent validates that s is a plausible SQL identifier: ASCII
// letters, digits, and underscore only.
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
