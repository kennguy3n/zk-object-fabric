# Beta Customer Onboarding Runbook

This runbook covers the five Phase 3 beta workload archetypes the
fabric is targeting. For each, it lays out the placement policy,
expected egress profile, billing-budget defaults, and the
operator-side checklist.

The companion runbook
[`docs/runbooks/tenant-setup.md`](./tenant-setup.md) covers the
mechanical "how to create a tenant via the console API" steps;
this runbook is the workload-shaped overlay on top of those steps.

## 1. Backup workloads

**Profile:** very large sequential writes, infrequent reads,
egress only on restore.

**Placement policy:**

```yaml
provider:           wasabi-us-east-1
region:             us-east-1
country:            US
erasure_profile:    8+3        # high durability, modest overhead
encryption_mode:    managed    # gateway-managed CMK
multipart:          required   # > 256 MiB writes
```

**Budgets:**

```json
{
  "requests_per_sec":  500,
  "burst_requests":    1000,
  "egress_tb_month":   2,
  "storage_tb_max":    50
}
```

**Onboarding checklist:**

1. Create tenant with `contract_type=b2c` (or `b2b_dedicated` if
   the customer wants their own cell).
2. Apply placement policy above.
3. Issue one API key for the customer's backup agent.
4. Verify multipart works against the gateway: a 1 GiB upload
   should complete in 4-part chunks via `aws s3 cp` with
   `--multipart-threshold 256MB`.
5. Set billing budget alerts at 50%, 80%, 100% of egress and
   storage caps.

**Avoid:** a backup tenant with surprise egress (e.g., a restore
that re-reads 100% of the dataset). Surface the
[`AbuseAnomalyAlert`](../../internal/auth/abuse.go) early so
operators can authorize the spike before the abuse guard
throttles.

---

## 2. SaaS asset storage

**Profile:** mixed read/write, moderate egress (CDN-shielded for
public assets), small-to-medium objects.

**Placement policy:**

```yaml
provider:           wasabi-us-east-1
region:             us-east-1
country:            US
erasure_profile:    6+2        # good durability, lower overhead than backup
encryption_mode:    managed
cdn_shielding:      true       # require requests via the SaaS CDN
```

**Budgets:**

```json
{
  "requests_per_sec":  2000,
  "burst_requests":    5000,
  "egress_tb_month":   10
}
```

**Onboarding checklist:**

1. Create tenant.
2. Apply placement; set `tenant.abuse.cdn_shielding=true` so the
   gateway's `AbuseGuard.cdn_shielded` rejects direct-to-origin
   requests with HTTP 403.
3. Document the CDN's allowlist headers (e.g.
   `X-CDN-Token: <secret>`) in `cfg.Abuse.CDNHeaders`.
4. Issue one API key for the SaaS app's server-side writers,
   plus presigned-URL-only access for the SaaS app's clients.
