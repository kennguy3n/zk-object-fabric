# AWS Control Plane

Phase 3 control plane provisioning for ZK Object Fabric on AWS.
This directory ships Terraform modules and CloudWatch dashboard
JSON for a beta-cell deployment.

## Components

| Module                       | Resources                                                                 |
| ---------------------------- | ------------------------------------------------------------------------- |
| `terraform/rds/`             | PostgreSQL 16 RDS instance (manifest + tenant + auth + placement + cells) for the control plane DSN. |
| `terraform/iam/`             | Gateway role + console role + KMS key policy bindings.                    |
| `terraform/clickhouse/`      | ClickHouse Cloud connection secrets, or self-hosted EC2 cluster scaffold. |
| `terraform/cloudwatch/`      | Log groups, alarms, and dashboards for gateway + console + billing.       |
| `terraform/secgroups/`       | Security groups: gateway↔RDS, gateway↔ClickHouse, console↔RDS.            |
| `dashboards/gateway.json`    | CloudWatch dashboard: p99 latency, cache hit ratio, billing throughput.   |
| `dashboards/abuse.json`      | CloudWatch dashboard: anomaly alerts, throttled request rate, 5xx rate.   |
| `policies/gateway-role.json` | IAM policy: KMS decrypt, S3 access, ClickHouse write, RDS connect.        |

## Prerequisites

- AWS account + credentials with the `iam:*`, `rds:*`, `kms:*`,
  `cloudwatch:*`, `ec2:*` permissions needed by the modules.
- A pre-existing VPC with at least three private subnets across
  AZs (we do not create the VPC here; reuse the org's standard
  network).
- Terraform 1.6+.
- The Wasabi origin already provisioned via `deploy/wasabi/`.

## Quick start

```bash
cd terraform
terraform init
terraform plan -var-file=prod.tfvars
terraform apply -var-file=prod.tfvars
```

Each sub-module is independently composable — you can apply
`rds/` first to bring up the metadata DB, wire that DSN into the
gateway snapshot, then apply `cloudwatch/` once the gateway is
emitting metrics. Variables are documented in each module's
`variables.tf`.

## Outputs (for the gateway config)

After `terraform apply`, the modules emit:

- `rds_endpoint` — paste into `config.control_plane.metadata_dsn`
  (along with the password from Secrets Manager).
- `kms_key_arn` — paste into `config.encryption.cmk_uri` (e.g.
  `arn:aws:kms:us-east-1:123456789012:key/abc...`).
- `clickhouse_url` — paste into `config.billing.clickhouse_url`.
- `gateway_role_arn` — attach to gateway EC2 instances or to the
  Linode instance profile via instance-profile mediation (pick
  one: AWS-side IAM role with the gateway's machine identity,
  *or* Wasabi-side static keys, but not both, to avoid
  multi-credential drift).

## Connection-pool tuning

The control plane config ships with sane defaults for the
gateway's RDS pool:

```json
"control_plane": {
  "metadata_dsn":      "postgres://zkof:...@zkof-prod.cluster-xyz.us-east-1.rds.amazonaws.com:5432/zkof?sslmode=require",
  "max_open_conns":    32,
  "max_idle_conns":    8,
  "conn_max_lifetime": "30m",
  "conn_max_idle_time": "5m"
}
```

The gateway opens exactly one `*sql.DB` per metadata DSN and shares
it across all five Postgres-backed stores (manifest, tenant, auth,
placement, dedicated cell), so `max_open_conns` is the
gateway-process-wide cap on metadata connections — not a per-store
multiplier. Sizing rule of thumb: `max_open_conns ≈ 2× CPU` per
gateway process; multiply by the gateway-fleet size for total
connections against RDS, then confirm that's well below the
instance's `max_connections` (or, with RDS Proxy, the proxy's
client connection limit).

The `conn_max_lifetime` should sit comfortably under RDS Proxy's
default 10-minute idle timeout; if the gateway fleet is wired
through RDS Proxy, set both `conn_max_lifetime` and
`conn_max_idle_time` to 5 minutes so terminated connections are
recycled before the proxy reaps them. See
[`internal/config/config.go`](../../internal/config/config.go) →
`ControlPlaneConfig` for the full field set.

## Alarms

The CloudWatch alarms ship at conservative thresholds suitable for
beta:

| Alarm                                | Trigger                                                |
| ------------------------------------ | ------------------------------------------------------ |
| `zkof-gateway-5xx-rate`              | `Sum(5xx) / Sum(requests) > 1%` over 5 minutes         |
| `zkof-gateway-cache-miss-rate`       | `cache_miss / (cache_miss + cache_hit) > 50%` over 15m |
| `zkof-billing-flush-failure`         | Any non-zero `billing_flush_errors` over 5 minutes     |
| `zkof-abuse-anomaly-rate`            | `> 5` `AbuseAnomalyAlert` events / minute, 3 datapoints|
| `zkof-rds-connections-saturation`    | RDS `DatabaseConnections > 80% of max` for 10 minutes  |

Wire the alarm topics through SNS to the same destination
configured in `config.abuse.alert_webhook_url`
(PagerDuty / Slack) so on-call sees one signal stream rather
than two.

## See also

- [`deploy/wasabi/`](../wasabi/) — origin bucket provisioning.
- [`deploy/linode/`](../linode/) — gateway data plane fleet.
- [`docs/runbooks/cmk-rotation.md`](../../docs/runbooks/cmk-rotation.md) — KMS rotation procedure.
