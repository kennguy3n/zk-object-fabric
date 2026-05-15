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
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS multipart_uploads_by_tenant_bucket
    ON multipart_uploads (tenant_id, bucket);

-- The expiry sweeper scans by created_at; the index supports the
-- "uploads past UploadTTL" query without a full table scan.
CREATE INDEX IF NOT EXISTS multipart_uploads_by_created_at
    ON multipart_uploads (created_at);

CREATE TABLE IF NOT EXISTS multipart_parts (
    upload_id           TEXT        NOT NULL
                                      REFERENCES multipart_uploads(upload_id)
                                      ON DELETE CASCADE,
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
