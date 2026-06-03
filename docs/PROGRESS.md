# ZK Object Fabric — Development Progress

- **Project**: ZK Object Fabric
- **License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).
- **Status**: Phase 3 — Beta Cell (IMPLEMENTATION COMPLETE — production validation pending). Phase 3.5 — Intra-Tenant Deduplication (IMPLEMENTATION COMPLETE — production validation pending). Phase 4 — Production & Scale (~75% implementation scaffold complete; not yet production-validated).
- **Last updated**: 2026-05-03

This document is a phase-gated tracker. Each phase has an explicit
checklist and entry criteria. Each phase builds on the previous one and
should not begin until the previous phase is complete.

For the technical design, see [PROPOSAL.md](PROPOSAL.md). For an
overview of phases and current status, see [PHASES.md](PHASES.md).

---

## Phase 1: Architecture Proof

**Status**: `COMPLETE`

**Goal**: lock the architecture on the AWS control plane + Linode
data plane + Wasabi storage backend stack, ratify the
provider-neutral manifest and migration plan, and produce enough
formal specification that Phase 2 engineers can implement without
re-debating core decisions.

Checklist:

- [x] Ratify the Phase 1 stack: AWS (control plane) + Linode (data
      plane) + Wasabi (storage backend).
- [x] Confirm no customer data flows through AWS, enforced by a
      control-plane contract test in `tests/control_plane/`.
- [x] Select the Phase 2+ local-DC base. **Ceph RGW** chosen for
      production maturity, operational tooling, and built-in erasure
      coding. SeaweedFS retained as a documented fallback.
- [x] Define the provider-neutral object manifest format
      (`metadata/manifest.go`).
- [x] Define the encryption envelope (per-object DEK, encrypted
      manifest, CMK support) in `encryption/envelope.go`.
- [x] Define the placement-policy DSL (YAML schema) in
      `metadata/placement_policy/`.
- [x] Define erasure-coding profiles for Phase 2+ (6+2, 8+3, 10+4)
      in `metadata/erasure_coding/`.
- [x] Define the S3 compatibility subset (PUT, GET, HEAD, DELETE,
      LIST, multipart, range GET). Operation matrix specified in
      [PROPOSAL.md §3.2.2](PROPOSAL.md).
- [x] Establish the S3 API as the phase-invariant contract
      ([PROPOSAL.md §3.2](PROPOSAL.md)).
- [x] Define the benchmark suite (PUT / GET latency percentiles,
      cache hit ratio, repair time, Wasabi origin egress ratio,
      network cost) in `tests/benchmark/`.
- [x] Define the multi-tenancy model (tenant isolation, billing,
      abuse controls) in `metadata/tenant/`.
- [x] Define the migration engine spec (dual-write, lazy migration on
      read, background rebalancer, migration state machine) in
      `migration/`.
- [x] Specify the Linode cache design (NVMe / block storage sizing,
      promotion rules, range-aligned chunking) in
      `cache/hot_object_cache/`.
- [x] Specify Wasabi fair-use guardrails (egress budgets, per-tenant
      cache hit ratio targets, 90-day minimum storage handling) in
      `providers/wasabi/guardrails.go`.

### Phase 1 base selection

AGPL options are ruled out because ZK Object Fabric ships under a
proprietary license. Garage is ruled out because it does not support
erasure coding and therefore cannot meet Phase 2+'s EC durable-origin
requirement.

**Design decision: Ceph RGW** is the Phase 2+ local-DC base. Ceph's
production maturity, operational tooling, and erasure-coding support
outweigh the slower custom-feature roadmap relative to SeaweedFS.
SeaweedFS is retained as a documented fallback: if Phase 2 operational
load or feature velocity pushes us off Ceph, SeaweedFS becomes the
second-choice base without reopening the AGPL / EC criteria.

| Requirement                              | Pick        | Notes                         |
| ---------------------------------------- | ----------- | ----------------------------- |
| Maximum production maturity (selected)   | Ceph RGW    | Phase 2+ local-DC base        |
| Faster custom product build (fallback)   | SeaweedFS   | Retained as documented backup |

---

## Phase 2: Prototype

**Status**: `COMPLETE`

**Goal**: a single-cell prototype that can PUT, GET, HEAD, DELETE,
LIST, and range-read encrypted objects end-to-end, backed by Wasabi
via the Linode gateway, with the migration engine wired up for a
dry-run cut-over to a local DC cell.

Checklist:

- [x] S3-compatible gateway in Go (`cmd/gateway/`, `api/s3compat/`)
      covering PUT, GET, HEAD, DELETE, LIST, range GET, and presigned
      URLs with the hot cache consulted on the GET path.
