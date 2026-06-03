-- schema.sql defines the tables the Postgres-backed PlacementStore
-- and the Phase 3 Postgres-backed AuthStore / DedicatedCellStore
-- depend on. Each tenant has at most one active placement policy
-- document; the full policy body is stored as JSON so the schema
-- can evolve without per-field migrations.

CREATE TABLE IF NOT EXISTS placement_policies (
    tenant_id   TEXT        PRIMARY KEY,
    policy_json JSONB       NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- auth_users persists the email → (bcrypt hash, tenant ID,
-- verified flag, verify token) mapping the B2C self-service signup
-- and login flow consumes. The email PRIMARY KEY enforces case-
-- insensitive uniqueness because PostgresAuthStore lower-cases
-- every email before insert/lookup.
CREATE TABLE IF NOT EXISTS auth_users (
    email         TEXT        PRIMARY KEY,
    password_hash TEXT        NOT NULL,
    tenant_id     TEXT        NOT NULL,
    verified      BOOLEAN     NOT NULL DEFAULT FALSE,
    verify_token  TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Tenant lookup is the hot path for the S3 VerifiedCheck gate; the
-- B-tree index makes IsVerified an O(log N) point lookup.
CREATE INDEX IF NOT EXISTS idx_auth_users_tenant ON auth_users(tenant_id);

-- The verify_token index covers ConsumeVerificationToken's pending-
-- row scan. The partial predicate keeps the index small (only
-- pending rows ever hold a non-null token).
CREATE INDEX IF NOT EXISTS idx_auth_users_verify_token
    ON auth_users(verify_token)
    WHERE verify_token IS NOT NULL;

-- refresh_tokens persists the long-lived refresh tokens the SPA
-- exchanges for fresh short-lived access tokens. Only the SHA-256
-- hash of each token is stored (token_hash PRIMARY KEY), never the
-- raw secret, so a database dump exposes no usable tokens. family_id
-- groups every token descended from one login: when a consumed token
-- is replayed (theft), PostgresRefreshTokenStore.Rotate deletes the
-- whole family. expires_at is Unix nanoseconds so expiry comparisons
-- are integer math independent of the database session time zone.
CREATE TABLE IF NOT EXISTS refresh_tokens (
    token_hash TEXT    PRIMARY KEY,
    family_id  TEXT    NOT NULL,
    tenant_id  TEXT    NOT NULL,
    expires_at BIGINT  NOT NULL,
    consumed   BOOLEAN NOT NULL DEFAULT FALSE
);

-- Rotation's reuse-revocation deletes by family_id; logout-everywhere
-- and password-reset invalidation delete by tenant_id. Both get a
-- B-tree index so the sweep is a point range rather than a table scan.
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_family ON refresh_tokens(family_id);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_tenant ON refresh_tokens(tenant_id);

-- dedicated_cells persists the operator-allocated cells the B2B /
-- sovereign console surface lists for tenants whose contract type
-- is b2b_dedicated or sovereign. Provisioning requests insert a
-- row in the "provisioning" state which is later flipped to
-- "active" once the operator-side bring-up workflow completes.
CREATE TABLE IF NOT EXISTS dedicated_cells (
    cell_id            TEXT        PRIMARY KEY,
    tenant_id          TEXT        NOT NULL,
    region             TEXT        NOT NULL,
    country            TEXT        NOT NULL,
    status             TEXT        NOT NULL,
    capacity_petabytes DOUBLE PRECISION NOT NULL DEFAULT 0,
    utilization        DOUBLE PRECISION NOT NULL DEFAULT 0,
    erasure_profile    TEXT        NOT NULL DEFAULT '',
    node_count         INTEGER     NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_dedicated_cells_tenant ON dedicated_cells(tenant_id);
