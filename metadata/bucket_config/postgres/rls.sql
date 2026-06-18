-- Row-Level Security for the bucket_config tables (see
-- docs/security/audit-package-security.md §8). This is the operator
-- reference; the gateway's live tests apply the identical statements via
-- postgres.RLSStatements(table, appRole), which delegates to
-- internal/rlsdb.Statements — the single source of truth shared
-- with the manifests and content_index tables.
--
-- bucket_config holds six per-bucket sub-resource tables, each keyed on
-- (tenant_id, bucket): versioning, object lock, CORS, lifecycle,
-- notification, and encryption. RLS
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
-- and, for the single audited cross-tenant sweep (ListLifecycle, run by
-- the background lifecycle evaluator):
--              SELECT set_config('zkof.scan_all', 'on', true);
-- The trailing `true` makes the setting transaction-local, so it is
-- discarded at COMMIT/ROLLBACK and is safe over a shared connection pool.

-- 1. Provision the least-privilege application role (run once per cell).
--    This is the SAME role used for the manifests and content_index
--    tables; provision it once and arm every tenant-scoped table.
--
--    CREATE ROLE zkof_app LOGIN PASSWORD '...' NOSUPERUSER NOBYPASSRLS;
--    GRANT CONNECT ON DATABASE <db> TO zkof_app;

-- 2. Arm RLS on each bucket_config table. `zkof_app` is the role from
--    step 1. The six blocks are identical except for the table name —
--    every table keys on (tenant_id, bucket), so the tenant_isolation
--    policy text is uniform.

ALTER TABLE bucket_versioning ENABLE ROW LEVEL SECURITY;
ALTER TABLE bucket_versioning FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bucket_versioning;
CREATE POLICY tenant_isolation ON bucket_versioning
USING (
current_setting('zkof.scan_all', true) = 'on'
OR tenant_id = current_setting('zkof.tenant_id', true)
)
WITH CHECK (
-- Deliberately omits the scan_all bypass: even the global sweep
-- may not write a row under a foreign tenant_id.
tenant_id = current_setting('zkof.tenant_id', true)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON bucket_versioning TO zkof_app;

ALTER TABLE bucket_object_lock ENABLE ROW LEVEL SECURITY;
ALTER TABLE bucket_object_lock FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bucket_object_lock;
CREATE POLICY tenant_isolation ON bucket_object_lock
USING (
current_setting('zkof.scan_all', true) = 'on'
OR tenant_id = current_setting('zkof.tenant_id', true)
)
WITH CHECK (
tenant_id = current_setting('zkof.tenant_id', true)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON bucket_object_lock TO zkof_app;

ALTER TABLE bucket_cors ENABLE ROW LEVEL SECURITY;
ALTER TABLE bucket_cors FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bucket_cors;
CREATE POLICY tenant_isolation ON bucket_cors
USING (
current_setting('zkof.scan_all', true) = 'on'
OR tenant_id = current_setting('zkof.tenant_id', true)
)
WITH CHECK (
tenant_id = current_setting('zkof.tenant_id', true)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON bucket_cors TO zkof_app;

ALTER TABLE bucket_lifecycle ENABLE ROW LEVEL SECURITY;
ALTER TABLE bucket_lifecycle FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bucket_lifecycle;
CREATE POLICY tenant_isolation ON bucket_lifecycle
USING (
current_setting('zkof.scan_all', true) = 'on'
OR tenant_id = current_setting('zkof.tenant_id', true)
)
WITH CHECK (
tenant_id = current_setting('zkof.tenant_id', true)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON bucket_lifecycle TO zkof_app;

ALTER TABLE bucket_notification ENABLE ROW LEVEL SECURITY;
ALTER TABLE bucket_notification FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bucket_notification;
CREATE POLICY tenant_isolation ON bucket_notification
USING (
current_setting('zkof.scan_all', true) = 'on'
OR tenant_id = current_setting('zkof.tenant_id', true)
)
WITH CHECK (
tenant_id = current_setting('zkof.tenant_id', true)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON bucket_notification TO zkof_app;

ALTER TABLE bucket_encryption ENABLE ROW LEVEL SECURITY;
ALTER TABLE bucket_encryption FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bucket_encryption;
CREATE POLICY tenant_isolation ON bucket_encryption
USING (
current_setting('zkof.scan_all', true) = 'on'
OR tenant_id = current_setting('zkof.tenant_id', true)
)
WITH CHECK (
tenant_id = current_setting('zkof.tenant_id', true)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON bucket_encryption TO zkof_app;
