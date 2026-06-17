-- multipart_uploads / multipart_parts — production-grade backing
-- store for S3 multipart-upload sessions.
--
-- See docs/PROPOSAL.md and api/s3compat/multipart/postgres_store.go
-- for the design notes. Operators are responsible for applying this
-- file to the metadata Postgres before pointing the gateway at a
-- non-empty control_plane.metadata_dsn; the Go store does not run
-- migrations on its own (it just opens an already-prepared DB).
--
-- DEKMaterial — the plaintext per-object DEK held in-memory for the
-- duration of a managed / public_distribution multipart session —
-- is NEVER persisted. Only the CMK-wrapped form (wrapped_dek) is
-- stored, and Get re-derives DEKMaterial via the gateway's
-- client_sdk.Wrapper at read time.

CREATE TABLE IF NOT EXISTS multipart_uploads (
    upload_id         TEXT        PRIMARY KEY,
    tenant_id         TEXT        NOT NULL,
    bucket            TEXT        NOT NULL,
    object_key        TEXT        NOT NULL,
    -- version_id is the object version assigned at Create and
    -- recorded on the final manifest at Complete. It is fixed
    -- up-front because the managed AAD v1 binding seals each part's
    -- chunks against tenant_id|bucket|object_key_hash|version_id at
    -- UploadPart time, and the GET path rebuilds the identical AAD.
    version_id        TEXT,
    backend           TEXT,
    -- policy is the resolved metadata.PlacementPolicy captured at
    -- CreateMultipartUpload time so each UploadPart applies the
    -- tenant's policy as it existed at Create, even if the
    -- placement engine was reconfigured mid-upload.
    policy            JSONB       NOT NULL,
    enc_mode          TEXT,
    -- wrapped_dek + wrapped_key_id + wrap_algorithm describe the
    -- envelope the gateway will unwrap at Get / Complete time to
    -- recover the in-memory DEKMaterial. content_algorithm names
    -- the AEAD the multipart handler uses to seal each part once
    -- the DEK is unwrapped.
    wrapped_dek       BYTEA,
    wrapped_key_id    TEXT,
    wrap_algorithm    TEXT,
    content_algorithm TEXT,
    -- metadata is the object tag set + S3 system / user metadata the
    -- client supplied on CreateMultipartUpload (x-amz-tagging,
    -- Content-Type, x-amz-meta-*, …), captured up front and applied to
    -- the final manifest at CompleteMultipartUpload. Holding it here
    -- (rather than only in-memory) lets a Complete served by a different
    -- node than the Create still stamp the tags and metadata on the
    -- object. NULL when neither tags nor metadata were supplied.
    metadata          JSONB,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS multipart_uploads_by_tenant_bucket
    ON multipart_uploads (tenant_id, bucket);

-- The expiry sweeper scans by created_at; the index supports the
-- "uploads past UploadTTL" query without a full table scan.
CREATE INDEX IF NOT EXISTS multipart_uploads_by_created_at
    ON multipart_uploads (created_at);

-- Backfill for deployments whose multipart_uploads table predates
-- the managed AAD v1 binding: CREATE TABLE IF NOT EXISTS above is a
-- no-op on an existing table, so it would NOT add version_id. This
-- idempotent ALTER does, making a plain re-apply of schema.sql the
-- complete migration step (no separately hand-written ALTER needed).
-- It is a no-op on a fresh DB (the column already exists from the
-- CREATE TABLE) and on an already-migrated DB. The column is
-- nullable, so adding it is an instant metadata-only change with no
-- table rewrite, and pre-existing rows read back NULL -> "" ->
-- unbound (legacy) AAD, exactly matching parts those sessions sealed.
-- Applying this before rolling out the new gateway code avoids the
-- deploy-before-migrate window where Create/Get would 500 on a
-- missing column.
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS version_id TEXT;

-- Backfill for deployments whose multipart_uploads table predates the
-- object tags + metadata capture (x-amz-tagging / x-amz-meta-* applied
-- at CreateMultipartUpload). Same idempotent pattern as version_id
-- above: a no-op on a fresh or already-migrated DB. The column is
-- nullable, so adding it is an instant metadata-only change; pre-existing
-- in-flight uploads read back NULL -> no tags / no metadata, exactly
-- matching how those sessions were created. Apply before rolling out the
-- new gateway code so Create/Complete never 500 on a missing column.
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS metadata JSONB;

CREATE TABLE IF NOT EXISTS multipart_parts (
    upload_id           TEXT        NOT NULL
                                      REFERENCES multipart_uploads(upload_id)
                                      ON DELETE CASCADE,
    -- tenant_id mirrors the owning upload's tenant_id. It is
    -- denormalised onto every part row so the uniform Row-Level
    -- Security policy (internal/rlsdb.Statements) can scope parts the
    -- same way it scopes every other tenant table — on a tenant_id
    -- column against the zkof.tenant_id GUC. Without it the parts
    -- table could not carry the tenant_isolation policy, and the
    -- cascade delete from multipart_uploads would not be RLS-visible
    -- under a tenant-bound transaction. PutPart populates it from the
    -- session's upload; the ALTER + backfill below arms pre-existing
    -- deployments.
    tenant_id           TEXT,
    part_number         INTEGER     NOT NULL,
    piece_id            TEXT        NOT NULL,
    backend             TEXT        NOT NULL,
    etag                TEXT,
    size_bytes          BIGINT,
    -- part_hash is the BLAKE3 digest of the ciphertext the
    -- gateway streamed to the backend (used by the
    -- client_side / unencrypted dedup pipeline at
    -- CompleteMultipartUpload time). NULL for non-dedup uploads.
    part_hash           BYTEA,
    -- plaintext_part_hash is the BLAKE3 digest of the *plaintext*
    -- (used by the managed / public_distribution deferred
    -- convergent consolidation path). NULL for non-dedup uploads.
    plaintext_part_hash BYTEA,
    uploaded_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (upload_id, part_number)
);

-- Backfill multipart_parts.tenant_id for deployments whose parts table
-- predates Row-Level Security: CREATE TABLE IF NOT EXISTS above is a no-op
-- on an existing table, so it would NOT add the column. This idempotent
-- ALTER does, and the UPDATE copies each part's tenant from its owning
-- upload so previously-stored in-flight parts become RLS-visible under a
-- tenant-bound transaction. Both are no-ops on a fresh DB (the column
-- exists from CREATE TABLE and no rows yet need backfilling) and on an
-- already-migrated DB. Apply before rolling out the RLS-aware gateway so
-- the parts of an in-flight legacy upload are not orphaned (fail-closed
-- invisible) mid-deploy.
ALTER TABLE multipart_parts ADD COLUMN IF NOT EXISTS tenant_id TEXT;

UPDATE multipart_parts p
   SET tenant_id = u.tenant_id
  FROM multipart_uploads u
 WHERE p.upload_id = u.upload_id
   AND p.tenant_id IS NULL;
