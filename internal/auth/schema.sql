-- schema.sql defines the tables the Postgres-backed TenantStore
-- depends on. The control-plane Postgres instance owns these tables;
-- the gateway fleet is a read/write client via PostgresTenantStore.
--
-- tenants holds the canonical tenant record (as JSON so the schema
-- can evolve without per-field migrations). tenant_bindings maps a
-- gateway access key to a tenant; the tenant_json column is a
-- denormalized cache of the tenant record at the time the binding
-- was written so LookupByAccessKey can answer without a JOIN on the
-- authentication hot path.
--
-- DeleteTenant cascades binding deletion through the ON DELETE
-- CASCADE clause so the signup rollback path (see
-- MemoryTenantStore.DeleteTenant docstring) does not need a second
-- round-trip to clean up orphan access keys.

CREATE TABLE IF NOT EXISTS tenants (
    tenant_id    TEXT        PRIMARY KEY,
    tenant_json  JSONB       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS tenant_bindings (
    access_key   TEXT        PRIMARY KEY,
    secret_key   TEXT        NOT NULL,
    tenant_id    TEXT        NOT NULL REFERENCES tenants(tenant_id) ON DELETE CASCADE,
    tenant_json  JSONB       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_tenant_bindings_tenant_id
    ON tenant_bindings(tenant_id);