- [x] Client-side encryption SDK (`encryption/client_sdk/`) with
      chunked XChaCha20-Poly1305 encrypt/decrypt, DEK generation, and
      CMK-agnostic wrap/unwrap.
- [x] Gateway-side encryption wiring across the single-piece PUT,
      erasure-coded PUT, and multipart `UploadPart` paths, with a
      symmetric decrypt on every read path. `managed` and
      `public_distribution` policies generate a fresh DEK per object,
      seal it with the configured CMK, and record the wrapped DEK on
      the manifest. `client_side` (Strict ZK) refuses PUTs lacking
      the `X-Amz-Meta-Zk-Encryption` header and streams ciphertext
      through untouched.
- [x] Encrypted manifest storage in the AWS control plane
      (`metadata/manifest_store/postgres/`), wired into the gateway
      with an in-memory store as the dev/test fallback.
- [x] Storage provider adapter framework (`wasabi`, `local_fs_dev`,
      stubs for `backblaze_b2`, `cloudflare_r2`, `aws_s3`,
      `ceph_rgw`).
- [x] Placement engine (`metadata/placement_policy/engine.go`) that
      filters eligible providers by policy constraints and picks the
      cheapest using `StorageProvider.CostModel()`.
- [x] Wasabi durable origin wired up as the primary backend and
      registered as the placement-engine default when no
      tenant-specific policy overrides it.
- [x] Linode hot cache (L0 / L1) with promotion rules
      (`cache/hot_object_cache/`).
- [x] Basic billing counters (per-tenant storage-seconds, PUTs,
      GETs, egress bytes) emitted from the handler via
      `billing/logger_sink.go`.
- [x] Range GET support with range-aligned cache chunks.
- [x] Hot-object promotion from Wasabi to Linode cache, driven by a
      non-blocking `PromotionSignal` bus and a policy-aware
      promotion worker.
- [x] Multi-tenant isolation: SigV4 authenticator, in-memory tenant
      store, per-tenant token-bucket rate limiter
      (`internal/auth/`). The authenticator supports both header- and
      query-string-presigned variants with chunked-payload signature
      verification.
- [x] Migration engine: dual-write, lazy migration on read, and a
      background rebalancer (`migration/dual_write/`,
      `migration/lazy_read_repair/`,
      `migration/background_rebalancer/`). Lazy read-repair is wired
      into the gateway GET path; the background rebalancer runs as
      an optional worker that shares the promotion worker's shutdown
      context.
- [x] S3 compliance test suite (`tests/s3_compat/`) exercising PUT,
      GET, HEAD, DELETE, LIST, range GET, DELETE idempotency,
      missing-key 404s, presigned GETs, and multipart-like overwrite
      semantics. The reusable `Run(t, Setup)` harness lets any
      provider be plugged in.
- [x] Simulated Wasabi → `local_fs_dev` migration test that asserts
      zero behavioral differences during cutover.
- [x] Benchmark execution covering PUT/GET p50/p95/p99, cache hit
      ratio, Wasabi origin egress ratio, small-object overhead, and
      LIST performance at 10M / 100M / 1B objects.

---

## Phase 3: Beta Cell

**Status**: `IMPLEMENTATION COMPLETE` — production validation pending
(load testing, chaos testing, security review, and DR exercises have
not yet been performed against this codebase). See the **Production
Readiness** section below for the full not-yet-validated list.

**Goal**: stand up a real beta deployment on the AWS + Linode +
Wasabi stack with paying / design-partner customers on both B2C and
B2B paths, plus a first local DC cell for early hybrid customers.

Checklist:

- [x] Production AWS control plane deploy scaffolding (`deploy/aws/`):
      Terraform modules for RDS PostgreSQL 16 (multi-AZ, encrypted,
      14-day backups), gateway / console IAM roles, KMS CMK with
      annual rotation, CloudWatch log groups, alarms, and
      dashboards. ClickHouse billing sink
      (`billing/clickhouse_sink.go`, schema in `billing/schema.sql`)
      is wired through `config.billing.clickhouse_url`. The
      `applyDBConnectionPool` helper centralizes pool tuning across
      every metadata-store call site.
- [x] Production Linode gateway fleet deploy scaffolding
      (`deploy/linode/`): Terraform module provisioning dedicated
      instances per region with attached NVMe block volumes and a
      regional NodeBalancer whose health check polls
      `/internal/ready`. Cloud-init bootstraps the gateway via a
      systemd unit (`ProtectSystem=strict`, `KillSignal=SIGTERM`).
      Caddy terminates TLS and blocks `/internal/*` from external
      clients.
