-- Per-bucket S3 configuration sub-resources (WS8.4+).
--
-- Today this holds bucket versioning state; future sub-resources
-- (CORS, lifecycle, notifications) can add columns or sibling tables.
-- Buckets are implicit in the fabric, so the row is keyed by
-- (tenant_id, bucket) directly rather than referencing a bucket table.
--
-- The Postgres deployment applies this via the standard migration
-- runner; the embedded SQLite profile self-creates an equivalent
-- table at store construction (see metadata/bucket_config/sqlite).

CREATE TABLE IF NOT EXISTS bucket_versioning (
    tenant_id  TEXT NOT NULL,
    bucket     TEXT NOT NULL,
    -- 'Enabled' or 'Suspended'. The never-configured state is the
    -- absence of a row, surfaced to callers as VersioningUnset.
    state      TEXT NOT NULL CHECK (state IN ('Enabled', 'Suspended')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bucket)
);
