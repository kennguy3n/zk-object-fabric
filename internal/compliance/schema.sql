-- Compliance audit trail. Append-only by design; no UPDATE / DELETE
-- privileges should be granted to the gateway role on this table.
--
-- The (tenant_id, recorded_at) index supports the canonical
-- "what did tenant X do between dates Y and Z" query that
-- Compliance and the data-residency dashboards rely on.
CREATE TABLE IF NOT EXISTS compliance_audit (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT        NOT NULL,
    operation       TEXT        NOT NULL,
    bucket          TEXT        NOT NULL,
    object_key      TEXT        NOT NULL,
    piece_id        TEXT        NOT NULL,
    piece_backend   TEXT        NOT NULL,
    backend_country TEXT        NOT NULL,
    request_id      TEXT        NOT NULL,
    recorded_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_compliance_audit_tenant_time
    ON compliance_audit (tenant_id, recorded_at);

-- Tenants can keep a country whitelist; the residency enforcer
-- joins against this table to short-circuit PUTs that would land
-- in the wrong jurisdiction. Empty rows mean "no restrictions".
CREATE TABLE IF NOT EXISTS tenant_country_allowlist (
    tenant_id  TEXT NOT NULL,
    country    TEXT NOT NULL,
    PRIMARY KEY (tenant_id, country)
);
