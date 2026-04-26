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
	q := fmt.Sprintf(`
		SELECT tenant_id, content_hash, piece_id, backend, ref_count, size_bytes, created_at
		FROM %s
		WHERE tenant_id = $1 AND content_hash = $2
	`, s.table)
	row := s.db.QueryRowContext(ctx, q, tenantID, contentHash)
	var e content_index.ContentIndexEntry
	if err := row.Scan(&e.TenantID, &e.ContentHash, &e.PieceID, &e.Backend, &e.RefCount, &e.SizeBytes, &e.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, content_index.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: content_index lookup: %w", err)
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
	q := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, content_hash, piece_id, backend, ref_count, size_bytes)
		VALUES ($1, $2, $3, $4, 1, $5)
		ON CONFLICT (tenant_id, content_hash) DO NOTHING
	`, s.table)
	res, err := s.db.ExecContext(ctx, q, entry.TenantID, entry.ContentHash, entry.PieceID, entry.Backend, entry.SizeBytes)
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
	return nil
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
	res, err := s.db.ExecContext(ctx, q, tenantID, contentHash)
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
	return nil
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
	var newCount int
	if err := s.db.QueryRowContext(ctx, q, tenantID, contentHash).Scan(&newCount); err != nil {
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
	return newCount, nil
}

// Delete removes the row for (tenantID, contentHash).
func (s *Store) Delete(ctx context.Context, tenantID, contentHash string) error {
	if tenantID == "" || contentHash == "" {
		return errors.New("postgres: tenant_id and content_hash are required")
	}
	q := fmt.Sprintf(`
		DELETE FROM %s WHERE tenant_id = $1 AND content_hash = $2
	`, s.table)
	res, err := s.db.ExecContext(ctx, q, tenantID, contentHash)
	if err != nil {
		return fmt.Errorf("postgres: content_index delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: content_index delete rows affected: %w", err)
	}
	if n == 0 {
		return content_index.ErrNotFound
	}
	return nil
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
