# Tenant Setup Runbook

End-to-end procedure for onboarding a new tenant via the console
API or CLI. This is the mechanical companion to the
workload-shaped [`beta-onboarding.md`](./beta-onboarding.md).

## Prerequisites

- Console API endpoint and admin bearer token
  (`config.console.admin_token`).
- The target Wasabi region (or local DC cell) is provisioned and
  reachable from the gateway fleet.
- The control-plane Postgres is up and the auth, tenant,
  placement, and dedicated-cell schemas are applied (see
  [`api/console/schema.sql`](../../api/console/schema.sql)).

## 1. Create the tenant

Tenants are created either via the SPA signup flow (B2C) or via
the operator-side admin API (B2B / sovereign). For operator
flows:

```bash
curl -fsS -X POST \
  -H "Authorization: Bearer $ZKOF_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{
    "id":            "tenant-acme-co",
    "display_name":  "Acme Corporation",
    "contract_type": "b2b_dedicated",
    "primary_email": "ops@acme.example"
  }' \
  "$CONSOLE_URL/api/admin/tenants"
```

Valid `contract_type` values: `b2c`, `b2b_dedicated`, `sovereign`.
Only `b2b_dedicated` and `sovereign` may have dedicated cells.

For B2C self-service signup, point the customer at the SPA
signup form and the gateway's
[`POST /api/v1/auth/signup`](../../api/console/auth_handler.go)
will create the tenant + user atomically.

## 2. Configure placement policy

Placement controls *where* the tenant's data lands. Set this
before the customer issues their first PUT — placement edits on
existing data require a migration via the rebalancer.

```bash
curl -fsS -X PUT \
  -H "Authorization: Bearer $ZKOF_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{
    "provider":           "wasabi-us-east-1",
    "region":             "us-east-1",
    "country_allow_list": ["US"],
    "erasure_profile":    "6+2",
    "encryption_mode":    "managed",
    "cdn_shielding":      false
  }' \
  "$CONSOLE_URL/api/tenants/tenant-acme-co/placement"
```

Field reference:

| Field                | Type     | Notes                                                                  |
| -------------------- | -------- | ---------------------------------------------------------------------- |
| `provider`           | string   | Registry key: `wasabi`, `wasabi-<region>`, `ceph_rgw`, `aws_s3`, etc.  |
| `region`             | string   | Provider-side region (e.g. `us-east-1`).                               |
| `country`            | string   | ISO-3166 alpha-2; tightens the placement to a single country.          |
| `country_allow_list` | string[] | Hard allow-list of countries; reject writes that resolve elsewhere.    |
| `erasure_profile`    | string   | `6+2`, `8+3`, `10+4`, `12+4`, `16+4`. Empty = single-piece replication.|
| `encryption_mode`    | string   | `managed` (gateway holds CMK) or `strict_zk` (customer holds CMK).     |
| `cdn_shielding`      | bool     | Reject direct-to-origin requests; require CDN-allowlisted headers.     |
| `cmk_uri`            | string   | Per-tenant CMK override; defaults to gateway-wide `encryption.cmk_uri`.|

The placement engine
([`metadata/placement_policy/`](../../metadata/placement_policy/))
selects the provider and EC profile per PUT; the migration
state machine handles transitions when policy changes affect
existing data.

## 3. Issue API keys

```bash
curl -fsS -X POST \
  -H "Authorization: Bearer $ZKOF_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{}' \
  "$CONSOLE_URL/api/tenants/tenant-acme-co/keys"
```

Response (one-time secret reveal):

```json
{
  "access_key": "ZKAK6F4U2P9R8G",
  "secret_key": "..."
}
```

The secret is shown **once**. The console UI surfaces this in the
[`APIKeysPage`](../../frontend/src/pages/APIKeysPage.tsx) and
masks it on every subsequent visit. Operator-issued keys are
identical to user-issued keys; the only difference is the audit
trail records `issued_by`.

To list / rotate / revoke keys:

```bash
# list (no secrets)
curl -fsS \
  -H "Authorization: Bearer $ZKOF_ADMIN_TOKEN" \
  "$CONSOLE_URL/api/tenants/tenant-acme-co/keys"

# revoke
curl -fsS -X DELETE \
  -H "Authorization: Bearer $ZKOF_ADMIN_TOKEN" \
  "$CONSOLE_URL/api/tenants/tenant-acme-co/keys/ZKAK6F4U2P9R8G"
```

## 4. Set budget limits