- [x] Production Wasabi origin (`deploy/wasabi/`): multi-region
      bucket provisioner that creates `zkof-{region}-{env}` buckets
      with a scoped IAM policy template, CORS for presigned URLs,
      and Public Access Block. Each region registers as its own
      `StorageProvider` so placement policies can target
      `wasabi-us-east-1`, `wasabi-eu-central-1`, and so on.
- [x] Production KMS / Vault wrapper for the gateway's CMK.
      `KMSWrapper` (algorithm tag `aws-kms-wrap-v1`, AWS SDK v2 KMS
      client) and `VaultWrapper` (algorithm tag
      `vault-transit-wrap-v1`) both implement the existing
      `client_sdk.Wrapper` interface so the data-plane PUT / GET
      paths are unchanged. `buildGatewayEncryption` selects the
      wrapper from the `cmk_uri` scheme. Operational guidance lives
      in [docs/runbooks/cmk-rotation.md](runbooks/cmk-rotation.md).
- [x] NVMe cache nodes (L0 / L1) on Linode via the `DiskCache`
      implementation, which rebuilds its index from disk on restart
      and supports TTL + capacity eviction + hot-pinning.
- [x] First local DC cell deploy scaffolding (`deploy/local-dc/`):
      cephadm bootstrap for Ceph Reef, service spec placing
      3 mons / 2 mgrs / 3 RGW / OSD HDD with NVMe BlueStore WAL+DB,
      Ansible host-prep playbook, Prometheus scrape, and a gateway
      config snippet wiring `ceph_rgw` into the provider registry.
- [x] Sizing guidance for 25–100 Gbps aggregate public bandwidth
      across Linode + local DC. Linode fleet sizing ramps from
      ~1.5 GB/s (beta) to ~12 GB/s (production); local DC nodes call
      for 25 Gbps front + 25 Gbps cluster network per OSD node.
- [x] Abuse throttling and per-tenant bandwidth budgets across
      `internal/auth/rate_limit.go` and `internal/auth/abuse.go`.
      Each request is subject to a per-tenant token-bucket RPS
      limit, a monthly egress ceiling, and a sliding-window anomaly
      detector. CDN shielding rejects direct-to-origin requests for
      shielded tenants with HTTP 403. When
      `config.abuse.alert_webhook_url` is set, a fire-and-forget
      JSON POST routes anomaly events to PagerDuty / Slack / a
      generic webhook.
- [x] Tenant console (React + Vite under `frontend/`) with login /
      signup, dashboard (storage / requests / egress), bucket
      management, API-key management, placement-policy YAML editor,
      and a dedicated-cells page gated on
      `contract_type ∈ {b2b_dedicated, sovereign}`. The console API
      (`api/console/`) is bound to `:8081` (separate from the S3
      data plane on `:8080`) and protected by a constant-time
      bearer-token comparison when `cfg.Console.AdminToken` is set.
- [x] B2C self-service onboarding (`api/console/auth_handler.go`).
      The Postgres-backed `AuthStore` persists email → (bcrypt hash,
      tenant ID, verified flag, verification token), with
      transactional verification semantics so two simultaneous
      `/verify` calls cannot double-flip the same row. The hCaptcha
      verifier gates signup behind `cfg.Console.CaptchaSecret`; SES
      sends the verification email via `cfg.Console.SESRegion` /
      `SESFromAddress` / `SESVerifyURLBase`.
- [x] B2B dedicated cell provisioning. The
      `internal/cellops.CellProvisioner` interface ships a
      `ManualProvisioner` that mints a fresh cell ID, persists a
      pending `CellStatus` via the `CellSink` interface, and logs a
      structured audit line. The Postgres-backed
      `DedicatedCellStore` and an in-memory store both satisfy
      `CellSink`. `POST /api/tenants/{id}/dedicated-cells` returns
      `202 Accepted` with a `cellops.CellStatus` payload so tenants
      and operators can poll for the `provisioning → active`
      transition.
- [x] Beta customer onboarding playbooks for backup workloads, SaaS
      asset storage, AI datasets, media libraries, and sovereign
      storage. See
      [docs/runbooks/beta-onboarding.md](runbooks/beta-onboarding.md)
      and [docs/runbooks/tenant-setup.md](runbooks/tenant-setup.md).
- [x] End-to-end migration dry run: move a beta bucket from Wasabi
      to the first local cell without customer-visible changes.
      `tests/s3_compat/live_migration_test.go` drives the full S3
      compliance suite while the rebalancer concurrently advances
      the migration state machine. Operator-side decommission ships
      in `deploy/cell-provisioner/provision_cell.sh --decommission`.
