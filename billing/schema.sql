-- ZK Object Fabric — billing schema for ClickHouse.
--
-- Applied by operators before pointing a gateway at this ClickHouse
-- cluster. The sink in billing/clickhouse_sink.go INSERTs into
-- usage_events; the aggregated roll-up lives in usage_counters.
--
-- Replace {{database}} with the target database name before applying.

CREATE TABLE IF NOT EXISTS {{database}}.usage_events (
    tenant_id      LowCardinality(String),
    bucket         LowCardinality(String),
    dimension      LowCardinality(String),
    delta          UInt64,
    observed_at    DateTime64(3, 'UTC'),
    source_node_id LowCardinality(String)
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(observed_at)
ORDER BY (tenant_id, bucket, dimension, observed_at);

CREATE TABLE IF NOT EXISTS {{database}}.usage_counters (
    tenant_id     LowCardinality(String),
    bucket        LowCardinality(String),
    dimension     LowCardinality(String),
    value         UInt64,
    period_start  DateTime,
    period_end    DateTime
) ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(period_start)
ORDER BY (tenant_id, bucket, dimension, period_start);
