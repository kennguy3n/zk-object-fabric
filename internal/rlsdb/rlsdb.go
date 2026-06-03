// Package rlsdb is the shared Postgres Row-Level Security (RLS)
// substrate — Workstream 3.4 defence-in-depth — used by every
// tenant-scoped metadata store (manifests, content_index, …).
//
// Tenant scoping is enforced in two independent layers:
//
//  1. Every query carries an explicit `WHERE tenant_id = $1` predicate
//     (the application layer, present since Phase 2).
//  2. Postgres RLS policies re-check the same predicate from a
//     transaction-local GUC, so a query that omits or mistakes the
//     predicate still cannot read or write another tenant's rows.
//
// Layer 2 is a safety net: it only changes behaviour when the
// application layer has a bug. The two layers bind the *same* tenant
// value, so under correct operation they are redundant by construction.
//
// IMPORTANT: Postgres bypasses RLS for superusers and roles with the
// BYPASSRLS attribute, regardless of FORCE ROW LEVEL SECURITY. The
// gateway's metadata connection must therefore use a least-privilege,
// non-superuser role for layer 2 to have any effect; cmd/gateway
// enforces this under env=production. When the connecting role is a
// superuser (e.g. a dev database), the GUC binding below is a harmless
// no-op and layer 1 continues to enforce isolation on its own.
package rlsdb

import (
	"context"
	"database/sql"
	"fmt"
)

const (
	// GUCTenantID is the transaction-local setting the tenant_isolation
	// policy reads to scope a statement to a single tenant. It is set
	// via set_config(name, value, true) so it is is_local — it lives
	// only for the current transaction and is discarded at COMMIT or
	// ROLLBACK, which makes it safe to use over a shared connection pool
	// (no leakage onto a recycled connection).
	GUCTenantID = "zkof.tenant_id"

	// GUCScanAll, when set to "on" transaction-locally, opens the
	// tenant_isolation policy to every tenant's rows. It exists for the
	// audited cross-tenant readers — currently the global ScanManifests
	// sweep (AAD v1 migration), the content_index orphan-GC tenant
	// enumeration, and the bucket_config ListLifecycle sweep (background
	// lifecycle evaluator) — and is never set on a request-scoped path.
	// The policy's WITH CHECK clause deliberately does NOT honour
	// GUCScanAll, so even a sweep cannot write a row under a foreign
	// tenant_id.
	GUCScanAll = "zkof.scan_all"
)

// BeginTenant opens a transaction on db whose tenant-isolation GUC is
// bound to tenantID transaction-locally, so Postgres RLS scopes every
// statement in the transaction to that tenant. Callers own the returned
// *sql.Tx and must Commit it (or rely on a deferred Rollback). A bind
// failure rolls the transaction back.
//
// An empty tenantID is rejected before any transaction is opened: binding
// zkof.tenant_id to '' would scope the transaction to rows with an empty
// tenant_id, which is never a real tenant. Every in-tree caller already
// validates tenantID upstream, but BeginTenant is exported and this guard
// fails closed for any future caller that forgets to.
func BeginTenant(ctx context.Context, db *sql.DB, tenantID string) (*sql.Tx, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("rlsdb: refusing to bind empty tenant id")
	}
	return beginWithScope(ctx, db, GUCTenantID, tenantID)
}

// BeginScanAll opens a transaction on db that may read every tenant's
// rows. It is the only constructor that sets GUCScanAll, and is used
// solely by the audited global sweeps (manifest AAD migration, content
// index orphan-GC tenant enumeration, and bucket_config ListLifecycle
// for the background lifecycle evaluator).
func BeginScanAll(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	return beginWithScope(ctx, db, GUCScanAll, "on")
}

func beginWithScope(ctx context.Context, db *sql.DB, guc, value string) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("rlsdb: begin tenant-scoped tx: %w", err)
	}
	// set_config(name, value, true) — the trailing true makes the
	// setting transaction-local. We bind the value as a parameter so a
	// tenant id can never be interpreted as SQL.
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, guc, value); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("rlsdb: bind %s scope: %w", guc, err)
	}
	return tx, nil
}

// Statements returns the idempotent DDL that arms Row-Level Security on
// table for a least-privilege application role. Operators run it once
// per cell after creating the table, and the live-PG tests run it to
// exercise enforcement. appRole is the non-superuser role the gateway
// connects as; both identifiers are validated as safe so they can be
// interpolated into ALTER/GRANT statements, which do not accept a bind
// parameter for an identifier.
//
// The generated tenant_isolation policy is uniform across tables: it
// scopes on a `tenant_id` column against GUCTenantID, with a GUCScanAll
// read-only bypass. Every tenant-scoped table in the fabric keys on a
// `tenant_id TEXT` column, so the same policy text applies verbatim.
func Statements(table, appRole string) ([]string, error) {
	if !isSafeIdent(table) {
		return nil, fmt.Errorf("rlsdb: invalid table name %q", table)
	}
	if !isSafeIdent(appRole) {
		return nil, fmt.Errorf("rlsdb: invalid app role %q", appRole)
	}
	return []string{
		fmt.Sprintf(`ALTER TABLE %s ENABLE ROW LEVEL SECURITY`, table),
		// FORCE so the policy applies even when the connecting role owns
		// the table; without it an owner connection would silently bypass
		// RLS exactly like a superuser does.
		fmt.Sprintf(`ALTER TABLE %s FORCE ROW LEVEL SECURITY`, table),
		fmt.Sprintf(`DROP POLICY IF EXISTS tenant_isolation ON %s`, table),
		fmt.Sprintf(`CREATE POLICY tenant_isolation ON %s
	USING (
		current_setting('%s', true) = 'on'
		OR tenant_id = current_setting('%s', true)
	)
	WITH CHECK (
		tenant_id = current_setting('%s', true)
	)`, table, GUCScanAll, GUCTenantID, GUCTenantID),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON %s TO %s`, table, appRole),
	}, nil
}

// isSafeIdent validates that s is a plausible SQL identifier: ASCII
// letters, digits, and underscore only, not starting with a digit.
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
