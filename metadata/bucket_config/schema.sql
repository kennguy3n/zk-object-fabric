-- Per-bucket S3 configuration sub-resources.
--
-- This holds bucket versioning state, Object Lock, CORS, lifecycle,
-- event-notification, and default-encryption configuration, one
-- sibling table per sub-resource.
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

-- Per-bucket S3 Object Lock configuration. The never-configured
-- state is the absence of a row, surfaced as a zero object_lock.Config
-- (Enabled false). When enabled WITH a default retention rule,
-- default_mode is set and exactly one of default_days/default_years is
-- > 0; new object versions inherit that retention at PUT time. Enabling
-- Object Lock requires bucket versioning, enforced at the API layer.
CREATE TABLE IF NOT EXISTS bucket_object_lock (
    tenant_id     TEXT NOT NULL,
    bucket        TEXT NOT NULL,
    enabled       BOOLEAN NOT NULL,
    default_mode  TEXT NOT NULL DEFAULT '' CHECK (default_mode IN ('', 'GOVERNANCE', 'COMPLIANCE')),
    default_days  INTEGER NOT NULL DEFAULT 0,
    default_years INTEGER NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bucket)
);

-- Per-bucket S3 CORS configuration. The never-configured state
-- is the absence of a row, surfaced to callers as an empty
-- cors.Config (no rules) and to the S3 API as 404
-- NoSuchCORSConfiguration. The rule set is stored as a JSON document
-- (the stable encoding owned by metadata/cors) rather than a column
-- per field, because each rule carries variable-length lists of
-- origins/methods/headers. DeleteBucketCors removes the row.
CREATE TABLE IF NOT EXISTS bucket_cors (
    tenant_id  TEXT NOT NULL,
    bucket     TEXT NOT NULL,
    rules      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bucket)
);

-- Per-bucket S3 lifecycle configuration. As with CORS, the
-- never-configured state is the absence of a row, surfaced to callers
-- as an empty lifecycle.Config (no rules) and to the S3 API as 404
-- NoSuchLifecycleConfiguration. The full rule set (expiration,
-- transition, abort-incomplete-multipart, and filter predicates) is
-- stored as one JSON document — the stable encoding owned by
-- metadata/lifecycle — rather than a column per field, because each
-- rule carries optional nested actions and variable-length tag
-- filters. DeleteBucketLifecycle removes the row. The background
-- lifecycle evaluator reads every row across all tenants once per pass
-- (Store.ListLifecycle).
CREATE TABLE IF NOT EXISTS bucket_lifecycle (
    tenant_id  TEXT NOT NULL,
    bucket     TEXT NOT NULL,
    rules      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bucket)
);

-- Per-bucket S3 event-notification configuration. As with CORS
-- and lifecycle, the never-configured state is the absence of a row,
-- surfaced to callers as an empty notification.Config (no rules). The
-- rule set (event classes, webhook endpoint, prefix/suffix filters) is
-- stored as one JSON document — the stable encoding owned by
-- metadata/notification — because each rule carries a variable-length
-- list of subscribed events. PutBucketNotificationConfiguration with an
-- empty body clears the configuration (the row is removed); S3 has no
-- separate DeleteBucketNotification operation.
CREATE TABLE IF NOT EXISTS bucket_notification (
    tenant_id  TEXT NOT NULL,
    bucket     TEXT NOT NULL,
    rules      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bucket)
);

-- Per-bucket default server-side encryption configuration. The
-- never-configured state is the absence of a row, surfaced to callers
-- as an empty sse.Config and to the S3 API as 404
-- ServerSideEncryptionConfigurationNotFoundError. The default
-- (algorithm + optional KMS key + bucket-key flag) is stored as one
-- JSON document — the stable encoding owned by metadata/sse — for
-- symmetry with the other JSON-backed sub-resources.
-- DeleteBucketEncryption removes the row.
CREATE TABLE IF NOT EXISTS bucket_encryption (
    tenant_id  TEXT NOT NULL,
    bucket     TEXT NOT NULL,
    config     TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bucket)
);
