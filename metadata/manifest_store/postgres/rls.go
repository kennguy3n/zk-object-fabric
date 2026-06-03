package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// Row-Level Security (RLS) substrate — Workstream 3.4 defence-in-depth.
//
// Tenant scoping is enforced in two independent layers:
//
//   1. Every query carries an explicit `WHERE tenant_id = $1` predicate
//      (the application layer, present since Phase 2).
//   2. Postgres RLS policies re-check the same predicate from a
//      transaction-local GUC, so a query that omits or mistakes the
//      predicate still cannot read or write another tenant's rows.
//
// Layer 2 is a safety net: it only changes behaviour when the
// application layer has a bug. The two layers bind the *same*
// tenant value (the caller's key.TenantID), so under correct
// operation they are redundant by construction.
//
// IMPORTANT: Postgres bypasses RLS for superusers and roles with the
// BYPASSRLS attribute, regardless of FORCE ROW LEVEL SECURITY. The
// gateway's metadata connection must therefore use a least-privilege,
// non-superuser role for layer 2 to have any effect; cmd/gateway
// enforces this under env=production. When the connecting role is a
// superuser (e.g. a dev database), the GUC binding below is a harmless
// no-op and layer 1 continues to enforce isolation on its own.

const (
	// gucTenantID is the transaction-local setting the tenant_isolation
	// policy reads to scope a statement to a single tenant. It is set
	// via set_config(name, value, true) so it is is_local — it lives
	// only for the current transaction and is discarded at COMMIT or
	// ROLLBACK, which makes it safe to use over a shared connection
	// pool (no leakage onto a recycled connection).
	gucTenantID = "zkof.tenant_id"

	// gucScanAll, when set to "on" transaction-locally, opens the
	// tenant_isolation policy to every tenant's rows. It exists for the
	// single legitimate cross-tenant reader — the global ScanManifests
	// sweep used by the AAD v1 migration worker — and is never set on a
	// request-scoped path. The policy's WITH CHECK clause deliberately
	// does NOT honour gucScanAll, so even the sweep cannot write a row
	// under a foreign tenant_id.
	gucScanAll = "zkof.scan_all"
)

// beginTenant opens a transaction whose tenant-isolation GUC is bound to
// tenantID transaction-locally, so Postgres RLS scopes every statement in
// the transaction to that tenant. Callers own the returned *sql.Tx and
// must Commit it (or rely on a deferred Rollback) — see the per-method
// commitOrRollback helper. A bind failure rolls the transaction back.
func (s *Store) beginTenant(ctx context.Context, tenantID string) (*sql.Tx, error) {
	return s.beginWithScope(ctx, gucTenantID, tenantID)
}

// beginScanAll opens a transaction that may read every tenant's rows. It
// is the only constructor that sets gucScanAll, and it is used solely by
// ScanManifests for the global migration sweep.
func (s *Store) beginScanAll(ctx context.Context) (*sql.Tx, error) {
	return s.beginWithScope(ctx, gucScanAll, "on")
}

func (s *Store) beginWithScope(ctx context.Context, guc, value string) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("postgres: begin tenant-scoped tx: %w", err)
	}
	// set_config(name, value, true) — the trailing true makes the
	// setting transaction-local. We bind the value as a parameter so a
	// tenant id can never be interpreted as SQL.
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, guc, value); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("postgres: bind %s scope: %w", guc, err)
	}
	return tx, nil
}

// RLSStatements returns the idempotent DDL that arms Row-Level Security on
// the manifests table for a least-privilege application role. Operators
// run it once per cell after creating the table (the table DDL lives in
// the package doc comment), and the live-PG tests run it to exercise
// enforcement. appRole is the non-superuser role the gateway connects as;
// it is validated as a safe identifier so it can be interpolated into the
// GRANT statement, which does not accept a bind parameter for the role.
func RLSStatements(table, appRole string) ([]string, error) {
	if !isSafeIdent(table) {
		return nil, fmt.Errorf("postgres: invalid table name %q", table)
	}
	if !isSafeIdent(appRole) {
		return nil, fmt.Errorf("postgres: invalid app role %q", appRole)
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
	)`, table, gucScanAll, gucTenantID, gucTenantID),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON %s TO %s`, table, appRole),
	}, nil
}