- [x] S3 compliance test suite passing against the `ceph_rgw`
      adapter (gated on `CEPH_RGW_*` env vars). Companion entry
      points `TestSuite_BackblazeB2`, `TestSuite_CloudflareR2`, and
      `TestSuite_AWSS3` gate BYOC / cloud adapter validation on the
      same pattern.
- [x] S3 compliance test suite passing during a live Wasabi → Ceph
      RGW migration. The test wires a Wasabi-primary /
      Ceph-RGW-secondary dual-write topology, pre-populates an
      object on Wasabi, and drives the full compliance suite while a
      background rebalancer concurrently advances the migration
      state machine.
- [x] Gateway fleet node health monitor (`internal/health/`) with
      `GET /internal/health`, `GET /internal/ready`,
      `POST /internal/drain`, in-flight gating, drain bounded by
      `DrainTimeout`, and optional cache eviction on drain. SIGTERM
      triggers `Drain()` before the signal bus closes.
- [x] Phase 3 billing metering backend. `ClickHouseSink` ingests
      usage events via `INSERT FORMAT JSONEachRow`, batches by size
      and interval, retries 5xx with exponential backoff, and
      drains on `Close()`. Schema ships `usage_events` (MergeTree)
      and `usage_counters` (SummingMergeTree).
- [x] Vendor-neutral `BillingProvider` integration seam. The
      gateway distinguishes the metering pipeline from the optional
      outbound invoicing / payment integration. `billing/provider.go`
      defines a `BillingProvider` interface (`Name`,
      `EnsureCustomer`, `EnsureSubscription`, `ReportUsage`,
      `IssueInvoice`, `CancelSubscription`); a process-wide registry
      in `billing/registry.go` lets future plug-ins register at
      `init()` time. A `NoopProvider` is the default.
- [x] BYOC / cloud adapter compliance entry points
      (`TestSuite_BackblazeB2`, `TestSuite_CloudflareR2`,
      `TestSuite_AWSS3`) following the same env-var gating pattern
      as the Ceph suite.
- [x] Real S3 multipart upload support
      (`api/s3compat/multipart_handler.go`):
      `CreateMultipartUpload`, `UploadPart`,
      `CompleteMultipartUpload`, `AbortMultipartUpload`, and
      `ListMultipartUploads`. The in-memory store provides
      tenant-scoped listing, part-ETag validation, and idempotent
      abort. The aggregate ETag follows the S3 `MD5(part_md5s)-N`
      convention.
- [x] Erasure coding wired into the write path for local-DC
      backends. `PlacementPolicy.ErasureProfile` diverts PUTs to
      `putErasureCoded`, which shards the body into k+m
      Reed-Solomon pieces per stripe. Profiles (6+2, 8+3, 10+4,
      12+4, 16+4) are registered in
      `metadata/erasure_coding/registry.go`. `getErasureCoded`
      reconstructs plaintext and tolerates up to `ParityShards`
      missing shards per stripe.
- [x] Storj BYOC provider adapter (`providers/storj/`) wired via the
      native `storj.io/uplink` library; registered when
      `config.providers.storj.access_grant` is set.
- [x] Lightweight Docker demo container. The container runs the
      gateway with `local_fs_dev`, in-memory manifests, and the
      logger billing sink, with the S3 API on `:8080` and the
      console API on `:8081`. Pre-loaded demo credentials let any
      S3-compatible client connect immediately. Object data persists
      in the `zk-data` Docker volume.
- [x] Reference downstream integrations. A `managed`-mode tenant
      template wires the fabric into mail, file sync, ERP, and
      messaging applications via the console API at `:8081`. Each
      integration provisions a per-tenant HMAC pair and bucket and
      streams uploads / downloads through the fabric in `managed`
      mode so application code never sees plaintext keys.

### Known limitations for early deployments

- Random high-egress public download traffic (breaks the Wasabi
  fair-use assumption before the cache is warm).
- Tiny-object, billions-scale workloads (unless packed into
  containers).
- Heavy compliance requirements before the product has completed its
  audits.
- Latency-critical transactional workloads (ZK Object Fabric targets
  object storage, not a transactional KV).

---

## Phase 3.5: Intra-Tenant Deduplication

**Status**: `IMPLEMENTATION COMPLETE` — production validation pending
(see **Production Readiness** below).

**Goal**: add object-level and block-level intra-tenant deduplication
to reduce storage costs for B2C community (viral / shared files) and
B2B org (company-wide documents) workloads. Cross-tenant dedup is
permanently excluded. Three integration patterns for external apps
are documented in [INTEGRATION.md](INTEGRATION.md).

Checklist:

- [x] `ContentHash` field on `ObjectManifest` (`metadata/manifest.go`),
      BLAKE3 of content (plaintext for Pattern B, ciphertext for
      Pattern C).
