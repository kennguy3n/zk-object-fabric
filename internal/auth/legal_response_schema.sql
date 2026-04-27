-- internal/auth/legal_response_schema.sql
--
-- Postgres schema for the LegalHoldStore. The control-plane
-- console writes here from the
-- POST /api/v1/tenants/{tid}/legal-hold endpoint; the s3compat
-- DELETE hot path queries it via auth.CheckDelete.
--
-- Holds are append-only: a release is recorded by setting
-- released=true and released_at=now(); the original row is
-- never deleted so the compliance audit trail stays intact.

CREATE TABLE IF NOT EXISTS legal_holds (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    bucket       TEXT NOT NULL DEFAULT '',
    object_key   TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL,
    case_id      TEXT NOT NULL DEFAULT '',
    issued_by    TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ,
    released     BOOLEAN NOT NULL DEFAULT FALSE,
    released_at  TIMESTAMPTZ
);

-- Hot-path lookup used by auth.CheckDelete on every DELETE. The
-- (tenant_id, bucket, object_key) tuple covers all three hold
-- scopes (tenant, bucket, object).
CREATE INDEX IF NOT EXISTS idx_legal_holds_tenant_bucket_key
    ON legal_holds (tenant_id, bucket, object_key)
    WHERE released = FALSE;

-- Per-tenant listing for the console handler.
CREATE INDEX IF NOT EXISTS idx_legal_holds_tenant
    ON legal_holds (tenant_id, created_at DESC);

-- Gateway role MUST NOT be granted UPDATE/DELETE on this table
-- beyond the explicit "release" operation; preventing accidental
-- mutation is part of the legal-hold defence in depth. Operators
-- run release through the console only.
