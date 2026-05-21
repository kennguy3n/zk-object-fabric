-- migration_jobs — distributed coordination table for the
-- FleetOrchestrator.
--
-- The table is the canonical source of truth for the migration
-- queue once multiple gateway nodes start running the
-- background_rebalancer. Every claim/heartbeat/release is a
-- single SQL UPDATE; Postgres' row-level locking + the
-- conditional WHERE clauses on (claimed_by IS NULL OR
-- claimed_until < now()) guarantee that two nodes calling
-- AcquireJob on the same job_id at the same instant see exactly
-- one success and one failure.
--
-- TTL semantics: a successful AcquireJob writes claimed_until =
-- now() + ttl. The owning node must call HeartbeatJob before
-- claimed_until passes the wall clock, or another node's
-- AcquireJob succeeds. This is the crash-recovery primitive: no
-- gateway can hold a job forever without active liveness, and
-- there is no separate "expire" RPC the orchestrator has to run.
--
-- The (state, claimed_until) index keeps ListActiveJobs cheap
-- once the table grows; production fleets are expected to
-- accumulate thousands of completed rows before any retention
-- sweep runs.

CREATE TABLE IF NOT EXISTS migration_jobs (
    job_id            TEXT        NOT NULL PRIMARY KEY,
    tenant_id         TEXT        NOT NULL,
    bucket            TEXT        NOT NULL,
    source_backend    TEXT        NOT NULL,
    dest_cell_id      TEXT        NOT NULL,
    dest_backend      TEXT        NOT NULL,
    bytes_per_second  BIGINT      NOT NULL DEFAULT 0,
    state             TEXT        NOT NULL DEFAULT 'pending',
    -- claim ownership. NULL when the row is pending or already
    -- terminal; populated only while a node holds the claim.
    claimed_by        TEXT        NULL,
    -- wall-clock instant at which the claim expires. A claim
    -- with claimed_until < now() is recoverable by any node.
    claimed_until     TIMESTAMPTZ NULL,
    -- progress counters; updated by ReleaseJob.
    bytes_copied      BIGINT      NOT NULL DEFAULT 0,
    pieces_copied     INT         NOT NULL DEFAULT 0,
    started_at        TIMESTAMPTZ NULL,
    completed_at      TIMESTAMPTZ NULL,
    error             TEXT        NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS migration_jobs_state_claim
    ON migration_jobs (state, claimed_until);