- [x] `DedupPolicy` struct and field on `PlacementPolicy`, consumed
      by the gateway PUT path (`api/s3compat/dedup.go`).
- [x] `ContentIndexStore` interface and Postgres implementation
      (`metadata/content_index/`) with race-safe `Register`
      (`INSERT … ON CONFLICT DO NOTHING`) and atomic `DecrementRef`
      (`UPDATE … RETURNING ref_count`).
- [x] `content_index` schema with `CHECK ref_count >= 0`, surfacing
      underflow as `ErrInvalidRefCount`.
- [x] Gateway convergent encryption (Pattern B) in the PUT path.
      The gateway streams plaintext through BLAKE3, derives the
      convergent DEK via `client_sdk.DeriveConvergentDEK`, encrypts
      deterministically, then runs the BLAKE3(ciphertext) lookup /
      register / refcount flow.
- [x] Client-side convergent encryption (Pattern C) in the PUT path.
      The gateway hashes the received ciphertext stream and dedups
      directly; plaintext is never observed.
- [x] `ConvergentNonce` option in the client SDK. When set,
      `nextFrame` derives
      `nonce_i = HKDF(DEK, info="zkof-nonce-v1" || chunk_idx)`.
- [x] `DeriveConvergentDEK` function in the client SDK: HKDF-SHA256
      with the content hash as input, tenant ID as salt, and the
      `zkof-convergent-dek-v1` info string.
- [x] Reference-counted DELETE path. When the manifest carries a
      `ContentHash`, the gateway calls
      `ContentIndex.DecrementRef`; the backend piece and the index
      row are removed only on `ref_count == 0`.
- [x] Multipart dedup at `CompleteMultipartUpload`. After assembly
      the gateway hashes the concatenated piece bytes, stores the
      digest on the manifest, and (for single-piece uploads) routes
      through the same lookup / register / refcount flow as
      single-PUT.
- [x] `DedupConfig` in `internal/config/`, wired through
      `buildContentIndex`, which selects the Postgres or in-memory
      `ContentIndexStore` based on the metadata DSN.
- [x] Console API endpoint for bucket dedup policy
      (`api/console/dedup_handler.go`). The `object+block` upgrade
      is gated by `bucketResolvesToCephRGW` (placement must list a
      Ceph provider AND the tenant must own a dedicated cell).
- [x] Ceph RGW block-level dedup operator guide in
      [deploy/local-dc/README.md](../deploy/local-dc/README.md).
- [x] S3 compliance tests with dedup (`tests/s3_compat/dedup_test.go`)
      covering Pattern B, Pattern C, reference-counted DELETE, and
      single-part multipart dedup.
- [x] Dedup metrics in the billing sink: `DedupHits`,
      `DedupBytesSaved`, `DedupRefCount` dimensions emitted from the
      PUT and DELETE paths.
- [x] Benchmark scenarios `dedup-b2c-80pct` and `dedup-b2b-60pct`
      plus `MetricDedupHitRatio`, `MetricDedupBytesSavedRatio`, and
      `MetricDedupPutLatencyOverheadP95`.
- [x] External-app integration guide
      ([docs/INTEGRATION.md](INTEGRATION.md)).

### Constraints

- Cross-tenant dedup is permanently excluded. ContentIndex is scoped
  to `tenant_id`.
- `client_side` with a random DEK (default) cannot dedup.
- DR copies are non-deduped full objects.
- MLS forward secrecy and post-compromise security are
  message-channel properties and are fully preserved. Stored-file
  forward secrecy depends on the CEK scheme (random = FS,
  convergent = no FS).
