-- Row-Level Security for the content_index table (see
-- docs/security/audit-package-security.md §8). This is the operator
-- reference; the gateway's live tests apply the identical statements via
-- postgres.RLSStatements(table, appRole), which delegates to
-- internal/rlsdb.Statements — the single source of truth shared
-- with the manifests table.
--
-- content_index's (tenant_id, content_hash) primary key is the
-- load-bearing isolation boundary for intra-tenant deduplication, so RLS
-- here is the defence-in-depth backstop behind the application's explicit
-- `WHERE tenant_id = $1` predicates. It only takes effect for a
-- non-superuser role WITHOUT the BYPASSRLS attribute — Postgres skips all
-- policies for superusers/BYPASSRLS roles regardless of FORCE. The
-- gateway therefore must connect with a least-privilege role in
-- production (cmd/gateway refuses to boot otherwise; see
-- checkProductionRLSRole).
--
-- The gateway binds a transaction-local GUC before every tenant-scoped
-- statement:  SELECT set_config('zkof.tenant_id', <tenant>, true);
-- and, for the single audited cross-tenant sweep (ListTenants, used by
-- orphan GC to enumerate tenants):
--              SELECT set_config('zkof.scan_all', 'on', true);
-- The trailing `true` makes the setting transaction-local, so it is
-- discarded at COMMIT/ROLLBACK and is safe over a shared connection pool.

-- 1. Provision the least-privilege application role (run once per cell).
--    This is the SAME role used for the manifests table; provision it
--    once and arm every tenant-scoped table against it.
--
--    CREATE ROLE zkof_app LOGIN PASSWORD '...' NOSUPERUSER NOBYPASSRLS;
--    GRANT CONNECT ON DATABASE <db> TO zkof_app;

-- 2. Arm RLS on the content_index table. `zkof_app` is the role from step 1.
ALTER TABLE content_index ENABLE ROW LEVEL SECURITY;
ALTER TABLE content_index FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON content_index;
CREATE POLICY tenant_isolation ON content_index
USING (
current_setting('zkof.scan_all', true) = 'on'
OR tenant_id = current_setting('zkof.tenant_id', true)
)
WITH CHECK (
-- Deliberately omits the scan_all bypass: even the global sweep
-- may not write a row under a foreign tenant_id.
tenant_id = current_setting('zkof.tenant_id', true)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON content_index TO zkof_app;
