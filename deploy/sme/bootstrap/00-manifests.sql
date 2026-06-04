-- manifests table for the Postgres-backed manifest store
-- (metadata/manifest_store/postgres). The store's package doc carries
-- the canonical DDL, but ships no .sql file because the live tests
-- build the table via internal/rlsdb. This file reproduces that DDL for
-- the SME single-pool bootstrap.
--
-- IMPORTANT: the SME deployment always configures a manifest body key
-- (encryption.manifest_body_key_path), so the store seals the manifest
-- JSON with XChaCha20-Poly1305 before INSERT. The body column is
-- therefore BYTEA, not JSONB — JSONB rejects the opaque ciphertext (see
-- metadata/manifest_store/postgres Config.BodyEncryptor).
CREATE TABLE IF NOT EXISTS manifests (
    tenant_id          TEXT  NOT NULL,
    bucket             TEXT  NOT NULL,
    object_key_hash    TEXT  NOT NULL,
    version_id         TEXT  NOT NULL,
    body               BYTEA NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bucket, object_key_hash, version_id)
);

CREATE INDEX IF NOT EXISTS manifests_by_tenant_bucket
    ON manifests (tenant_id, bucket, object_key_hash);