- Multipart with `managed` / `public_distribution` encryption
  dedups via deferred convergent consolidation at
  `CompleteMultipartUpload`. The gateway hashes each part's
  plaintext at `UploadPart`, then at Complete time looks up the
  combined hash in `content_index`. On hit, the random-DEK parts
  are deleted and the manifest redirects to the canonical
  convergent piece. On miss, the gateway re-encrypts under a
  convergent DEK and registers a new entry. See
  [INTEGRATION.md §8.5](INTEGRATION.md#85-complete-dedup-scenario-matrix)
  for the per-method × per-mode matrix.
- Multi-piece multipart uploads (`len(pieces) > 1`) dedup across
  every convergent mode: `client_side` / unencrypted via the
  nullable `piece_ids` JSONB column on `content_index`, and
  `managed` / `public_distribution` via the deferred convergent
  consolidation flow described above.
- EC-coded objects are excluded from object-level dedup; B2B tenants
  rely on Ceph block-level dedup at the RADOS tier instead.
- `CopyObject` dedup requires a single-piece source with
  `ContentHash`; EC and multipart sources are rejected with
  HTTP 501.

---

## Phase 4: Production & Scale

**Status**: `IN PROGRESS | ~75% implementation scaffold complete`
(9 of 12 checklist items implemented; none yet production-validated —
see **Production Readiness** below)

**Goal**: move from a single beta deployment to a production,
multi-cell fabric with published product tiers and operational
maturity. Wasabi remains the cloud overflow / DR backend; owned local
DC cells become the primary origin. Phase 3.5 (Intra-Tenant
Deduplication) should be complete before Phase 4 begins, as dedup
savings directly affect capacity planning and COGS projections for
multi-cell production.

Checklist:

- [x] Cell architecture (multi-cell, 2–20 PB per cell).
      `internal/cellops/registry.go` ships the read-only
      `CellRegistry` over the `dedicated_cells` table; the gateway
      registers each active cell as a provider on startup.
- [x] Cross-cell replication (opt-in, policy-driven).
      `migration/cross_cell/replicator.go` mirrors manifests whose
      `PlacementPolicy.ReplicationPolicy.Mode == "async"` from a
      source provider to a destination provider; the replicated
      manifest's piece `Backend` is rewritten so dest-side GETs
      route to the destination backend.
- [ ] Hardware procurement engine for high-density HDD nodes.
- [ ] DC and power strategy.
- [ ] Global peering and transit.
- [x] Automated repair and drive replacement.
      `internal/repair/repair_queue.go` polls a `HealthSignalSource`
      (Ceph mgr `/api/v0.1/health` adapter shipped) and re-encodes
      affected EC manifests by decoding surviving shards and
      re-encoding a fresh shard set.
- [x] Abuse, DDoS, and legal response operations.
      `internal/auth/ddos_shield.go` ships the `DDoSShield`
      interface, a `CompositeShield` fanout, a `CloudflareProvider`,
      and a `MemoryShield`. `internal/auth/legal_response.go` ships
      `LegalHold` records, `LegalHoldStore`, `CheckDelete`, and
      `ErrLegalHoldActive`. The DELETE path enforces legal hold via
      the `LegalHoldChecker` hook.
- [x] Observability stack (metrics, traces, logs at scale).
      `internal/metrics/prometheus.go` is a self-contained
      text-format exporter; `internal/tracing/tracing.go` is a
      minimal span-based API with HTTP middleware that adds
      `tenant_id` / `bucket` / `method` / `backend` attributes per
      request.
- [x] Capacity forecasting and supply planning.
      `billing/forecasting.go` performs a linear least-squares fit
      over post-dedup byte counts and projects a fill date;
      `api/console/forecast_handler.go` exposes
      `GET /api/v1/cells/{cellId}/forecast`.
- [x] Region-specific compliance (GDPR pre-flight).
      `internal/compliance/audit_trail.go`,
      `internal/compliance/residency_enforcer.go`, and
      `internal/auth/legal_response.go` are wired end-to-end
      through `s3compat.ComplianceHooks`: residency check on PUT
      and multipart, audit emission on every PUT and GET success
      path, and DELETE-path legal-hold check. HIPAA, FINRA, and SEC
      compliance are explicitly out of Phase 4 scope.
- [x] Published public product tiers (ZK Archive, ZK Standard,
      ZK Hot, ZK Dedicated, ZK Sovereign).
      `metadata/tenant/tier_config.go` ships the canonical mapping;
      `api/console/tier_handler.go` exposes `GET /api/v1/tiers`;
      `frontend/src/pages/TiersPage.tsx` renders the comparison.
- [x] At-scale migration: drain remaining Wasabi-backed tenants off
      the cloud origin onto local cells where their placement policy
      requires it. `migration/fleet_orchestrator.go` queues
      per-(tenant, bucket) jobs with per-cell concurrency caps;
      `api/console/migration_handler.go` exposes
      `GET /api/v1/migrations` and `GET /api/v1/migrations/{jobId}`.

### Recent changes

- GDPR-aligned compliance pipeline fully wired end-to-end: residency
  pre-flight on PUT and multipart, audit emission on every PUT/GET
  success path (including EC and multipart branches), and DELETE-path
  legal-hold check.
- Cross-cell replication now rewrites each replica's piece
  `Backend` to the destination provider so dest-side GETs route
  correctly instead of orphaning back to the source.
- Provider adapter registration (`backblaze_b2`, `cloudflare_r2`,
  `aws_s3`) is gated on per-provider config sections; the nightly
  BYOC compliance workflow exercises each adapter against real
  buckets when its gating secrets are present.
- Deferred convergent consolidation for `managed` /
  `public_distribution` multipart uploads at
  `CompleteMultipartUpload` time; see
  [INTEGRATION.md §8.5](INTEGRATION.md#85-complete-dedup-scenario-matrix).

---

## Production Readiness

The phase status labels above reflect **code-implementation completeness**,
not production validation. The following items are explicitly **not yet
validated** against this codebase and should be completed before any
external load is run on it:

- **Load testing** — harness wired ([`tests/benchmark`](../tests/benchmark/),
  CLI at [`cmd/benchmark-runner`](../cmd/benchmark-runner/), runbook at
  [`docs/runbooks/load-testing.md`](runbooks/load-testing.md)). Numeric
  targets published below. **Real-deployment runs** against the
  Linode + Wasabi staging gateway are still pending and must be
  attached as JSON reports under `docs/reports/load/` before this
  item closes.
- **Chaos / failure-injection testing** — single-node loss, zone loss,
  metadata-DB failover, provider-side outages, cache partition.
- **External security review** — independent review of the auth path
  (HMAC, presigned URLs, IAM-style policies), the encryption envelope,
  manifest sealing, and DEK handling.
  - **Hand-off package READY** —
    [`docs/security/audit-package-security.md`](security/audit-package-security.md)
    is a complete review package cross-referenced to the source files
    (`internal/auth/`, `api/s3compat/`, `metadata/manifest_store/postgres/`,
    `cmd/gateway/main.go`). Bundle for the auditor via
    `make audit-bundle`. Vendor engagement (e.g. Trail of Bits / Cure53 /
    NCC Group) still TBD; findings will land in
    [`docs/security/findings/<vendor>-<YYYY-MM-DD>/`](security/findings/).
- **External cryptography review** — review of the AEAD bindings
  (per-chunk AAD, manifest body AAD), the KEK / CMK hierarchy, and the
  HMAC SigV4 implementation.
  - **Hand-off package READY** —
    [`docs/security/audit-package-cryptography.md`](security/audit-package-cryptography.md)
    is a complete crypto review package cross-referenced to
    `encryption/client_sdk/`, `encryption/envelope.go`, and
    `metadata/manifest_store/postgres/body_encryptor.go`. Same bundle
    target as above; specialist crypto auditor engagement still TBD.
- **S3 conformance report** — a published conformance matrix against
  the AWS S3 API surface (PUT, GET, HEAD, DELETE, LIST, multipart,
  range, ACL, versioning, lifecycle) using an external conformance
  harness, not only the in-repo `tests/s3_compat` suite.
- **Disaster-recovery exercises** — restore-from-backup runbooks
  exercised end-to-end, cross-cell replication failover, manifest-DB
  restore-and-resume, and customer-visible RPO / RTO measurement.
  *Runbooks plus an automated in-process verifier published in
  WS1.6 (see [`docs/runbooks/dr.md`](runbooks/dr.md) and
  [`tests/dr/verifier.go`](../tests/dr/verifier.go) — Postgres /
  cross-cell / manifest-resume drills still require operator-led
  external exercises against real infrastructure).*
- **Multi-tenant abuse / quota validation** — abuse-control trip
  thresholds tested under adversarial workloads (slowloris, key-space
  flood, egress-budget exhaustion).
- **Audit hand-off package** — single tarball an external auditor
  receives, bundling the security + cryptography audit packages
  (WS1.3 / WS1.4), the capacity envelope dossier (WS2.3), the S3
  conformance matrix (WS1.5 / WS2.2), the disaster-recovery
  runbooks + verifier (WS1.6), and the Linode + Wasabi staging
  deploy + Tier 3 verifier (WS2.1). Hand-off pipeline lives at
  [`cmd/audit-handoff`](../cmd/audit-handoff/) (the bundler) and
  [`deploy/audit-handoff/`](../deploy/audit-handoff/) (the
  manifest + auditor README); drift tests at
  [`tests/audit/handoff_test.go`](../tests/audit/) pin the
  invariant that every manifest component is mentioned in the
  auditor README and resolves to a real on-disk path. The bundle
  is anchored to a public commit SHA recorded in `MANIFEST.txt`
  with a SHA-256 chain the auditor verifies with `sha256sum -c`.
  **Hand-off mechanism READY**; each individual workstream's
  source PR (#77 / #78 / #79 / #81 / #82 / #83) flips its
  component from `optional: true` to fully required as it merges.

This list will shrink as items are exercised against the codebase. When
an item is completed, move it from this section into the relevant
phase's checklist with a link to the report or runbook.

---

## Roadmap Workstreams (WS8–WS9)

Specified in [PROPOSAL.md §15](PROPOSAL.md). Coverage view in
[S3_COMPATIBILITY.md](S3_COMPATIBILITY.md). Each slice is sized for an
independent PR; check the box when the handler is wired and covered by
`tests/s3_compat/` (and, for WS8, the key is removed from
`unsupportedSubresources` in `api/s3compat/handler.go`).

**WS8 — Richer S3 API Support**

- [ ] WS8.1 Object tagging (`?tagging`) — `tagging_handler.go`, JSONB
      tags on the manifest row.
- [ ] WS8.2 Object lifecycle (`?lifecycle`) — `metadata/lifecycle/`,
      `bucket_lifecycle` table, daily evaluator, console editor.
- [ ] WS8.3 Object Lock / WORM (`?object-lock`, `?retention`,
      `?legal-hold`) — `metadata/object_lock/`; depends on WS8.4.
- [ ] WS8.4 Bucket versioning (`?versioning`) — config endpoints +
      delete markers.
- [ ] WS8.5 CORS (`?cors`) — per-bucket config + gateway middleware.
- [ ] WS8.6 Event notifications (`?notification`) —
      `internal/notifications/` webhook dispatcher + DLQ.
- [ ] WS8.7 Server-side encryption config (`?encryption`) — SSE header
      → ZKOF encryption modes.
- [ ] WS8.8 Docs — PROPOSAL §3.2.2 + §15.1, ARCHITECTURE.md packages,
      S3_COMPATIBILITY.md matrix. *(this slice)*

**WS9 — Rust client-side encryption SDK**

- [ ] WS9 Rust SDK in `encryption/rust_sdk/`, byte-compatible with the
      Go SDK; cross-language parity test corpus in CI.

---

## Appendix: Key Metrics to Track

| Metric                                       | Target                          | Phase     |
| -------------------------------------------- | ------------------------------- | --------- |
| PUT p99 latency (cache hit)                  | ≤ 50 ms                         | Phase 2   |
| PUT p99 latency (Wasabi origin)              | ≤ 200 ms                        | Phase 2   |
| GET p99 latency (L0 / memory cache)          | ≤ 20 ms                         | Phase 2   |
| GET p99 latency (L1 / NVMe disk cache)       | ≤ 100 ms                        | Phase 2   |
| GET p99 latency (Wasabi origin miss)         | ≤ 300 ms                        | Phase 2   |
| Sustained throughput / gateway node          | ≥ 10 000 req/s                  | Phase 2   |
| Per-request error rate (sustained load)      | ≤ 1e-3                          | Phase 2   |
| Offered-load efficiency (attained / target)  | ≥ 0.95                          | Phase 2   |
| Linode cache hit ratio (Hot tier)            | > 90%                           | Phase 3   |
| Wasabi origin egress ratio (egress ÷ stored) | ≤ 1.0 per tenant                | Phase 2–3 |
| Repair time (single node loss, Phase 2+)     | TBD                             | Phase 2   |
| Storage COGS / TB-month (local DC)           | TBD                             | Phase 3   |
| Erasure overhead (Phase 2+)                  | 1.375× (8+3) or 1.4× (10+4)     | Phase 2   |
| Migration throughput (Wasabi → local cell)   | TBD                             | Phase 3   |

The latency, throughput, and error-rate targets above are the
machine-enforced gates in [`tests/benchmark/suite.go`](../tests/benchmark/suite.go)
(`TargetPutP99CacheHitMs`, `TargetPutP99OriginMs`, `TargetGetP99L0Ms`,
`TargetGetP99L1Ms`, `TargetGetP99OriginMs`, `TargetSustainedRPS`,
`TargetErrorRateMax`, `TargetRPSEfficiencyMin`). They are gated by
[`cmd/benchmark-runner`](../cmd/benchmark-runner/) and by the
`load-test-smoke` job in [`.github/workflows/ci.yml`](../.github/workflows/ci.yml).

For the full numeric envelope — including the S3 protocol limits
(max object size, multipart parts/sizes), the per-cell sizing range
(2–20 PiB usable), the availability target derived from the error-rate
ceiling, and the explicit enumeration of open operational targets (the
three TBDs above plus the audit-bundle cross-reference map) — see
[`docs/CAPACITY.md`](CAPACITY.md). That document is sourced from a
single Go module ([`tests/capacity`](../tests/capacity/)) so any
constant change breaks a pinned test until the doc, the gate, and the
audit-bundle cross-reference all move together.

See [`docs/runbooks/load-testing.md`](runbooks/load-testing.md) for the
end-to-end procedure (local CI smoke → Ceph RGW demo → Linode + Wasabi
staging) and the schema of the JSON reports the harness emits.
