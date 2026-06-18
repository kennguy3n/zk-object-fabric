-- Row-Level Security for the multipart tables (see
-- docs/security/audit-package-security.md §8). This is the operator
-- reference; the gateway's live tests apply the identical statements via
-- internal/rlsdb.Statements — the single source of truth shared with the
-- manifests, content_index, and bucket_config tables.
--
-- The multipart store spans TWO tenant-scoped tables:
--   multipart_uploads — one row per in-flight upload session.
--   multipart_parts   — one row per uploaded part. Its tenant_id is
--                       denormalised from the owning upload (see
--                       schema.sql) so the uniform tenant_isolation
--                       policy keys on a tenant_id column here too;
--                       without it the cascade delete from
--                       multipart_uploads would not be RLS-visible under
--                       a tenant-bound transaction.
--
-- RLS here is the defence-in-depth backstop behind the application's
-- explicit tenant scoping (every store method binds the caller's tenant
-- and Get also re-checks the row's tenant_id in Go). It only takes effect
-- for a non-superuser role WITHOUT the BYPASSRLS attribute — Postgres
-- skips all policies for superusers/BYPASSRLS roles regardless of FORCE.
-- The gateway therefore must connect with a least-privilege role in
-- production (cmd/gateway refuses to boot otherwise; see
-- checkProductionRLSRole).
--
-- The gateway binds a transaction-local GUC before every tenant-scoped
-- statement:  SELECT set_config('zkof.tenant_id', <tenant>, true);
-- and, for the single audited cross-tenant sweep (the expiry sweeper,
-- which enumerates expired uploads across every tenant):
--              SELECT set_config('zkof.scan_all', 'on', true);
-- The trailing `true` makes the setting transaction-local, so it is
-- discarded at COMMIT/ROLLBACK and is safe over a shared connection pool.
-- The expiry sweeper only ENUMERATES under scan_all; it re-binds each
-- upload's own tenant before deleting it, and the WITH CHECK clause below
-- deliberately omits the scan_all bypass, so even a sweep cannot write or
-- delete across tenants in a single bound transaction.

-- 1. Provision the least-privilege application role (run once per cell).
--    This is the SAME role used for the manifests / content_index /
--    bucket_config tables; provision it once and arm every tenant-scoped
--    table against it.
--
--    CREATE ROLE zkof_app LOGIN PASSWORD '...' NOSUPERUSER NOBYPASSRLS;
--    GRANT CONNECT ON DATABASE <db> TO zkof_app;

-- 2. Arm RLS on multipart_uploads. `zkof_app` is the role from step 1.
ALTER TABLE multipart_uploads ENABLE ROW LEVEL SECURITY;
ALTER TABLE multipart_uploads FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON multipart_uploads;
CREATE POLICY tenant_isolation ON multipart_uploads
USING (
	current_setting('zkof.scan_all', true) = 'on'
	OR tenant_id = current_setting('zkof.tenant_id', true)
)
WITH CHECK (
	-- Deliberately omits the scan_all bypass: even the global expiry
	-- sweep may not write a row under a foreign tenant_id.
	tenant_id = current_setting('zkof.tenant_id', true)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON multipart_uploads TO zkof_app;

-- 3. Arm RLS on multipart_parts with the identical uniform policy.
ALTER TABLE multipart_parts ENABLE ROW LEVEL SECURITY;
ALTER TABLE multipart_parts FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON multipart_parts;
CREATE POLICY tenant_isolation ON multipart_parts
USING (
	current_setting('zkof.scan_all', true) = 'on'
	OR tenant_id = current_setting('zkof.tenant_id', true)
)
WITH CHECK (
	-- Deliberately omits the scan_all bypass: even the global expiry
	-- sweep may not write a part under a foreign tenant_id.
	tenant_id = current_setting('zkof.tenant_id', true)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON multipart_parts TO zkof_app;
