package compliance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SQLiteAllowlistStore is the SQLite-backed tenant country allowlist
// used by the embedded / single-node deployment profile (docker
// compose up with no Postgres). It mirrors the Postgres
// tenant_country_allowlist table the residency enforcer joins against,
// but — like the other embedded stores — self-creates its schema since
// the embedded profile has no separate migration step.
//
// Before this store existed the embedded profile had no third tier for
// the residency allowlist: with a static allowlist configured it used
// that, otherwise the lookup was nil (allow-all) — the asymmetry with
// the legal-hold store's Postgres → SQLite → Memory selection that the
// #106 review flagged. This store is the missing tier: in the embedded
// profile (no static config, no Postgres) the residency enforcer reads
// the allowlist from the local SQLite file instead of being
// unconditionally allow-all, and the rows survive a gateway restart
// like the embedded audit and legal-hold stores.
//
// It mirrors the Postgres tenant_country_allowlist tier exactly: the
// table is operator-seeded (the schema is self-created here because the
// embedded profile has no migration step), and the residency hot path
// only reads it via Lookup. Static config, when present, remains the
// authoritative first tier and is not copied here. Replace is the
// single writer, used by tests and reusable by any future management
// surface.
type SQLiteAllowlistStore struct {
	db *sql.DB
}

// Lookup is the read path the residency enforcer consults. The method
// value satisfies AllowlistLookup.
var _ AllowlistLookup = (*SQLiteAllowlistStore)(nil).Lookup

// NewSQLiteAllowlistStore returns a store backed by db and creates the
// backing table if it does not yet exist. The caller owns the
// connection pool's lifecycle (it is shared with the other embedded
// stores via internal/embeddeddb).
func NewSQLiteAllowlistStore(db *sql.DB) (*SQLiteAllowlistStore, error) {
	if db == nil {
		return nil, errors.New("compliance: sqlite allowlist store requires a non-nil *sql.DB")
	}
	s := &SQLiteAllowlistStore{db: db}
	if err := s.ensureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteAllowlistStore) ensureSchema(ctx context.Context) error {
	const q = `CREATE TABLE IF NOT EXISTS tenant_country_allowlist (
		tenant_id TEXT NOT NULL,
		country   TEXT NOT NULL,
		PRIMARY KEY (tenant_id, country)
	)`
	if _, err := s.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("compliance: ensure sqlite allowlist schema: %w", err)
	}
	return nil
}

// Lookup returns the country codes a tenant is permitted to land data
// in, or a nil slice when the tenant has no rows. A zero-length result
// (nil or empty) means "no restriction": the enforcer treats it as
// allow-all so tenants without an allowlist do not regress. Countries
// are stored normalized (trimmed, upper-cased) so the result is stable.
//
// The signature (no context) matches AllowlistLookup and the Postgres
// lookup in cmd/gateway; it uses context.Background internally.
func (s *SQLiteAllowlistStore) Lookup(tenantID string) ([]string, error) {
	const q = `SELECT country FROM tenant_country_allowlist WHERE tenant_id = ? ORDER BY country`
	rows, err := s.db.QueryContext(context.Background(), q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("compliance: query allowlist: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("compliance: scan allowlist row: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Replace sets the exact country allowlist for a tenant, replacing any
// existing rows in a single transaction. An empty (or all-blank)
// country list clears the tenant's allowlist, reverting it to
// allow-all. Countries are normalized (trimmed, upper-cased) and
// de-duplicated before insert, matching how the enforcer compares.
//
// Replace is the store's only writer: the gateway uses it to seed
// StaticAllowlist config into the embedded DB on boot, and a future
// management surface (console/CLI) can reuse it for runtime updates.
func (s *SQLiteAllowlistStore) Replace(ctx context.Context, tenantID string, countries []string) error {
	if strings.TrimSpace(tenantID) == "" {
		return errors.New("compliance: tenant_id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("compliance: begin allowlist replace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM tenant_country_allowlist WHERE tenant_id = ?`, tenantID); err != nil {
		return fmt.Errorf("compliance: clear allowlist: %w", err)
	}
	seen := map[string]struct{}{}
	for _, c := range countries {
		norm := strings.ToUpper(strings.TrimSpace(c))
		if norm == "" {
			continue
		}
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tenant_country_allowlist (tenant_id, country) VALUES (?, ?)`,
			tenantID, norm,
		); err != nil {
			return fmt.Errorf("compliance: insert allowlist row: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("compliance: commit allowlist replace: %w", err)
	}
	return nil
}