Budgets control the per-tenant rate limiter and the abuse guard.
Both live on the `tenant.Budgets` record and are reloaded on the
next request after a write.

```bash
curl -fsS -X PUT \
  -H "Authorization: Bearer $ZKOF_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{
    "requests_per_sec":  2000,
    "burst_requests":    5000,
    "egress_tb_month":   10
  }' \
  "$CONSOLE_URL/api/admin/tenants/tenant-acme-co/budgets"
```

Behavior:

- `requests_per_sec` + `burst_requests` drive the
  [`internal/auth/rate_limit.go`](../../internal/auth/rate_limit.go)
  token-bucket; over-budget requests get HTTP 429 and emit
  `AbuseBudgetExhausted`.
- `egress_tb_month` is enforced as a sliding monthly window.
  Over-budget reads return HTTP 429 with a
  `Retry-After` hinting at the next billing-period reset.
- The abuse guard's anomaly detector
  ([`internal/auth/abuse.go`](../../internal/auth/abuse.go))
  layers on top — even within budget, a 2x deviation from the
  EWMA baseline emits `AbuseAnomalyAlert` and (if
  `config.abuse.throttle_on_anomaly=true`) throttles for the
  configured cooldown.

## 5. Monitor tenant usage

The console SPA's dashboard
([`DashboardPage.tsx`](../../frontend/src/pages/DashboardPage.tsx))
streams real-time usage via SSE at
`/api/v1/usage/stream/{tenantID}`.

For ClickHouse-side queries (the ground truth):

```sql
-- Storage in the last hour
SELECT
  tenant_id,
  toStartOfHour(timestamp) AS hour,
  sumIf(value, dimension='Stored') AS stored_bytes,
  sumIf(value, dimension='Puts')   AS puts,
  sumIf(value, dimension='Gets')   AS gets,
  sumIf(value, dimension='EgressBytes') AS egress_bytes
FROM zkof.usage_events
WHERE tenant_id = 'tenant-acme-co'
  AND timestamp > now() - INTERVAL 24 HOUR
GROUP BY hour, tenant_id
ORDER BY hour DESC;
```

```sql
-- Top-10 buckets by egress in the last 7 days
SELECT
  bucket,
  sumIf(value, dimension='EgressBytes') AS bytes
FROM zkof.usage_events
WHERE tenant_id = 'tenant-acme-co'
  AND timestamp > now() - INTERVAL 7 DAY
GROUP BY bucket
ORDER BY bytes DESC
LIMIT 10;
```

## 6. Optional: dedicated cell

Only for `contract_type=b2b_dedicated` or `sovereign`. See
[`deploy/cell-provisioner/README.md`](../../deploy/cell-provisioner/README.md)
for the end-to-end cell provisioning flow. After the cell is
provisioned, repeat step 2 with `provider: ceph_rgw` and
`cell: <cell_id>` so the placement engine pins the tenant's
data to that cell.

## 7. Hand-off checklist

Before declaring the tenant onboarded:

- [ ] Tenant record exists in the tenant store
      (`SELECT id, contract_type FROM tenants WHERE id='tenant-acme-co';`).
- [ ] Placement policy applied
      (`GET /api/tenants/tenant-acme-co/placement` returns the expected JSON).
- [ ] At least one API key issued and verified via a probe PUT/GET.
- [ ] Budget limits set and acknowledged by the customer.
- [ ] Monitoring dashboard rendering for the tenant
      (open the SPA as the customer's first admin user, hit
      the dashboard, confirm SSE updates appear within 5 seconds).
- [ ] Beta-onboarding archetype-specific checklist completed
      (see [`beta-onboarding.md`](./beta-onboarding.md)).
- [ ] CMK rotation runbook acknowledged by the customer's
      security team
      ([`cmk-rotation.md`](./cmk-rotation.md)).

## 8. Decommission

```bash
# Stops new writes
curl -fsS -X PATCH \
  -H "Authorization: Bearer $ZKOF_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"status":"frozen"}' \
  "$CONSOLE_URL/api/admin/tenants/tenant-acme-co"

# After data migration / export, hard-delete
curl -fsS -X DELETE \
  -H "Authorization: Bearer $ZKOF_ADMIN_TOKEN" \
  "$CONSOLE_URL/api/admin/tenants/tenant-acme-co"
```

The hard-delete cascades through the manifest store (manifest
rows go to `state='deleted'`) and the rebalancer eventually
drops the on-disk pieces. The auth-store's `auth_users` row is
deleted synchronously so the customer's email is freed for
re-registration.
