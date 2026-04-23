-- schema.sql defines the tables the Postgres-backed PlacementStore
-- depends on. Each tenant has at most one active placement policy
-- document; the full policy body is stored as JSON so the schema
-- can evolve without per-field migrations.

CREATE TABLE IF NOT EXISTS placement_policies (
    tenant_id   TEXT        PRIMARY KEY,
    policy_json JSONB       NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
