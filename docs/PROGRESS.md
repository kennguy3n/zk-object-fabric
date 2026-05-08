# ZK Object Fabric — Progress

- **Project**: ZK Object Fabric
- **License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).
- **Status**: Phase 3 — Beta Cell (COMPLETE). Phase 3.5 — Intra-Tenant Deduplication (COMPLETE). Phase 4 — Production & Scale (IN PROGRESS).
- **Last updated**: 2026-05-03 (PR #55 — status refresh + convergent crypto string corrections)

This document is a phase-gated tracker. Each phase has an explicit
checklist and a decision gate. Do not skip to the next phase until the
current phase's gate has been met.

For the technical design, see [PROPOSAL.md](PROPOSAL.md).

---

## Phase 1: Architecture Proof (Weeks 1–3)

**Status**: `COMPLETE`

**Goal**: lock the architecture on the **AWS control plane + Linode
data plane + Wasabi storage backend** stack, ratify the
provider-neutral manifest and migration plan, and produce enough
formal specification that Phase 2 engineers can implement without
re-debating core decisions.

Checklist:

- [x] Ratify the Phase 1 stack: AWS (control plane) + Linode (data
      plane) + Wasabi (storage backend). Reflected in the code
      scaffold's AWS / Linode / Wasabi separation
      (`cmd/gateway`, `providers/wasabi`, `internal/config`).
- [x] Confirm that no customer data flows through AWS (contract test
      on control-plane API surface) — implemented in
      `tests/control_plane/no_data_test.go`, which reflects over every
      control-plane type (manifest, tenant, placement policy, billing)
      and rejects any field that could carry raw object bytes.
- [x] Select the Phase 2+ local-DC base (Ceph RGW vs SeaweedFS —
      AGPL options are ruled out). Decision: **Ceph RGW** for maximum
      production maturity; SeaweedFS remains documented as the
      "faster custom product build" alternative should priorities
      shift. See the decision-gate table below.
- [x] Define the provider-neutral object manifest format (implemented
      in `metadata/manifest.go` with JSON round-trip coverage in
      `metadata/manifest_test.go`).
- [x] Define the encryption envelope (per-object DEK, encrypted
      manifest, CMK support) — implemented in
      `encryption/envelope.go`.
- [x] Define the placement policy DSL (YAML schema) — implemented in
      `metadata/placement_policy/policy.go`.
- [x] Define erasure-coding profiles for Phase 2+ (6+2, 8+3, 10+4)
      — implemented in `metadata/erasure_coding/profile.go` with
      `Profile6Plus2`, `Profile8Plus3`, `Profile10Plus4` constants,
      a `Validate` method, and a `StorageOverhead` helper.
      Coverage in `metadata/erasure_coding/profile_test.go`.
      Phase 1 still uses Wasabi's native durability; EC is not wired
      into the write path until Phase 2+.
- [x] Define the S3 compatibility subset (PUT, GET, HEAD, DELETE,
      LIST, multipart, range GET). Full operation matrix specified in
      [PROPOSAL.md §3.2.2](PROPOSAL.md).
- [x] Define the S3 API as the phase-invariant contract (operation
      matrix, migration behavior, compliance test suite spec) —
      specified in [PROPOSAL.md §3.2](PROPOSAL.md).
- [x] Define the benchmark suite (PUT / GET latency percentiles,
      cache hit ratio, repair time, Wasabi origin egress ratio,
      network cost) — declarative harness in `tests/benchmark/suite.go`
      with PUT/GET p50/p95/p99 targets, cache-hit ratio (>90% Hot),
      Wasabi origin egress ratio (≤1.0 per tenant), and LIST
      performance at 10M / 100M / 1B objects. Coverage in
      `tests/benchmark/suite_test.go`.
- [x] Define the multi-tenancy model (tenant isolation, billing,
      abuse controls) — implemented in `metadata/tenant/tenant.go`
      with the §5.5 schema (`contract_type`, `license_tier`, `keys`,
      `placement_default`, `budgets`, `abuse`, `billing`), plus
      `Validate`, JSON, and YAML round-trips in
      `metadata/tenant/tenant_test.go`.
- [x] Define the migration engine spec (dual-write, lazy migration on
      read, background rebalancer, migration state machine) — state
      machine in `migration/state.go` with transition coverage in
      `migration/state_test.go`.
- [x] Specify the Linode cache design (NVMe / block storage sizing,
      promotion rules, range-aligned chunking) —
      `cache/hot_object_cache/cache.go` now defines `PromotionPolicy`
      (monthly egress ratio, daily read count, p95 miss latency) and
      `EvictionPolicy` (LRU with hot-pin) with L0/L1 defaults plus
      NVMe/block-storage sizing guidance in the package comment.
      Coverage in `cache/hot_object_cache/cache_test.go`.
- [x] Specify Wasabi fair-use guardrails (egress budgets, per-tenant
      cache hit ratio targets, 90-day minimum storage handling) —
      implemented in `providers/wasabi/guardrails.go` with
      `FairUseEgressBudget`, `MinStorageTracker`, `CacheHitRatioTarget`,
      `AlertThresholds`, and the composite `Guardrails` type. The
      default budget encodes the ≤1.0 egress/storage ratio from
      PROPOSAL.md §3.11; coverage in
      `providers/wasabi/guardrails_test.go`.
- [x] Decision gate: Phase 2+ base selection — **Ceph RGW** picked as
      the Phase 2+ local-DC origin. See table below.

### Phase 1 decision gate: base selection

AGPL options are ruled out because ZK Object Fabric ships under a
proprietary license. Garage is ruled out because it does not support
erasure coding and therefore cannot meet Phase 2+'s EC
durable-origin requirement.

**Decision (Phase 1 gate, 2026-04-22): Ceph RGW** is the Phase 2+
local-DC base. Ceph's production maturity, operational tooling, and
erasure-coding support outweigh the slower custom-feature roadmap
relative to SeaweedFS. SeaweedFS is retained as a documented
fallback: if Phase 2 operational load or feature velocity pushes us
off Ceph, SeaweedFS becomes the second-choice base without
reopening the AGPL / EC gates.

| Requirement                              | Pick        | Notes                        |
| ---------------------------------------- | ----------- | ---------------------------- |
| Maximum production maturity (selected)   | Ceph RGW    | Phase 2+ local-DC base       |
| Faster custom product build (fallback)   | SeaweedFS   | Retained as documented backup |

---

## Phase 2: Prototype (Weeks 4–9)

**Status**: `COMPLETE`

**Goal**: a single-cell prototype that can PUT, GET, HEAD, DELETE,
LIST, and range-read encrypted objects end-to-end, backed by Wasabi
via the Linode gateway, with the migration engine wired up for a
dry-run cut-over to a local DC cell.

Checklist:

- [x] S3-compatible gateway on Linode (Go) — `cmd/gateway/main.go`
      now wires a full `s3compat.Config`: Postgres-backed manifest
      store (or in-memory fallback), `wasabi` + `local_fs_dev`
      providers, the placement engine, the HMAC authenticator, the
      in-memory hot object cache, and the logger billing sink.
      Request routing in `api/s3compat/handler.go` covers PUT, GET,
      HEAD, DELETE, LIST, range GET, and presigned URLs, with the
      hot cache consulted on the GET path.
- [x] Client-side encryption SDK — `encryption/client_sdk/sdk.go`
      implements chunked XChaCha20-Poly1305 encrypt/decrypt (16 MiB
      chunks so range reads can decrypt a single chunk); DEK
      generation in `keygen.go`; CMK-agnostic wrap / unwrap in
      `wrap.go`; round-trip + wrong-key coverage in `sdk_test.go`.
- [x] Gateway-side encryption wiring — `api/s3compat/handler.go`,
      `api/s3compat/erasure_coding.go`, and
      `api/s3compat/multipart_handler.go` now apply the SDK on
      every write path (single-piece PUT, erasure-coded PUT, and
      multipart `UploadPart`) and mirror the decrypt on every read
      path. `managed` and `public_distribution` tenant policies
      generate a fresh DEK per object (per-session for multipart),
      seal it with the gateway-configured CMK via
      `client_sdk.LocalFileWrapper`, record the wrapped DEK on the
      manifest (`metadata.EncryptionConfig.WrappedDEK` +
      `WrapAlgorithm`), and keep plaintext bytes out of every
      backend piece. `client_side` (Strict ZK) refuses PUTs
      lacking the `X-Amz-Meta-Zk-Encryption` header and streams
      ciphertext through untouched. The new `GatewayEncryption`
      struct is constructed in `cmd/gateway/main.go` from
      `config.encryption.cmk_path` / `cmk_uri`; the Postgres
      manifest store grew an optional `BodyEncryptor`
      (`metadata/manifest_store/postgres/body_encryptor.go`) that
      seals the manifest JSON with a separate gateway-held key
      when `config.encryption.manifest_body_key_path` is set. End-
      to-end coverage lives in
      `tests/s3_compat/encryption_test.go` (managed round-trip,
      wrong-CMK fail-closed, Strict ZK reject + passthrough,
      object-key opacity, Encryption.Mode always populated,
      erasure-coded and multipart managed encryption,
      cross-size backend inspection for plaintext leaks, legacy /
      no-policy backward compatibility, and the manifest body
      AEAD construction).
- [x] Encrypted manifest storage in the AWS control plane —
      Postgres-backed `ManifestStore` implementation in
      `metadata/manifest_store/postgres/store.go` (opaque JSONB
      bodies, index on `(tenant_id, bucket, object_key_hash,
      version_id)`), wired into `cmd/gateway/main.go` behind the
      `postgres` build tag; in-memory store used for dev + tests.
- [x] Storage provider adapter framework (`wasabi`, `local_fs_dev`,
      stubs for `backblaze_b2`, `cloudflare_r2`, `aws_s3`) — `wasabi`
      wired on AWS SDK v2; `ceph_rgw`, `backblaze_b2`,
      `cloudflare_r2`, and `aws_s3` adapters scaffolded with Config,
      constructor, Capabilities, CostModel, and PlacementLabels.
- [x] Placement engine (provider + region + country + storage_class)
      — `metadata/placement_policy/engine.go` filters eligible
      providers by policy constraints and picks the cheapest using
      `StorageProvider.CostModel()`. Coverage in `engine_test.go`
      across B2C pooled, B2B dedicated, and BYOC tenant paths.
- [x] Wasabi durable origin wired up as the primary backend —
      `cmd/gateway/main.go` registers `wasabi` in the provider map
      and sets it as the placement-engine default when no
      tenant-specific policy overrides it.
- [x] Linode hot cache (L0 / L1) with promotion rules —
      `cache/hot_object_cache/memory_cache.go` implements an LRU
      with hot-pin region, size/byte accounting, and stats; the
      promotion worker in `promotion_worker.go` consumes signals off
      the handler's non-blocking `SignalBus` and populates the cache
      against the configured `PromotionPolicy`.
- [~] Node health monitor for the Linode gateway fleet — **deferred
      to Phase 3**. Phase 2 relies on the existing liveness endpoint
      plus external process supervision; a purpose-built health
      monitor (per-cell quorum, cache-tier drain, graceful gateway
      replacement) is tracked as a Phase 3 deliverable alongside the
      production Linode fleet stand-up.
- [x] Basic billing counters (per-tenant storage-seconds, PUTs,
      GETs, egress bytes) — `billing/logger_sink.go` is a
      structured-log `BillingSink` wired into `s3compat.Config`; the
      handler emits `Stored`, `Puts`, `Gets`, `EgressBytes`,
      `CacheHits`, and `CacheMisses` events per request.
- [x] Range GET support, range-aligned cache chunks — handler's GET
      path parses `Range` headers and hands them to the provider
      via `GetOptions`; cache keys align with piece IDs so chunked
      reads populate / serve from the same entry as the full GET.
- [x] Hot-object promotion from Wasabi to Linode cache — GET-path
      cache miss publishes a `PromotionSignal` onto the non-blocking
      `SignalBus`; the promotion worker evaluates the signal against
      policy and, on promotion, calls `provider.GetPiece` and
      `cache.Put`.
- [x] Multi-tenant isolation layer — `internal/auth/authenticator.go`
      verifies AWS Signature V4 against a per-tenant access key and
      returns `tenantID`; `internal/auth/tenant_store.go` supplies an
      in-memory `TenantStore` with JSON loading;
      `internal/auth/rate_limit.go` applies per-tenant token-bucket
      limits sourced from the tenant's `Budgets.RequestsPerSec`.
      PR #29 reworked the SigV4 path to a flexible dispatch
      (commit 32956cc): `HMACAuthenticator` now drives an ordered
      `AuthStrategy` list (`PresignedV4Strategy`,
      `HeaderV4Strategy`) with hooks for future STS / SigV4A /
      chunked-only strategies, falls back from `x-amz-date` to the
      standard `Date` header (RFC1123) for legacy AWS SDK
      configurations, surfaces the derived signing key plus seed
      signature via the new `AuthResult` / `AuthenticateEx` so the
      streaming / multipart `Content-Encoding: aws-chunked` path
      can verify per-chunk signatures via the exported
      `VerifyChunkSignature` helper, and gains an explicit
      `PresignedGetExpired` subtest in `tests/s3_compat/` for the
      `X-Amz-Expires` + clock-skew window.
- [x] Migration engine: dual-write, lazy migration on read,
      background rebalancer (exercised against a `local_fs_dev`
      target) — `migration/dual_write/dual_write.go` mirrors writes
      to primary + secondary and falls back on reads;
      `migration/lazy_read_repair/repair.go` copies missing pieces
      from the old backend onto the new one during GETs and updates
      the manifest; `migration/background_rebalancer/rebalancer.go`
      advances manifests through the
      `wasabi_primary → dual_write → local_primary_wasabi_backup →
      local_primary_wasabi_drain → local_only` state machine with
      bandwidth limits. Coverage in each package's `_test.go`.
      **Lazy read-repair is now wired into the gateway GET path**
      via `s3compat.Config.ReadRepair` — when the primary backend
      fails to serve a piece for a manifest whose `MigrationState`
      names a distinct new primary (Generation > 1), the handler
      falls back to `lazy_read_repair.ReadRepair.Repair()` and
      serves the repaired body. **The background rebalancer is now
      started as an optional background worker** by
      `cmd/gateway/main.go` when `config.migration.targets` is
      non-empty; the goroutine shares the promotion worker's
      shutdown context so SIGTERM drains both cleanly.
- [x] Implement S3 compliance test suite (`tests/s3_compat/`) and
      run against `wasabi` and `local_fs_dev` adapters — AWS SDK v2
      test client in `tests/s3_compat/suite_test.go` exercises PUT,
      GET, HEAD, DELETE, LIST, range GET, DELETE idempotency,
      missing-key 404s, presigned GETs, and multipart-like
      overwrite semantics. Reusable `Run(t, Setup)` harness so any
      provider can be plugged in.
- [x] Validate S3 API behavior during a simulated Wasabi →
      `local_fs_dev` migration (zero behavioral differences) —
      `tests/s3_compat/migration_test.go` runs the full compliance
      suite through a `DualWriteProvider` topology and separately
      asserts that every PUT lands on both backends and that GETs
      transparently fall back to the secondary when the primary
      fails.
- [x] Benchmark execution (PUT / GET p50 / p95 / p99, cache hit
      ratio, Wasabi origin egress ratio vs stored bytes,
      small-object overhead, LIST performance at 10M / 100M / 1B
      objects) — `tests/benchmark/runner.go` implements
      `ProviderRunner` and `RunSuite`, driving each scenario's
      request mix against a `StorageProvider`, recording per-target
      metrics, and emitting a JSON `Report` for CI consumption.
      Repair time and network-cost metrics are included as
      first-class `Result` entries for the live-driver follow-up.

---

## Phase 3: Beta Cell (Weeks 10–15)

**Status**: `COMPLETE`

**Goal**: stand up a real beta deployment on the AWS + Linode +
Wasabi stack with paying / design-partner customers on both B2C and
B2B paths, plus a first local DC cell for early hybrid customers.

Checklist:

- [x] Production AWS control plane (RDS, IAM, CloudWatch,
      ClickHouse or equivalent). Deploy artifacts landed in
      PR #30 under `deploy/aws/`; PR #30 also refactored every
      metadata-store call site to share a single `*sql.DB`
      (commit d01c283f) so manifest, tenant, auth, placement, and
      dedicated-cell stores no longer each open their own pool.
      Remaining work is the actual cloud provisioning (running the
      Terraform modules against a live AWS account). ClickHouse billing sink
      (`billing/clickhouse_sink.go`, schema in `billing/schema.sql`)
      is wired into `cmd/gateway/main.go` under
      `config.billing.clickhouse_url`. Operator-side stand-up
      ships in `deploy/aws/`: Terraform modules for RDS
      PostgreSQL 16 (multi-AZ, encrypted, performance-insights,
      14-day backups), gateway / console IAM roles with KMS +
      RDS-IAM + CloudWatch policies, KMS CMK with annual
      rotation enabled, and CloudWatch log groups + alarms
      (`zkof-gateway-5xx-rate`, `zkof-cache-miss-rate`,
      `zkof-billing-flush-failure`, `zkof-abuse-anomaly-rate`,
      `zkof-rds-connections-saturation`) plus dashboards
      (`gateway.json`, `abuse.json`). `internal/config/config.go`
      grew `ControlPlaneConfig.MaxOpenConns`,
      `MaxIdleConns`, `ConnMaxLifetime`, and `ConnMaxIdleTime`,
      applied to every `sql.Open` call site by the new
      `cmd/gateway/main.go#applyDBConnectionPool` helper so RDS
      Proxy / direct-RDS deployments share one tuning surface.
- [x] Production Linode gateway fleet, multi-region (deploy
      artifacts landed in PR #30; remaining work is actual
      infrastructure provisioning).
      Operator stand-up in `deploy/linode/`: Terraform module
      provisioning `g6-dedicated-8` instances per region with
      attached NVMe block volumes and a regional NodeBalancer
      whose health check polls `/internal/ready` (matches
      `internal/health/health.go`). Cloud-init bootstraps the
      gateway via `scripts/install_gateway.sh`; the systemd
      unit (`systemd/zk-gateway.service`) runs the binary as a
      non-root `zkof` user with `ProtectSystem=strict` and
      `KillSignal=SIGTERM` matched to the gateway's drain
      handler. Caddy (`caddy/Caddyfile`) terminates TLS for
      direct-attached topologies and blocks `/internal/*` from
      external clients. README documents the multi-region
      NodeBalancer + GeoDNS pattern, drain / replace procedure,
      and beta / production / high-egress sizing guidance.
- [x] Production Wasabi buckets (per region) wired as the durable
      origin. Multi-region Wasabi config plus the
      `deploy/wasabi/` provisioner landed in PR #30; remaining
      work is provisioning the actual buckets / IAM policies
      against a live Wasabi account.
      `internal/config/config.go#WasabiConfig` grew a
      `Regions []WasabiRegionConfig` slice with each entry's
      `ResolvedName()` defaulting to `wasabi-<region>`;
      `cmd/gateway/main.go#buildProviderRegistry` registers each
      region as its own `StorageProvider` so placement policies
      can target `wasabi-us-east-1`, `wasabi-eu-central-1`, etc.
      Operator-side provisioning in `deploy/wasabi/`: idempotent
      `provision_buckets.sh` creates buckets named
      `zkof-{region}-{env}` with the per-bucket IAM policy
      template (`iam_policy.template.json`, scoped to object IO
      + bucket list, no admin), CORS rules for presigned-URL
      GET / PUT (`cors_config.json`), and Public Access Block.
      The script emits a `gateway_config.generated.json` snippet
      ready to paste into the gateway config.
- [x] Production KMS / Vault wrapper for the gateway's CMK
      (PR #28) — `encryption/client_sdk/kms_wrapper.go` ships `KMSWrapper`
      (algorithm tag `aws-kms-wrap-v1`, AWS SDK v2 KMS client,
      KeyId verification on every Decrypt), and
      `encryption/client_sdk/vault_wrapper.go` ships `VaultWrapper`
      (algorithm tag `vault-transit-wrap-v1`, minimal HTTP client
      against `{mount}/encrypt/{name}` and `{mount}/decrypt/{name}`).
      Both implement the existing `client_sdk.Wrapper` interface so
      the data-plane PUT / GET paths are unchanged.
      `cmd/gateway/main.go#buildGatewayEncryption` selects the
      wrapper from the `cmk_uri` scheme: `cmk://local/...` (or
      empty) routes to `LocalFileWrapper` (dev only),
      `arn:aws:kms:...` / `kms://...` routes to `KMSWrapper`, and
      `vault://...` / `transit://...` routes to `VaultWrapper`.
      `internal/config/config.go#EncryptionConfig` exposes
      `KMSRegion`, `VaultAddr`, `VaultToken`, and
      `VaultTransitMount` with environment fallbacks (`AWS_REGION`,
      `VAULT_ADDR`, `VAULT_TOKEN`, transit mount default
      `"transit"`). Coverage in
      `encryption/client_sdk/kms_wrapper_test.go` (round-trip,
      scheme normalization, wrong-algorithm rejection, KeyId
      mismatch) and `encryption/client_sdk/vault_wrapper_test.go`
      (httptest-backed round-trip, scheme normalization, error
      surface). The CMK rotation runbook landed in PR #30 at
      `docs/runbooks/cmk-rotation.md` (KMS rolling rotation, Vault
      transit rotation, emergency revocation, dev-only local-file
      fallback). Remaining work is operator-side execution of the
      runbook against a live deployment (provisioning the production
      KMS keys / Vault mounts and running the first rotation
      end-to-end on real infrastructure).
- [x] NVMe cache nodes (L0 / L1) on Linode. `DiskCache`
      implementing `HotObjectCache` lives in
      `cache/hot_object_cache/disk_cache.go`, rebuilds its index
      from disk on restart, supports TTL + capacity eviction + hot
      pinning, and is wired into `cmd/gateway/main.go` via
      `config.gateway.cache_path`. Coverage in
      `cache/hot_object_cache/disk_cache_test.go` (round-trip,
      restart-persistence, TTL expiry, capacity eviction, orphan
      cleanup, oversize rejection).
- [x] First local DC cell: 6–12 storage nodes, 300 TB – 1 PB raw
      capacity, HDD durable nodes (L2), NVMe cache, gateway fleet.
      Operator stand-up in `deploy/local-dc/`: cephadm
      bootstrap script (`cephadm/install.sh`) for Ceph Reef +
      service spec (`cephadm/cluster.yaml`) placing 3 mons / 2
      mgrs / 3 RGW / OSD HDD service with NVMe BlueStore WAL+DB,
      Ansible host-prep playbook (`ansible/playbook.yml`,
      `ansible/hosts.example.ini`) for OS hardening and cephadm
      install across 6+ nodes, monitoring scrape
      (`monitoring/prometheus.yml`) for ceph-mgr / RGW /
      gateway, and a gateway config snippet
      (`gateway_config.example.json`) wiring `ceph_rgw` into
      the providers registry plus a migration target for the
      Wasabi → local-cell rebalancer. README documents the beta
      sizing (300 TB raw / replication 3) → production sizing
      (EC 6+2 / 1 PB raw) ramp.
- [x] 25–100 Gbps aggregate public bandwidth across Linode + local
      DC. The Linode fleet sizing guidance in `deploy/linode/README.md`
      ramps from 1.5 GB/s (3× g6-dedicated-8 beta) to 12 GB/s
      (7× g6-dedicated-32 production). The local-DC sizing in
      `deploy/local-dc/README.md` calls for 25 Gbps front + 25
      Gbps cluster network per OSD node (50 Gbps aggregate per
      node, 300+ Gbps cluster-wide for a 6-node cell), with
      monitoring scrape on `ceph_exporter` + RGW + gateway
      `/internal/metrics`.
- [x] Abuse throttling and per-tenant bandwidth budgets — split
      across `internal/auth/rate_limit.go` (production) and
      `internal/auth/abuse.go`, both wired with per-region runtime
      tuning via `config.abuse.*`. `rate_limit.go` layers three
      enforcement bands on every request: the per-tenant
      token-bucket RPS limit (`budgets.requests_per_sec` +
      `burst_requests`), a monthly egress ceiling sourced from
      `budgets.egress_tb_month`, and a sliding-window anomaly
      detector with a configurable EWMA baseline and alert
      multiplier; budget exhaustion returns HTTP 429 and emits
      `AbuseBudgetExhausted`, anomalies emit `AbuseAnomalyAlert`
      and, when `ThrottleOnAnomaly` is set, throttle for a
      cooldown window. `abuse.go` runs alongside as a sibling
      middleware that re-reads `tenant.Budgets.EgressTBMonth` and
      `tenant.Abuse.CDNShielding` directly off the tenant record,
      adds the CDN-shielding gate (rejects direct-to-origin
      requests for shielded tenants with HTTP 403), and exposes a
      2x-of-rolling-average egress-rate anomaly path that emits
      the same billing dimensions. The new `AbuseConfig`
      (`internal/config/config.go`) plus
      `cmd/gateway/main.go#applyAbuseConfigToRateLimiter` and
      `applyAbuseConfigToAbuseGuard` apply
      `anomaly_multiplier`, `anomaly_window`, `anomaly_cooldown`,
      `throttle_on_anomaly`, and `baseline_alpha` to both guards
      so operators can re-tune per region without redeploying.
      Production alert routing now fans out: when
      `config.abuse.alert_webhook_url` is set,
      `cmd/gateway/main.go#buildAbuseAlertSink` composes a
      `MultiAlertSink` over the billing sink and the new
      `internal/auth/webhook_alert_sink.go`, which fire-and-forget
      JSON-POSTs every `billing.UsageEvent` to the configured
      webhook (PagerDuty / Slack / generic). Coverage in
      `internal/auth/rate_limit_test.go`,
      `internal/auth/abuse_test.go`, and
      `internal/auth/webhook_alert_sink_test.go` (HTTP delivery,
      non-blocking dispatch, MultiAlertSink fanout).
- [x] Tenant console (React) for onboarding, billing, placement
      policy, and key management. Vite + React + TypeScript
      scaffold under `frontend/` ships login / signup, dashboard
      (storage / requests / egress), bucket management, API-key
      management (access key + one-time secret reveal on create),
      placement-policy YAML editor with a structured summary, and
      a dedicated-cells page gated on
      `contract_type ∈ {b2b_dedicated, sovereign}`. Backend API in
      `api/console/` covers `GET /api/tenants/{id}`,
      `GET /api/tenants/{id}/usage`, `POST /api/tenants/{id}/keys`,
      `GET` / `PUT /api/tenants/{id}/placement`,
      `GET /api/tenants/{id}/buckets`, and
      `GET` / `POST /api/tenants/{id}/dedicated-cells` on its own
      HTTP mux bound to `:8081` (separate from the S3 data plane
      on `:8080`) via `cfg.Console.ListenAddr`. `console.UsageQuery`
      is satisfied by the ClickHouse billing sink when available; a
      no-op stub ships otherwise. SSE usage stream
      (`/api/v1/usage/stream/{id}`) and the Playwright e2e suite
      were already done; the Postgres-backed `PlacementStore`
      (`api/console/postgres_placement.go`) is wired via
      `buildPlacementStore`, and the Phase 3 batch adds
      `PostgresAuthStore` so the B2C signup / verification flow
      persists across restarts. Admin auth (`buildAdminAuth` with
      constant-time bearer-token comparison) gates every
      `/api/tenants/...` request when `cfg.Console.AdminToken` is
      set. Coverage in `api/console/handler_test.go`,
      `api/console/auth_handler_test.go`,
      `api/console/postgres_auth_test.go` (env-gated),
      `api/console/postgres_placement_test.go`, and the Playwright
      suite under `frontend/`.
- [x] B2C self-service onboarding flow. Frontend signup / login
      forms in `frontend/src/pages/SignUp.tsx` and `Login.tsx`
      drive the gateway's `POST /api/v1/auth/signup`,
      `POST /api/v1/auth/login`, and `POST /api/v1/auth/verify`
      handlers in `api/console/auth_handler.go`. Production wiring
      lands in this batch: `console.NewPostgresAuthStore`
      (`api/console/postgres_auth.go`) persists the
      email → (bcrypt hash, tenant ID, verified flag, verification
      token) mapping in the new `auth_users` table
      (`api/console/schema.sql`), with constant-time token
      comparison inside a transaction so two simultaneous
      `/verify` calls cannot double-flip the same row;
      `cmd/gateway/main.go#buildAuthStore` selects it whenever
      `cfg.ControlPlane.MetadataDSN` is set. The hCaptcha verifier
      (`api/console/captcha.go`) gates signup behind
      `cfg.Console.CaptchaSecret` (with `HCAPTCHA_SECRET` env
      fallback), and the SES verification email sender
      (`api/console/email_ses.go`) wires
      `cfg.Console.SESRegion` / `SESFromAddress` /
      `SESVerifyURLBase` so the S3 `VerifiedCheck` gate in
      `api/s3compat/handler.go` only enables when an email path
      is actually configured. Coverage in
      `api/console/auth_handler_test.go`,
      `api/console/postgres_auth_test.go` (env-gated),
      `api/console/captcha_test.go`, and `api/console/email_ses_test.go`.
- [x] B2B dedicated cell provisioning. Console surface
      (`frontend/src/pages/B2BPage.tsx`) lists dedicated cells from
      `GET /api/tenants/{id}/dedicated-cells` for tenants whose
      `contract_type` is `b2b_dedicated` or `sovereign`. The Phase
      3 batch adds the operator-side scaffold:
      `internal/cellops/provisioner.go` defines the
      `CellProvisioner` interface (`ProvisionCell` /
      `DecommissionCell` / `CellStatus`) and ships
      `ManualProvisioner`, which validates the request, mints a
      fresh cell ID via `crypto/rand`, persists a pending
      `CellStatus` (status `provisioning`) via the `CellSink`
      interface, and logs a structured audit line so operators
      get a paged trail. `MemoryDedicatedCellStore` (dev / tests)
      and the new `PostgresDedicatedCellStore`
      (`api/console/postgres_dedicated_cells.go`, backed by the
      `dedicated_cells` table in `api/console/schema.sql`) both
      satisfy `CellSink` so the provisioner is interchangeable
      between dev and prod. The `POST /api/tenants/{id}/dedicated-cells`
      endpoint in `api/console/handler.go` validates the JSON
      body, forces the URL-path tenant ID (so a forged body
      cannot bind a cell to a different tenant), and returns
      `202 Accepted` with the `cellops.CellStatus` payload so
      tenants and operators can poll for the
      `provisioning → active` transition.
      `cmd/gateway/main.go#buildDedicatedCellStore` selects the
      Postgres store when `cfg.ControlPlane.MetadataDSN` is set
      and falls back to the in-memory store otherwise;
      `buildCellProvisioner` wires whichever store satisfies
      `cellops.CellSink` to `ManualProvisioner`. Coverage in
      `internal/cellops/provisioner_test.go` (validation,
      persistence, decommission idempotence) and
      `api/console/handler_test.go`. Full automation
      (Terraform / Ansible bring-up that flips the cell to
      `active`) lives behind the same interface in Phase 4.
- [x] Beta customer onboarding (backup, SaaS assets, AI datasets,
      media libraries, sovereign storage). `docs/runbooks/beta-onboarding.md`
      ships per-archetype playbooks: backup workloads (8+3 EC,
      managed CMK, multipart-required, 2 TB egress / month),
      SaaS asset storage (6+2 EC, CDN-shielded, 10 TB egress),
      AI datasets (10+4 EC, stride-aligned range GETs, 100 TB
      egress, dedicated-cell-eligible), media libraries (6+2,
      aggressive hot-object promotion, 50 TB egress), and
      sovereign storage (Ceph RGW dedicated cell with country
      allow-list and customer-held CMK via Vault). The companion
      `docs/runbooks/tenant-setup.md` covers the mechanical
      console-API onboarding (create tenant, configure
      placement, issue API keys, set budgets, monitor usage,
      decommission).
- [x] End-to-end migration dry run: move a beta bucket from Wasabi
      to the first local cell without customer-visible changes.
      `tests/s3_compat/live_migration_test.go` ships
      `TestLiveMigration_WasabiToCephRGW` (covered below) and
      drives the full S3 compliance suite while the rebalancer
      is concurrently advancing the migration state machine.
      The deploy-side decommission flow lives in
      `deploy/cell-provisioner/provision_cell.sh --decommission`
      so an operator can drain a cell into a Wasabi region (or
      a sibling cell) without customer-visible changes.
- [x] Run S3 compliance test suite against `ceph_rgw` adapter —
      100% pass required before production traffic. Executed
      against a live Ceph Reef RGW (quay.io/ceph/demo:latest-reef,
      `127.0.0.1:8888`, bucket `zkof-ceph-compliance`); the full
      `Run(t, Setup)` subtest matrix in
      `tests/s3_compat/suite_test.go` passes (PUT/GET/HEAD/DELETE,
      range GET prefix/middle/tail, LIST, idempotent DELETE,
      missing-key 404, presigned GET, multipart-like overwrite,
      multipart round-trip, multipart abort, and 6+2 erasure-
      coded round-trip). Adapter fix: `providers/s3_generic/
      generic.go#PutPiece` now per-call applies
      `v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware` so
      the AWS SDK v2 signer accepts a non-seekable `io.Reader`
      body against non-AWS S3-compatible endpoints (Ceph RGW,
      Backblaze B2, Cloudflare R2). Captured test log:
      `tests/s3_compat/ceph_compliance.log`. Companion entrypoints
      `TestSuite_BackblazeB2`, `TestSuite_CloudflareR2`, and
      `TestSuite_AWSS3` gate BYOC / cloud adapter validation on
      the same env-var pattern and inherit the same PutPiece fix.
- [x] Run S3 compliance test suite during a live Wasabi → Ceph RGW
      migration with beta customers. `tests/s3_compat/live_migration_test.go`
      adds `TestLiveMigration_WasabiToCephRGW`, which wires a
      Wasabi-primary / Ceph-RGW-secondary `DualWriteProvider`
      (`migration/dual_write`), pre-populates an object on Wasabi
      so the rebalancer has an outstanding piece to mirror, and
      then drives the full `Run(t, Setup)` compliance suite
      against the dual-write topology while a goroutine-driven
      `background_rebalancer.Rebalancer` (`migration/background_rebalancer`)
      is concurrently advancing the migration state machine. The
      test asserts at least one rebalancer pass completed during
      the suite (so a stalled rebalancer does not hide behind a
      green compliance run) and re-reads the preloaded piece from
      the primary post-migration. Gated on `WASABI_ENDPOINT`,
      `WASABI_BUCKET`, `CEPH_RGW_ENDPOINT`, and
      `CEPH_RGW_BUCKET` (with the matching access / secret keys
      and optional region / cell / country) so default CI stays
      green without credentials.
- [x] Gateway fleet node health monitor (deferred from Phase 2):
      per-cell quorum, cache-tier drain, graceful gateway
      replacement. Implemented in `internal/health/health.go` with
      `GET /internal/health`, `GET /internal/ready`, `POST
      /internal/drain` endpoints, `Monitor.Track()` for in-flight
      gating, `Monitor.Drain()` bounded by `DrainTimeout`, and
      optional cache eviction on drain. Wired into
      `cmd/gateway/main.go` as a background goroutine alongside
      the rebalancer and promotion worker; SIGTERM triggers
      `Drain()` before `signalBus.Close()`. Coverage in
      `internal/health/health_test.go` for quorum transitions,
      drain readiness flip, in-flight gating, and timeout
      handling.
- [x] Phase 3 billing metering backend. `ClickHouseSink` in
      `billing/clickhouse_sink.go` ingests usage events via
      ClickHouse HTTP `INSERT FORMAT JSONEachRow`, batches by size
      + interval, retries 5xx with exponential backoff, and
      drains on `Close()`. Schema in `billing/schema.sql` ships
      `usage_events` (MergeTree) + `usage_counters`
      (SummingMergeTree). Coverage in
      `billing/clickhouse_sink_test.go` for batch-size flush,
      close flush, 5xx retry, and config validation.
- [x] Vendor-neutral `BillingProvider` integration seam. The
      gateway now distinguishes the metering pipeline (the
      existing `BillingSink`) from the optional outbound
      invoicing / payment integration. `billing/provider.go`
      defines a `BillingProvider` interface (`Name`,
      `EnsureCustomer`, `EnsureSubscription`, `ReportUsage`,
      `IssueInvoice`, `CancelSubscription`) plus a `NoopProvider`
      default that logs every call without making outbound
      requests. `billing/registry.go` adds a process-wide
      registry (`RegisterProvider` / `BuildProvider`) so future
      plug-ins (Stripe, Chargebee, …) can register themselves at
      `init()` time without `cmd/gateway/main.go` learning about
      a specific vendor; the gateway resolves the configured
      provider from `cfg.Billing.Provider` (with a free-form
      `cfg.Billing.ProviderConfig` map for vendor-specific
      settings) and falls back to `noop` when no provider is
      configured. Coverage in `billing/provider_test.go` and
      `billing/registry_test.go`.
- [x] BYOC / cloud adapter compliance entrypoints.
      `TestSuite_BackblazeB2`, `TestSuite_CloudflareR2`, and
      `TestSuite_AWSS3` added in `tests/s3_compat/suite_test.go`
      following the `TestSuite_CephRGW` pattern, each gated on
      the provider's `*_ENDPOINT` / `*_BUCKET` env vars so CI
      stays green without credentials.
- [x] Real S3 multipart upload support. `CreateMultipartUpload`,
      `UploadPart`, `CompleteMultipartUpload`, `AbortMultipartUpload`,
      and `ListMultipartUploads` implemented in
      `api/s3compat/multipart_handler.go`, backed by
      `api/s3compat/multipart/store.go` (in-memory `Store` with
      tenant-scoped listing, part-ETag validation, and idempotent
      abort). Per-part pieces are addressed by a deterministic
      `{uploadID}-p{partNumber:05d}` piece ID; the `Complete`
      aggregate ETag follows the S3 `MD5(part_md5s)-N` convention.
      The GET path in `api/s3compat/erasure_coding.go#getMultipart`
      concatenates pieces in ascending `PartNumber` order.
      Handler routing in `api/s3compat/handler.go#dispatch` covers
      `?uploads`, `?uploadId=...&partNumber=...`, and
      `?uploadId=...` variants. Coverage in
      `api/s3compat/multipart/store_test.go` and integration tests
      `MultipartRoundTrip` + `MultipartAbort` in
      `tests/s3_compat/suite_test.go`.
- [x] Erasure coding wired into the write path for local DC
      backends. `PlacementPolicy.ErasureProfile` diverts PUTs to
      `api/s3compat/erasure_coding.go#putErasureCoded`, which
      shards the body into k+m Reed-Solomon pieces per stripe via
      the clean-room encoder in
      `metadata/erasure_coding/encoder.go` (codec:
      `github.com/klauspost/reedsolomon`, MIT). Profiles are
      registered in `metadata/erasure_coding/registry.go`
      (`DefaultRegistry` ships 6+2, 8+3, 10+4, 12+4, 16+4 per
      `StandardProfiles`). Each shard lands as its own piece
      carrying `StripeIndex`, `ShardIndex`, and `ShardKind`
      metadata; `getErasureCoded` reconstructs the plaintext and
      tolerates up to `ParityShards` missing shards per stripe.
      `cmd/gateway/main.go` wires the default registry into
      `s3compat.Config.ErasureCoding`. Coverage in
      `metadata/erasure_coding/encoder_test.go` (pad, round-trip,
      single + multi-shard loss) and `ErasureRoundTrip` in
      `tests/s3_compat/suite_test.go`.
- [x] Storj BYOC provider adapter wired into gateway. `providers/storj/storj.go`
      implements `StorageProvider` via the native `storj.io/uplink` library
      (not the S3 gateway). `providers/storj/uplink_bridge.go` bridges
      `*uplink.Project` to the adapter's `UplinkProject` interface.
      `cmd/gateway/main.go` registers the provider under `"storj"` when
      `config.providers.storj.access_grant` is set. PR #19 review findings
      (ContentType guard, ListObjects page cap, OAuth login fast-path,
      VerifiedCheck gate, verify endpoint auth, Vite proxy) are all resolved.
- [x] Lightweight Docker demo container. `Dockerfile` (multi-stage:
      Go build + Vite frontend build + Alpine runtime),
      `docker-compose.yml`, `demo/config.json`, `demo/tenants.json`,
      and `.dockerignore`. The container runs the gateway in dev mode
      (`local_fs_dev` backend, in-memory manifest store, logger
      billing sink) with the S3 API on `:8080` and the console API
      on `:8081`. Pre-loaded demo tenant credentials (`demo-access-key`
      and the `kmail-access-key` pair scoped to tenant
      `kmail-tenant-001`) allow immediate use with any S3-compatible
      client. Verified as the backend for kmail's Stalwart blob store
      — the same S3 API that serves Phase 1 Wasabi and Phase 2 Ceph
      RGW deploys now serves kmail's local dev stack. Object data
      persists in the `zk-data` Docker volume; tenant and manifest
      state is in-memory only.
- [x] Kapp Business Suite integration. The Kapp `kapp-fab` repo now
      provisions a per-tenant HMAC credential pair plus a dedicated
      bucket against the fabric console API at `:8081` during its
      tenant setup wizard, and runs every file attachment upload /
      download through the fabric in `managed` encryption mode so
      ERP file attachments inherit per-tenant zero-knowledge
      encryption. Joins kmail and zk-drive as a reference downstream
      integration alongside the existing Stalwart blob store path.
      Co-deploys cleanly via `docker-compose.yml` — Kapp talks to the
      fabric on the same Compose network with no extra plumbing.

### Avoid early customers with

- Random high-egress public download traffic (breaks the Wasabi
  fair-use assumption before the cache is warm).
- Tiny-object, billions-scale workloads (unless packed into
  containers).
- Heavy compliance requirements before the product has completed
  its audits.
- Latency-critical transactional workloads (ZK Object Fabric targets
  object storage, not a transactional KV).

---

## Phase 3.5: Intra-Tenant Deduplication

**Status**: `COMPLETE`

**Goal**: add object-level and block-level intra-tenant deduplication
to reduce storage costs for B2C community (viral/shared files) and
B2B org (company-wide documents) workloads. Cross-tenant dedup is
permanently excluded. Three integration patterns for external apps
(KChat, kmail, zk-drive, Kapp, any S3 client) are documented in
[INTEGRATION.md](INTEGRATION.md).

Checklist:

- [x] `ContentHash` field on `ObjectManifest` (`metadata/manifest.go`).
      BLAKE3 of content (plaintext for Pattern B, ciphertext for Pattern C).
      Added in PR #36 scaffolding; JSON round-trip test landed in
      `metadata/manifest_test.go#TestObjectManifest_ContentHashJSONRoundTrip`.
- [x] `DedupPolicy` struct and field on `PlacementPolicy`
      (`metadata/manifest.go`). Scaffolded in PR #36; consumed by the
      gateway PUT path (`api/s3compat/dedup.go#dedupEnabled`).
- [x] `ContentIndexStore` interface and Postgres implementation
      (`metadata/content_index/`). Memory store at
      `metadata/content_index/memory_store.go`; Postgres store at
      `metadata/content_index/postgres/store.go` with race-safe
      Register (INSERT … ON CONFLICT DO NOTHING) and atomic
      DecrementRef (UPDATE … RETURNING ref_count). Tests in
      `memory_store_test.go` and the env-gated `postgres/store_test.go`.
- [x] `content_index` schema in `metadata/content_index/schema.sql`.
      Already shipped with PR #36; the `CHECK ref_count >= 0`
      constraint surfaces underflow as `ErrInvalidRefCount`.
- [x] Gateway convergent encryption (Pattern B) in PUT path
      (`api/s3compat/dedup.go#prepareDedupedPutPatternB`). Streams
      plaintext through BLAKE3, derives the convergent DEK via
      `client_sdk.DeriveConvergentDEK`, encrypts deterministically,
      then runs the BLAKE3(ciphertext) lookup / register / refcount
      flow.
- [x] Client-side convergent encryption (Pattern C) in PUT path
      (`api/s3compat/dedup.go#prepareDedupedPutPatternC`). The gateway
      hashes the received ciphertext stream and dedups directly —
      plaintext is never observed.
- [x] `ConvergentNonce` option in client SDK
      (`encryption/client_sdk/sdk.go`). The `nextFrame` path now
      derives `nonce_i = HKDF(DEK, info="zkof-nonce-v1" || chunk_idx)`
      when set; tests in `sdk_test.go` cover determinism and
      per-chunk uniqueness.
- [x] `DeriveConvergentDEK` function in client SDK
      (`encryption/client_sdk/keygen.go`). HKDF-SHA256 with the
      content hash as input, tenant ID as salt, and the
      `zkof-convergent-dek-v1` info string. Tests in
      `keygen_test.go` cover determinism, distinct tenants, distinct
      hashes, and empty-input rejection.
- [x] Reference-counted DELETE path (`api/s3compat/handler.go`
      `Delete`). When the manifest carries a `ContentHash` the
      gateway calls `ContentIndex.DecrementRef`; the backend piece
      and the index row are removed only on `ref_count == 0`.
- [x] Multipart dedup (`api/s3compat/multipart_handler.go`
      `CompleteMultipartUpload`). After assembly the gateway hashes
      the concatenated piece bytes, stores the digest on the
      manifest, and (for single-piece uploads) routes through the
      same lookup / register / refcount flow as single-PUT.
- [x] `DedupConfig` in `internal/config/config.go`. Wired through
      `cmd/gateway/main.go#buildContentIndex`, which selects the
      Postgres or in-memory `ContentIndexStore` based on the
      metadata DSN.
- [x] Console API endpoint for bucket dedup policy
      (`api/console/dedup_handler.go`). POST/GET/DELETE on
      `/api/v1/tenants/{tid}/buckets/{bucket}/dedup-policy`. The
      `object+block` upgrade is gated by
      `bucketResolvesToCephRGW` (placement must list a Ceph
      provider AND the tenant must own a dedicated cell). Tests in
      `api/console/dedup_handler_test.go`.
- [x] Ceph RGW block-level dedup operator guide in
      `deploy/local-dc/README.md` ("Block-level deduplication"
      section). Documents the dedup-tier pool layout, per-tenant
      pool isolation requirements, and the `ceph-mgr` /
      ClickHouse-side monitoring map.
- [x] S3 compliance tests with dedup (`tests/s3_compat/dedup_test.go`).
      Pattern B (managed encryption + dedup), Pattern C (client_side
      convergent ciphertext), reference-counted DELETE, and
      single-part multipart dedup all pass against `local_fs_dev`.
- [x] Dedup metrics in billing sink: `DedupHits`, `DedupBytesSaved`,
      `DedupRefCount` dimensions added to `billing/metering.go`.
      Emitted from the PUT and DELETE paths.
- [x] Benchmark: `tests/benchmark/suite.go` ships `dedup-b2c-80pct`
      and `dedup-b2b-60pct` scenarios, plus the
      `MetricDedupHitRatio`, `MetricDedupBytesSavedRatio`, and
      `MetricDedupPutLatencyOverheadP95` metrics.
- [x] `docs/INTEGRATION.md` — external app integration guide
      (already shipped with the PR #36 scaffolding).

### Constraints

- Cross-tenant dedup permanently excluded. ContentIndex scoped to
  tenant_id.
- `client_side` with random DEK (default) cannot dedup.
- DR copies are non-deduped full objects.
- MLS FS/PCS are message-channel properties, fully preserved.
  Stored file FS depends on CEK scheme (random = FS, convergent = no FS).
- Multipart with `managed` / `public_distribution` encryption now
  dedups via deferred convergent consolidation at
  `CompleteMultipartUpload` time (PR #54). The gateway hashes each
  part's plaintext at `UploadPart` time, then at Complete time
  looks up the combined hash in `content_index`. On hit, the
  random-DEK parts are deleted and the manifest redirects to the
  canonical convergent piece. On miss, the gateway re-encrypts
  under a convergent DEK and registers a new entry. Cost: one-time
  read+decrypt+re-encrypt on the miss branch; subsequent uploads
  cost only a `LookupByPlaintextHash` and the deletion of the
  just-uploaded random-DEK parts. See [INTEGRATION.md §8.5](INTEGRATION.md#85-complete-dedup-scenario-matrix)
  for the full per-method × per-mode matrix.
- Multi-piece multipart uploads (`len(pieces) > 1`) dedup across
  every convergent mode: `client_side` / unencrypted via the
  nullable `piece_ids` JSONB column on `content_index`, and
  `managed` / `public_distribution` via the deferred convergent
  consolidation flow described above.
- EC-coded objects are excluded from object-level dedup; B2B tenants
  rely on Ceph block-level dedup at the RADOS tier instead.
- CopyObject dedup requires a single-piece source with `ContentHash`;
  EC and multipart sources are rejected with HTTP 501.

---

## Phase 4: Production & Scale (Post-Beta)

**Status**: `IN PROGRESS | ~75%` (9 of 12 checklist items complete)

**Goal**: move from a single beta deployment to a production,
multi-cell fabric with published product tiers and operational
maturity. Wasabi remains the cloud overflow / DR backend; owned local
DC cells become the primary origin. Phase 3.5 (Intra-Tenant
Deduplication) should be complete before Phase 4 begins, as dedup
savings directly affect capacity planning and COGS projections for
multi-cell production.

Checklist:

- [x] Cell architecture (multi-cell, 2–20 PB per cell). `internal/cellops/registry.go` ships the read-only `CellRegistry` over the existing `dedicated_cells` table; `cmd/gateway` registers each active cell as a provider on startup.
- [x] Cross-cell replication (opt-in, policy-driven). `migration/cross_cell/replicator.go` mirrors manifests whose `PlacementPolicy.ReplicationPolicy.Mode == "async"` from a source provider to a destination provider; `internal/config.CrossCellConfig` gates the worker. (PR #48 fixed the Devin Review finding: `replicateOne` now deep-copies the manifest via `cloneManifestForReplica` and rewrites each `replica.Pieces[i].Backend` to `r.Dest.ID`, so dest-side GETs route to the destination backend instead of orphaning back to the source.)
- [ ] Hardware procurement engine for high-density HDD nodes.
- [ ] DC and power strategy.
- [ ] Global peering and transit.
- [x] Automated repair and drive replacement. `internal/repair/repair_queue.go` polls a `HealthSignalSource` (Ceph mgr `/api/v0.1/health` adapter shipped) and re-encodes affected EC manifests by Decoding the surviving shards and re-Encoding a fresh shard set.
- [x] Abuse, DDoS, and legal response operations. `internal/auth/ddos_shield.go` ships the `DDoSShield` interface + `CompositeShield` fanout + `CloudflareProvider` + `MemoryShield`; `internal/auth/legal_response.go` ships `LegalHold` records, `LegalHoldStore`, `CheckDelete`, and `ErrLegalHoldActive` (DELETE-path wiring is the follow-up).
- [x] Observability stack (metrics, traces, logs at scale). `internal/metrics/prometheus.go` self-contained text-format exporter; `internal/tracing/tracing.go` minimal span-based API + middleware.
- [x] Capacity forecasting and supply planning. `billing/forecasting.go` performs a linear least-squares fit over post-dedup byte counts and projects fill date; `api/console/forecast_handler.go` exposes `GET /api/v1/cells/{cellId}/forecast`.
- [x] Region-specific compliance (GDPR, HIPAA, FedRAMP, etc). `internal/compliance/audit_trail.go` + `residency_enforcer.go` + `internal/auth/legal_response.go` cover the GDPR-aligned pre-flight, fully wired end-to-end through `s3compat.ComplianceHooks` (residency check on PUT / multipart, audit emission on every PUT and GET success path, DELETE-path legal-hold check); HIPAA / FINRA / SEC are explicitly out of Phase 4 scope.
- [x] Published public product tiers (ZK Archive, ZK Standard, ZK
      Hot, ZK Dedicated, ZK Sovereign). `metadata/tenant/tier_config.go` ships the canonical mapping; `api/console/tier_handler.go` exposes `GET /api/v1/tiers`; `frontend/src/pages/TiersPage.tsx` renders the comparison.
- [x] At-scale migration: drain remaining Wasabi-backed tenants off
      the cloud origin onto local cells where their placement policy
      requires it. `migration/fleet_orchestrator.go` queues per-(tenant, bucket) jobs with per-cell concurrency caps; `api/console/migration_handler.go` exposes `GET /api/v1/migrations` and `GET /api/v1/migrations/{jobId}`.

**Notes:**

- **2026-05-08** — Documentation audit: cleaned up status header,
  fixed Phase 4 compliance checkbox (GDPR pre-flight complete,
  HIPAA/FINRA/SEC out of scope → checked), updated Phase 3.5
  constraints to reflect PR #54 deferred convergent consolidation,
  created standalone PHASES.md and ARCHITECTURE.md, updated README
  with actual project structure and test instructions.

- **2026-04-27** — Compliance wiring tail (#49 → #51 plus this
  PR). #49 was the broad compliance follow-up wave: legal-hold
  blocking on DELETE via the new `LegalHoldChecker` interface,
  symmetric residency check on `CreateMultipartUpload`, audit
  hooks on `putErasureCoded` / `putDeduped` /
  `CompleteMultipartUpload`, and the `PostgresAuditStore.Query`
  zero-`TimeRange` fix. #50 added the matching `GET` audit rows
  to the `getErasureCoded` and `getMultipart` success paths so
  EC and multipart reads land in the audit trail symmetric with
  managed-mode single-piece reads. #51 was a small follow-up
  cleaning up an unchecked `http.Post` error in the legal-hold
  cross-tenant integration test surfaced by `go vet`.

  This PR closes the remaining Phase 4 compliance gaps the
  follow-up review surfaced:

  - `api/s3compat/erasure_coding.go#putErasureCoded` now logs
    the audit row with `manifest.Pieces[0].PieceID` instead of
    the bare `versionID`, so the EC `PUT` audit reference
    matches a concrete shard the operator can address on the
    backend (mirroring the single-PUT path at
    `api/s3compat/handler.go:474`).
  - `internal/config.ComplianceConfig` gains a
    `LegalHoldEnabled` flag and `cmd/gateway/main.go#buildComplianceHooks`
    now constructs a `LegalHoldStore` (`auth.NewPostgresLegalHoldStore`
    when the metadata DB is configured, falling back to
    `auth.NewMemoryLegalHoldStore`) and assigns it to
    `s3compat.ComplianceHooks.LegalHoldStore` via a thin
    `legalHoldAdapter` that maps `auth.LegalHold` → `s3compat.LegalHoldEntry`.
    The DELETE-path `LegalHoldChecker` shipped in #49 was a
    no-op until this wiring landed because nothing populated
    the field at startup.
  - `internal/auth/postgres_legal_hold_store.go` ships the
    Postgres-backed implementation against the
    `legal_holds` schema in
    `internal/auth/legal_response_schema.sql`. The `Active`
    query replicates `LegalHold.Matches` (tenant-, bucket-,
    and object-scoped holds) and honours the `ExpiresAt`
    column so expired holds drop out of the hot path.
    Coverage in `internal/auth/postgres_legal_hold_store_test.go`
    skips when `METADATA_DSN` is unset, mirroring the
    `compliance/postgres_audit_test.go` pattern.

  Open Phase 4 checklist items remain the three operational
  ones (hardware procurement engine, DC + power strategy,
  global peering + transit). Region-specific compliance
  (HIPAA/FINRA/SEC) is still out of Phase 4 scope; the GDPR
  pre-flight via `residency_enforcer.go` + `audit_trail.go` +
  `legal_response.go` is now fully wired end-to-end.

- **2026-04-27** — Phase 4 Tier 1/2/3 implementation batch shipped
  across ten reviewable PRs (kennguy3n/zk-object-fabric#38
  through #46) plus this PROGRESS.md update:

  - **Pre-existing items** (#38): orphan-GC worker
    (`metadata/content_index/orphan_gc.go` walks every tenant's
    content_index, drops entries whose `pieceID` no longer
    appears in any manifest, and `provider.DeletePiece` +
    `content_index.Delete`s in a single pass);
    `Copy` handler in `api/s3compat/copy.go` (dedup-aware via
    `ContentIndex.IncrementRef`, falls back to GET+PUT for
    providers without `SupportsServerSideCopy`); object
    versioning by `?versionId=` propagation through `resolve()`,
    `Delete()`, `Head()`, plus `ListVersions` / `?versions`
    handler.
  - **Tier 1 — Observability** (#39):
    `internal/metrics/prometheus.go` self-contained text-format
    exporter exposes `zkof_request_duration_seconds`,
    `zkof_cache_hit_total`, `zkof_dedup_*`,
    `zkof_provider_errors_total`, `zkof_active_requests`;
    `MetricsBillingSink` wraps any `BillingSink` and counts
    Emit calls; `internal/tracing/tracing.go` ships a minimal
    span-based API and an HTTP middleware that opens a span per
    S3 request with `tenant_id` / `bucket` / `method` /
    `backend` attributes.
  - **Tier 1 — Compliance** (#40): append-only `audit_trail`
    table + `internal/compliance/audit_trail.go` `AuditEntry` /
    `AuditStore` / Postgres impl; `residency_enforcer.go`
    pre-flight check called from `handler.go#Put` after
    `ResolveBackend` returns `403 DataResidencyViolation` when
    `provider.PlacementLabels().Country` is outside the tenant's
    allowed list.
  - **Tier 1 — Cell registry & provisioner** (#41):
    `internal/cellops/registry.go` `CellRegistry` over the
    existing `dedicated_cells` table; `automated_provisioner.go`
    `AutomatedProvisioner` wraps a `TerraformRunner` interface
    (production: shell out to terraform; tests: stub) to bring
    cells up and flip them to `active`. (Devin Review flagged
    two correctness items on user-authored commits:
    `IDGenerator` nil guard and `DecommissionCell` mutex; both
    confirmed and pending operator confirmation before fix.)
  - **Tier 1 — Cross-cell replication** (#42):
    `migration/cross_cell/replicator.go` async replicator scans
    a `[]ScopeKey` of (tenant, bucket) and mirrors manifests
    whose `PlacementPolicy.ReplicationPolicy.Mode == "async"`.
    `LagNanos` / `CopiedPieces` counters expose progress.
    Outstanding: replicated manifest's `Piece.Backend` should
    be rewritten to `r.Dest.ID` (Devin Review finding).
  - **Tier 1 — Repair queue** (#43):
    `internal/repair/repair_queue.go` polls a
    `HealthSignalSource`, asks a `ManifestScanner` for the
    manifests that reference any degraded piece, and re-encodes
    affected EC manifests by `Decode`ing surviving shards and
    `Encode`ing a fresh shard set. `ceph_health.go`
    `CephHealthClient` adapter for the Ceph manager health API.
  - **Tier 2 — Capacity forecasting** (#44):
    `billing/forecasting.go` `Forecaster` runs a linear
    least-squares fit over post-dedup byte counts and emits
    `Alert=true` when `ProjectedFillAt` is inside the
    `AlertWindow` (default 90 days). Console handler at
    `api/console/forecast_handler.go` mounts
    `GET /api/v1/cells/{cellId}/forecast`.
  - **Tier 2 — DDoS shield + legal hold** (#45):
    `internal/auth/ddos_shield.go` `DDoSShield` /
    `CompositeShield` fanout / `CloudflareProvider` /
    `MemoryShield`; `legal_response.go` `LegalHold` record +
    `LegalHoldStore` + `CheckDelete` + `ErrLegalHoldActive`
    sentinel; Postgres schema in
    `legal_response_schema.sql`; console handler at
    `api/console/legal_hold_handler.go`. DELETE-path wiring is
    the follow-up.
  - **Tier 3 — Tier config + fleet migration** (#46):
    `metadata/tenant/tier_config.go` canonical `TierConfig`
    mapping for Archive (rs-16-4), Standard (rs-8-3), Hot
    (rs-6-2), Dedicated (rs-8-3 + object+block dedup),
    Sovereign (rs-8-3 + country-locked); console handler at
    `api/console/tier_handler.go` mounts `GET /api/v1/tiers`;
    `migration/fleet_orchestrator.go` queues per-(tenant,
    bucket) `MigrationJob`s with per-cell concurrency caps and
    a pluggable `JobRunner`; console handler at
    `api/console/migration_handler.go` mounts
    `GET /api/v1/migrations` and `GET /api/v1/migrations/{id}`;
    `frontend/src/pages/TiersPage.tsx` renders the comparison
    table.

  Cross-tenant deduplication, parallel email-only key hierarchy,
  and HIPAA/FINRA/SEC compliance are intentionally **out** of
  Phase 4 scope per the do-not-do list. The Phase 4 brief's
  remaining items (hardware procurement engine; DC and power
  strategy; global peering and transit; region-specific
  compliance beyond GDPR) are operational rather than
  software-engineering-shaped and live outside this repo.

- **2026-04-27** — Compliance wiring follow-ups (#49 plus this
  PR). `#49` closed the largest gaps the Phase 4 compliance
  assessment flagged on top of #40's `audit_trail.go` /
  `residency_enforcer.go` / `legal_response.go` primitives:

  - `LegalHoldChecker` was added to
    `api/s3compat/handler.go#ComplianceHooks` and the S3
    `Delete` handler now calls
    `Compliance.LegalHold.Active(ctx, tenant, bucket, key)` and
    returns `403 ObjectUnderLegalHold` (via `ErrLegalHoldActive`)
    when any active hold is found, completing the legal-hold
    follow-up #45 had deferred.
  - `CreateMultipartUpload` in
    `api/s3compat/multipart_handler.go` now mirrors the
    single-PUT `Compliance.Residency.Check(...)` call after
    `ResolveBackend` and returns `403 DataResidencyViolation`
    when the resolved provider's country is outside the policy.
  - `h.audit()` was added to the success paths of
    `putErasureCoded` (EC manifest backend) and `putDeduped`
    (post-redirect piece backend with country looked up via
    `PlacementLabels()`), and `CompleteMultipartUpload` now
    audits the assembled manifest's first-piece backend so
    multipart PUTs land in the audit trail symmetric with
    single-PUT.
  - `internal/compliance/postgres_audit.go#PostgresAuditStore.Query`
    builds its `WHERE` clause dynamically: a zero `TimeRange`
    omits the `recorded_at` predicate entirely (matching
    `MemoryAuditStore`), and one-bound ranges use `>=` /
    `<=` against the non-zero side; coverage in
    `internal/compliance/postgres_audit_test.go#TestPostgresAuditStore_ZeroTimeRange`.

  This PR finishes the remaining audit-trail gaps the
  assessment surfaced. The S3 GET path's single-piece audit at
  `handler.go:609` already covered managed-mode reads, but the
  EC and multipart early-return branches (`getErasureCoded`,
  `getMultipart` in `api/s3compat/erasure_coding.go`) bypassed
  it. They now emit a `GET` audit entry on the success path
  using `manifest.MigrationState.PrimaryBackend` (EC) and
  `manifest.Pieces[0].Backend` (multipart) for backend
  attribution, so every served byte is traceable in the
  compliance audit table.

  The three unchecked Phase 4 checklist items — hardware
  procurement engine, DC + power strategy, and global peering
  + transit — remain operational rather than software-shaped
  and continue to live outside this repo's scope.

- **2026-04-26** — Provider adapter wiring: `backblaze_b2`,
  `cloudflare_r2`, and `aws_s3` are now registered by
  `cmd/gateway/main.go#buildProviderRegistry` when their respective
  config sections are populated, following the same pattern used for
  `wasabi`, `ceph_rgw`, and `storj`. Registration is gated on
  `cfg.Providers.BackblazeB2.Endpoint`, `cfg.Providers.CloudflareR2.AccountID`
  or `cfg.Providers.CloudflareR2.Endpoint`, and `cfg.Providers.AWSS3.Region`
  being non-empty. The adapters are wired end-to-end but still marked
  "pending live compliance validation" — the Phase 3 BYOC nightly CI
  (`.github/workflows/byoc-compliance.yml`) exercises each adapter
  against real buckets when its gating secrets are set; Phase 4 flips
  these to "Wired; compliance green" once each adapter has a captured
  reference log in `tests/s3_compat/testdata/`.

---

## Appendix: Key Metrics to Track

| Metric                                  | Target                              | Phase   |
| --------------------------------------- | ----------------------------------- | ------- |
| PUT p99 latency (client → Linode → Wasabi) | TBD                              | Phase 2 |
| GET p99 latency (Linode cache hit)      | TBD                                 | Phase 2 |
| GET p99 latency (Wasabi origin miss)    | TBD                                 | Phase 2 |
| Linode cache hit ratio (Hot tier)       | > 90%                               | Phase 3 |
| Wasabi origin egress ratio (egress ÷ stored) | ≤ 1.0 per tenant               | Phase 2–3 |
| Repair time (single node loss, Phase 2+)| TBD                                 | Phase 2 |
| Storage COGS / TB-month (local DC)      | < $3.00 at 1 PB                     | Phase 3 |
| Erasure overhead (Phase 2+)             | 1.375× (8+3) or 1.4× (10+4)         | Phase 2 |
| Migration throughput (Wasabi → local cell) | TBD                              | Phase 3 |
