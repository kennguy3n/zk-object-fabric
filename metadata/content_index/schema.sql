-- content_index — intra-tenant deduplication index.
--
-- See docs/PROPOSAL.md §3.14.3.
--
-- Primary key (tenant_id, content_hash) is the load-bearing
-- isolation boundary: cross-tenant dedup is permanently excluded
-- from the fabric, so every lookup must carry tenant_id.
--
-- The piece_id index supports two read paths:
--   1. Orphan GC: scan rows whose piece_id no longer matches any
--      live manifest within the tenant.
--   2. Reverse lookup when the provider reports a missing piece.

CREATE TABLE content_index (
    tenant_id     TEXT        NOT NULL,
    content_hash  TEXT        NOT NULL,
    piece_id      TEXT        NOT NULL,
    backend       TEXT        NOT NULL,
    ref_count     INT         NOT NULL DEFAULT 1 CHECK (ref_count >= 0),
    size_bytes    BIGINT      NOT NULL DEFAULT 0,
    -- etag is the original PUT response ETag the first uploader saw.
    -- Recorded so subsequent dedup-hit PUTs / GETs / HEADs return the
    -- same ETag a non-dedup PUT of the same content would have. NULL
    -- for entries written before Phase 3.5 added the column.
    etag          TEXT        NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, content_hash)
);

CREATE INDEX content_index_piece_id ON content_index (piece_id);