5. Confirm presigned GET / PUT works through the gateway's
   `s3compat` v4 query-string presigning (PR #24 wired this).

---

## 3. AI dataset storage

**Profile:** very large objects (multi-GB shards), sequential
reads at high throughput, write-once / read-many.

**Placement policy:**

```yaml
provider:           wasabi-us-west-1
region:             us-west-1
country:            US
erasure_profile:    10+4       # higher durability, low parity overhead per shard
encryption_mode:    managed
multipart:          required
range_get:          aligned    # stride-aligned to the EC stripe size
```

**Budgets:**

```json
{
  "requests_per_sec":  200,
  "burst_requests":    400,
  "egress_tb_month":   100
}
```

**Onboarding checklist:**

1. Create tenant. Set `contract_type=b2b_dedicated` if the
   customer wants their own cell (most AI customers do, for
   data residency).
2. If dedicated cell, run
   [`deploy/cell-provisioner/`](../../deploy/cell-provisioner/) to
   stand up a Ceph RGW cell first; placement targets `ceph_rgw`.
3. Apply the placement policy.
4. Confirm range GET stride-alignment works: a 4 GiB object
   stored under EC 10+4 with 16 MiB stripe size should serve a
   `bytes=0-1048575` range without fetching the whole stripe.
5. Pre-warm the gateway's NVMe cache by triggering a
   `PromotionSignal` for the customer's hot training set.
6. Set egress alerts at 80% and 100% — AI training jobs can blow
   through a 100 TB monthly budget in days.

**Avoid:** a customer who plans to do random small-object reads
out of an AI dataset bucket. The placement policy assumes
sequential range GETs; random small-object reads have
proportionally worse cache miss ratios.

---

## 4. Media library

**Profile:** hot reads, CDN-shielded egress, mid-size objects
(images, video segments).

**Placement policy:**

```yaml
provider:           wasabi-us-east-1
region:             us-east-1
country:            US
erasure_profile:    6+2
encryption_mode:    managed
cdn_shielding:      true
hot_object_promotion: aggressive  # promote on first miss, not just on second
```

**Budgets:**

```json
{
  "requests_per_sec":  10000,
  "burst_requests":    50000,
  "egress_tb_month":   50
}
```

**Onboarding checklist:**

1. Create tenant.
2. Configure CDN shielding (same as SaaS profile).
3. Apply placement with aggressive hot-object promotion so video
   segments land in the cache after the first miss.
4. Watch the cache hit ratio — for video traffic it should
   stabilize > 85% within 24 hours of warm-up. If it doesn't,
   the customer's catalog is too big for the configured cache
   capacity; either grow the cache or accept higher origin egress.

---

## 5. Sovereign storage (B2B dedicated cell)

**Profile:** full data residency, country-locked, customer-managed
or HSM-rooted CMK, no traffic outside the customer's geography.

**Placement policy:**

```yaml
provider:           ceph_rgw
region:             local-dc-eu-1
country:            DE          # ISO-3166 alpha-2
country_allow_list: ["DE"]      # hard reject if any other country appears
cell:               cell-eu-1
erasure_profile:    6+2
encryption_mode:    managed     # or strict_zk if customer holds the keys
cmk_uri:            vault://vault.eu.zkof.local:8200/transit/cell-eu-1
```

**Budgets:** customer-defined; budget enforcement on a sovereign
cell is primarily for their internal accounting, not for fair-use
shaping.

**Onboarding checklist:**

1. Provision the cell first — this is a multi-day operator task
   covered in [`deploy/local-dc/`](../../deploy/local-dc/) and
   [`deploy/cell-provisioner/`](../../deploy/cell-provisioner/).
2. Create tenant with `contract_type=sovereign`.
3. Apply placement policy targeting the cell's `ceph_rgw`
   provider.
4. If the customer holds their own CMK, configure
   `encryption.cmk_uri` per their preferred wrapper (Vault for
   on-prem, KMS for cloud-hosted-key escrow); see
   [`docs/runbooks/cmk-rotation.md`](./cmk-rotation.md).
5. Issue API keys via the console; offer a hardware-token
   issuance path (TOTP-protected admin role) if the customer
   requires it.
6. Run `go test -v -run TestSuite_CephRGW
   ./tests/s3_compat/` against the new cell's RGW endpoint and
   archive the test log under
   `docs/audits/${cell_id}-compliance.log` for the customer's
   compliance team.
7. Set up monitoring exports: Prometheus scrape from the cell's
   `ceph_exporter` to the customer's Prometheus, and the
   gateway's `/internal/metrics` (Phase 4) to the customer's
   observability stack.

**Avoid:** a sovereign customer who also wants their data
replicated cross-country. That is a contradiction; refuse the
ask, document the trade-off, and offer a same-country
multi-cell topology instead.

---

## Operator workflow per beta cohort

For each cohort of beta customers:

1. Pre-flight: confirm the target cell (Wasabi region or local
   DC cell) has capacity and is in `HEALTH_OK`.
2. Onboarding session: walk the customer through their
   placement policy and budget caps. Do this synchronously —
   beta customers should not self-serve placement edits yet.
3. Apply the policy via the console (or via
   [`deploy/cell-provisioner/apply_placement.sh`](../../deploy/cell-provisioner/apply_placement.sh)).
4. Issue API keys.
5. Run a synthetic smoke workload appropriate to the archetype
   (e.g., for AI: 100 GB of 256 MiB shards via multipart).
6. Hand off the customer's monitoring view: the
   [`DashboardPage`](../../frontend/src/pages/DashboardPage.tsx)
   shows storage / requests / egress in real time via the SSE
   `/api/v1/usage/stream/{tenantID}` stream.
7. Schedule a 30-day retro to review actual vs projected usage
   and resize budgets / placement.

## Escalation paths

- Egress anomaly: `internal/auth/abuse.go` emits
  `AbuseAnomalyAlert` → routed to the configured webhook.
- Cell health degraded: `internal/health/health.go` flips
  `/internal/ready` to 503 → NodeBalancer pulls the node out;
  PagerDuty fires off the CloudWatch
  `zkof-gateway-cell-quorum` alarm.
- Billing flush failure: ClickHouse sink retries with backoff;
  on persistent failure the
  [`zkof-billing-flush-failure`](../../deploy/aws/terraform/cloudwatch/main.tf)
  alarm fires.
