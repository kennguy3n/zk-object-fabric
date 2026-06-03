-- Row-Level Security for the manifests table (Workstream 3.4,
-- docs/security/audit-package-security.md §8). This is the operator
-- reference; the gateway's live tests apply the identical statements via
-- postgres.RLSStatements(table, appRole), which is the source of truth.
--
-- RLS is a *defence-in-depth* layer behind the application's explicit
-- `WHERE tenant_id = $1` predicates. It only takes effect for a
-- non-superuser role WITHOUT the BYPASSRLS attribute — Postgres skips all
-- policies for superusers/BYPASSRLS roles regardless of FORCE. The
-- gateway therefore must connect with a least-privilege role in
-- production (cmd/gateway refuses to boot otherwise; see
-- checkProductionRLSRole).
--
-- The gateway binds a transaction-local GUC before every tenant-scoped
-- statement:  SELECT set_config('zkof.tenant_id', <tenant>, true);
-- and, for the single audited cross-tenant sweep (ScanManifests):
--              SELECT set_config('zkof.scan_all', 'on', true);
-- The trailing `true` makes the setting transaction-local, so it is
-- discarded at COMMIT/ROLLBACK and is safe over a shared connection pool.

-- 1. Provision the least-privilege application role (run once per cell).
--    Replace the password with a secret from your secret manager.
--
--    CREATE ROLE zkof_app LOGIN PASSWORD '...' NOSUPERUSER NOBYPASSRLS;
--    GRANT CONNECT ON DATABASE <db> TO zkof_app;

-- 2. Arm RLS on the manifests table. `zkof_app` is the role from step 1.
ALTER TABLE manifests ENABLE ROW LEVEL SECURITY;
ALTER TABLE manifests FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON manifests;
CREATE POLICY tenant_isolation ON manifests
	USING (
		current_setting('zkof.scan_all', true) = 'on'
		OR tenant_id = current_setting('zkof.tenant_id', true)
	)
	WITH CHECK (
		-- Deliberately omits the scan_all bypass: even the global sweep
		-- may not write a row under a foreign tenant_id.
		tenant_id = current_setting('zkof.tenant_id', true)
	);

GRANT SELECT, INSERT, UPDATE, DELETE ON manifests TO zkof_app;
