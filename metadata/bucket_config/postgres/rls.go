package postgres

import (
	"context"
	"database/sql"

	"github.com/kennguy3n/zk-object-fabric/internal/rlsdb"
)

// Row-Level Security (RLS) for the bucket_config tables — Workstream 3.4
// defence-in-depth. Every bucket sub-resource (versioning, object lock,
// CORS, lifecycle, notification) is keyed on (tenant_id, bucket), so a query that
// forgot its tenant predicate must not read or write another tenant's
// bucket configuration. The mechanism lives in the shared
// internal/rlsdb package (GUC binding, the tenant_isolation
// policy DDL, and the superuser caveat); the helpers below are thin
// Store-scoped adapters so the store methods read naturally.

// beginTenant opens a transaction whose tenant-isolation GUC is bound to
// tenantID transaction-locally, so Postgres RLS scopes every statement in
// the transaction to that tenant. Callers own the returned *sql.Tx and
// must Commit it (or rely on a deferred Rollback).
func (s *Store) beginTenant(ctx context.Context, tenantID string) (*sql.Tx, error) {
	return rlsdb.BeginTenant(ctx, s.db, tenantID)
}

// beginScanAll opens a transaction that may read every tenant's rows. It
// is used solely by ListLifecycle, the cross-tenant sweep the background
// lifecycle evaluator runs once per pass.
func (s *Store) beginScanAll(ctx context.Context) (*sql.Tx, error) {
	return rlsdb.BeginScanAll(ctx, s.db)
}

// RLSStatements returns the idempotent DDL that arms Row-Level Security on
// one bucket_config table for a least-privilege application role. It is a
// thin alias of rlsdb.Statements, kept so operators and tests in this
// package have a table-local entry point. bucket_config has five
// tenant-keyed tables (versioning, object lock, CORS, lifecycle,
// notification); arm each one with its own call (see rls.sql).
func RLSStatements(table, appRole string) ([]string, error) {
	return rlsdb.Statements(table, appRole)
}
