# ZK Object Fabric — As-Built Architecture

This document describes the system **as currently implemented** —
the directory layout, the components in each layer, the deployment
modes, and the wire-level contracts. It complements but does not
replace:

- [PROPOSAL.md](PROPOSAL.md) — full technical design (encryption
  envelope, manifest format, placement DSL, dedup design, migration
  engine, cell architecture).
- [STORAGE_INFRA.md](STORAGE_INFRA.md) — deployment-model →
  storage-backend mapping and provider adapter matrix.
- [INTEGRATION.md](INTEGRATION.md) — dedup integration patterns for
  external applications.
- [S3_COMPATIBILITY.md](S3_COMPATIBILITY.md) — the supported S3 API
  surface.

## High-level architecture

```mermaid
flowchart TD
    Client["Client / SDK / S3-compatible Gateway"]
    Auth["ZK Auth & Policy<br/>(AWS control plane)"]
    Enc["Client-side or Gateway-side Encryption<br/>(Linode data plane)"]
    Manifest["Encrypted Object Manifest"]
    Adapter["Storage Provider Adapter"]
    Wasabi["Wasabi<br/>(cloud primary)"]
    B2["Backblaze B2<br/>(alternative)"]
    R2["Cloudflare R2<br/>(hot egress alternative)"]
    Local["Local DC Cell"]
    Cache["Hot Cache Layer<br/>(Linode NVMe / Akamai CDN)"]
    Repair["Repair & Audit System"]
    Bw["Bandwidth Accounting"]

    Client --> Auth
    Client --> Enc
    Enc --> Manifest
    Manifest --> Adapter
    Adapter --> Wasabi
    Adapter --> B2
    Adapter --> R2
    Adapter --> Local
    Adapter --> Cache
    Cache --> Bw
    Wasabi --> Repair
    Local --> Repair
    Wasabi --> Bw
    Local --> Bw
```

Every layer below the ZK Gateway operates on ciphertext. Keys never
leave the client boundary unless the customer explicitly opts into a
managed key mode.

## Tech stack

- **Server-side**: Go for the gateway, control plane services,
  storage provider adapters, migration engine, billing, and
  metadata services.
- **Frontend**: React + Vite + TypeScript for the tenant console,
  admin dashboards, self-service onboarding, and operator UIs
  (`frontend/`).
- **Rust** is reserved for selective hot paths where memory
  footprint and per-byte CPU cost dominate (chunking, encryption,
  cache eviction loops, erasure coding); Go currently covers the
  full data plane.
- **Datastores**: PostgreSQL for metadata (manifest, tenant, auth,
  placement, dedicated-cell, audit, legal hold, content index),
  ClickHouse for billing telemetry, Redis-style in-memory + on-disk
  LRU for hot object caching.
- **Crypto primitives**: XChaCha20-Poly1305 for object framing,
  BLAKE3 for content addressing, HKDF-SHA256 for convergent DEK
  derivation, AES-KW (via AWS KMS or Vault Transit) for CMK envelope
  wrap.

## Repository layout (as-built)

```
zk-object-fabric/
  Dockerfile
  docker-compose.yml
  go.mod / go.sum
  .dockerignore
  cmd/
    gateway/              # Gateway entry point (main.go)
  api/
    s3compat/             # S3 API handlers (object, multipart, copy,
                          # tagging, versioning, lifecycle, object
                          # lock, CORS, notification, bucket SSE),
                          # dedup, erasure coding, encryption
    console/              # Console API: tenant mgmt, auth, billing,
                          # placement, dedup policy
  encryption/
    client_sdk/           # Client-side encryption SDK
                          # (XChaCha20-Poly1305, convergent DEK)
    envelope.go           # Encryption envelope types
  metadata/
    manifest_store/       # Manifest persistence (memory + Postgres)
    bucket_config/        # Per-bucket S3 config (versioning state,
                          # Object Lock config, CORS rules, lifecycle
                          # rules, notification config, default SSE;
                          # memory + Postgres + SQLite)
    lifecycle/            # LifecycleRule domain + validation
    object_lock/          # LockConfig + retention/legal-hold
    cors/                 # CORS rule domain + origin matching
    notification/         # Bucket notification rule domain
    sse/                  # Bucket default SSE config domain
    placement_policy/     # Placement engine and policy DSL
    erasure_coding/       # EC profiles, encoder, registry
    content_index/        # Dedup ContentIndex (memory + Postgres)
    pieceintegrity/       # Shared per-piece content-hash verification
    tenant/               # Tenant schema, tier config
  lifecycle/
    evaluator/            # Daily lifecycle sweep (expire /
                          # delete-marker / abort), audit + billing
                          # hooks; wired into cmd/gateway/main.go
  providers/
    s3_generic/           # Shared S3-compatible base adapter
    wasabi/               # Wasabi adapter + fair-use guardrails
    ceph_rgw/             # Ceph RGW adapter (local-DC origin)
    aws_s3/               # AWS S3 adapter (BYOC / DR)
    backblaze_b2/         # Backblaze B2 adapter
    cloudflare_r2/        # Cloudflare R2 adapter
    storj/                # Storj native uplink adapter
    local_fs_dev/         # Filesystem loopback (dev/demo)
  cache/
    hot_object_cache/     # LRU memory cache, disk cache,
                          # promotion worker
  migration/
    dual_write/           # Dual-write provider wrapper
    lazy_read_repair/     # Read-path migration
    background_rebalancer/# Background object drain
    cross_cell/           # Cross-cell async replicator
  billing/                # Metering, ClickHouse sink, logger sink,
                          # forecasting, provider registry
  internal/
    auth/                 # SigV4 authenticator, rate limiter,
                          # abuse guard, DDoS shield, legal hold
    cellops/              # Cell registry, provisioner
    compliance/           # Audit trail, residency enforcer
    config/               # Gateway configuration
    health/               # Health/ready/drain endpoints
    metrics/              # Prometheus text-format exporter
    repair/               # Automated repair queue
    tracing/              # Request tracing
  frontend/               # React + Vite tenant console
  deploy/
    aws/                  # Terraform: RDS, IAM, KMS, CloudWatch
    linode/               # Terraform: gateway fleet, NodeBalancer
    wasabi/               # Bucket provisioner, IAM policy
    local-dc/             # Ceph cephadm, Ansible, monitoring
    cell-provisioner/     # Automated cell provisioning scripts
  tests/
    s3_compat/            # S3 compliance suite, dedup, encryption,
                          # migration tests
    benchmark/            # Performance benchmark runner
    control_plane/        # Control-plane contract tests
    providers/            # Provider-specific tests
  demo/
    config.json           # Dev gateway config (local_fs_dev,
                          # in-memory stores)
    tenants.json.tmpl     # Demo tenant template
    entrypoint.sh         # Docker entrypoint
    README.md             # Demo usage guide
  docs/
    PROPOSAL.md           # Technical design (full architecture spec)
    ARCHITECTURE.md       # As-built architecture overview (this file)
    INTEGRATION.md        # Dedup integration guide for external apps
    STORAGE_INFRA.md      # Deployment-model to storage mapping
    S3_COMPATIBILITY.md   # ZKOF-vs-AWS-S3 compatibility matrix
    runbooks/             # Operational runbooks
```

### Per-bucket S3 configuration storage

The richer S3 sub-resources are all wired into the gateway and
persisted through the `metadata/bucket_config` store, keyed by
`(tenant_id, bucket)` with memory + Postgres + SQLite backends (the
embedded single-node profile self-creates equivalent tables):

- **Object tagging** — `api/s3compat/tagging_handler.go`; tags stored
  as JSONB on the existing manifest row rather than in a new table.
- **Lifecycle** — `lifecycle_handler.go` + `metadata/lifecycle` +
  the `lifecycle/evaluator` daily sweep; rules in `bucket_lifecycle`
  (rule sets JSON-encoded).
- **Object Lock / WORM** — `object_lock_handler.go` +
  `metadata/object_lock`; default config in `bucket_object_lock`,
  with per-object-version retention mode / retain-until / legal-hold
  riding on the object manifest so they version with the object.
- **Bucket versioning** — `versioning_handler.go`; state in
  `bucket_versioning`.
- **CORS** — `cors_handler.go` + `metadata/cors`; rules in
  `bucket_cors`.
- **Event notifications** — `notification_handler.go` +
  `metadata/notification` + the `internal/notifications` async
  dispatcher; rules in `bucket_notification` (JSON-encoded).
- **Bucket default encryption (SSE)** — `encryption_handler.go` +
  `metadata/sse`; `Put/Get/DeleteBucketEncryption` set a bucket
  default of `AES256` or `aws:kms`, layered onto object writes as
  gateway-managed encryption.

## Component overview

### S3 API layer — `api/s3compat/`

- HTTP handlers for `PUT`, `GET`, `HEAD`, `DELETE`, `LIST`,
  `CopyObject`, `ListObjectVersions`, multipart lifecycle
  (`Create` / `Upload` / `Complete` / `Abort` / `ListParts` /
  `ListMultipartUploads`), range GET, presigned-URL generation and
  validation, S3-compatible XML errors.
- SigV4 verification and per-tenant rate limiting via
  `internal/auth/`.
- Conditional reads on GET/HEAD (`If-Match` / `If-None-Match` with
  strong ETag comparison, `If-Modified-Since` / `If-Unmodified-Since`,
  and `If-Range`), `response-*` header overrides, multi-range reads
  served as `206 multipart/byteranges`, and `x-amz-copy-source-if-*`
  conditionals on `CopyObject`. Server-side copy reconstructs
  erasure-coded and multipart sources.
- Sub-resource handlers: object tagging (`tagging_handler.go`),
  bucket lifecycle config (`lifecycle_handler.go`), Object Lock /
  retention / legal-hold (`object_lock_handler.go`), bucket versioning
  (`versioning_handler.go`), CORS (`cors_handler.go` plus the
  request-time `applyCORS` middleware and unauthenticated OPTIONS
  preflight), event notifications (`notification_handler.go`), and
  bucket default encryption (`encryption_handler.go`). The daily
  `lifecycle/evaluator` sweep is wired into `cmd/gateway/main.go`
  against the same `bucket_config` store.
- Encryption pipeline: client-side ciphertext passthrough,
  managed-mode envelope encryption (gateway derives DEK and wraps
  with the configured CMK), erasure-coding shard fan-out for the
  EC PUT path.
- Dedup pipeline: Pattern B (gateway convergent) and Pattern C
  (client-side convergent) under `dedup.go`; deferred convergent
  consolidation for `managed` / `public_distribution` multipart
  uploads at `CompleteMultipartUpload` time.
- Compliance hooks: residency pre-flight, audit trail emission,
  legal-hold check on DELETE. Object Lock / WORM enforces
  per-version retention (GOVERNANCE/COMPLIANCE) and legal holds in
  the permanent-delete and PUT-overwrite paths.

### Console API — `api/console/`

- Bound to `:8081` (separate from the S3 data plane on `:8080`).
- Endpoints:
  - `GET /api/tenants/{id}` — tenant record.
  - `GET /api/tenants/{id}/usage` — storage / requests / egress.
  - `POST /api/tenants/{id}/keys` — issue HMAC pair (one-time
    secret reveal).
  - `GET` / `PUT /api/tenants/{id}/placement` — placement policy
    YAML.
  - `GET /api/tenants/{id}/buckets` — bucket list.
  - `GET` / `POST /api/tenants/{id}/dedicated-cells` — gated on
    `contract_type ∈ {b2b_dedicated, sovereign}`.
  - `GET` / `POST` / `DELETE
    /api/v1/tenants/{tid}/buckets/{bucket}/dedup-policy`.
  - `GET /api/v1/cells/{cellId}/forecast` — capacity forecasting.
  - `GET /api/v1/tiers` — published tier catalog.
  - `GET /api/v1/migrations` and `/api/v1/migrations/{jobId}` —
    fleet migration status.
- Admin auth: constant-time bearer-token comparison gates every
  `/api/tenants/...` route when `cfg.Console.AdminToken` is set.

### Encryption — `encryption/`

- `client_sdk/` — XChaCha20-Poly1305 chunked AEAD framing,
  per-frame nonces, per-object envelope DEKs.
- `kms_wrapper.go`, `vault_wrapper.go`, `local_file_wrapper.go` —
  CMK wrap implementations selected by `cmk_uri` scheme.
- `keygen.go#DeriveConvergentDEK` — HKDF-SHA256 with tenant ID as
  salt; `nextFrame` derives `nonce_i = HKDF(DEK, ...)` when
  `ConvergentNonce` is set.
- `envelope.go` — `EncryptionConfig` types stored on each manifest.

### Metadata — `metadata/`

- `manifest_store/` — `ManifestStore` interface with in-memory and
  Postgres implementations.
- `placement_policy/` — `PlacementPolicy` DSL parser, evaluator,
  resolver (`ResolveBackend`).
- `erasure_coding/` — EC profile registry (6+2, 8+3, 10+4) and the
  `Encode` / `Decode` paths called by the EC PUT/GET paths.
- `content_index/` — Dedup `ContentIndexStore` (memory + Postgres),
  refcounted, supports nullable `piece_ids` JSONB column for
  multi-piece multipart entries and dual-format `plaintext_hash` for
  the deferred convergent consolidation path.
- `tenant/` — `Tenant`, `Budgets`, `Abuse`, `Billing`,
  `TierConfig`.

### Providers — `providers/`

All adapters implement the same `StorageProvider` interface so the
fabric can add, remove, and migrate between backends without
customer-visible changes:

- `s3_generic/` — shared S3-compatible base.
- `wasabi/` — primary cloud origin + fair-use guardrails
  (egress budgets, min-storage tracker, cache-hit-ratio target).
- `ceph_rgw/` — local-DC origin.
- `aws_s3/` — BYOC / DR.
- `backblaze_b2/`, `cloudflare_r2/`, `storj/` — alternative
  backends, gated on per-provider config sections.
- `local_fs_dev/` — filesystem loopback for dev / demo.

Every adapter must pass the S3 compliance suite at
`tests/s3_compat/`. See [STORAGE_INFRA.md](STORAGE_INFRA.md) for the
adapter status matrix.

### Cache — `cache/hot_object_cache/`

- In-memory LRU `MemoryCache`, on-disk `DiskCache` with a
  rebuild-from-disk index, TTL + capacity eviction, hot-pin support.
- Promotion worker driven by `PromotionPolicy` (monthly egress
  ratio, daily read count, p95 miss latency).

### Migration — `migration/`

- `dual_write/` — provider wrapper that mirrors writes to source
  and destination during cutover.
- `lazy_read_repair/` — GET-path migration that copies objects on
  first read.
- `background_rebalancer/` — bulk drain worker with state-machine
  progress tracking.
- `cross_cell/` — async manifest replicator for tenants whose
  placement policy enables `ReplicationPolicy.Mode == "async"`.
- `fleet_orchestrator.go` — at-scale per-(tenant, bucket) job queue
  with per-cell concurrency caps, exposed via the console API.

### Billing — `billing/`

- `metering.go` — request, storage, egress, and dedup dimensions
  (`dedup_hits`, `dedup_bytes_saved`, `dedup_ref_count`).
- `clickhouse_sink.go` — production ClickHouse sink with batched
  inserts; `logger_sink.go` for dev / demo.
- `forecasting.go` — linear least-squares fit over post-dedup byte
  counts with `ProjectedFillAt` + `Alert` semantics.
- `provider.go` + `registry.go` — per-provider cost-model registry
  used by the placement engine.

### Internal services — `internal/`

- `auth/` — SigV4 authenticator, rate limiter, abuse guard, webhook
  alert sink, DDoS shield (Cloudflare provider + memory shield),
  legal-hold store (memory + Postgres + SQLite — the embedded
  single-node profile persists holds locally across restart).
- `cellops/` — cell registry over the `dedicated_cells` table,
  manual + automated provisioner (Terraform runner stub).
- `compliance/` — audit trail (memory + Postgres + SQLite; the
  embedded profile persists the trail locally, and it now also
  receives lifecycle-evaluator entries), residency enforcer
  (`Check(...)` returns `403 DataResidencyViolation`).
- `config/` — gateway configuration (providers, encryption,
  control plane, abuse, dedup, compliance, console).
- `health/` — `/internal/health`, `/internal/ready`,
  `/internal/drain`.
- `logging/` — process-wide structured logger (slog) plus a bridge
  that points the legacy std `log` package at the same handler, so
  every existing `log.Printf` emits a structured record. Tuned by
  `LOG_LEVEL` (`debug|info|warn|error`, default `info`) and
  `LOG_FORMAT` (`json|text`, default `json`); `ZKOF_PROFILE=compact`
  flips the unset-`LOG_FORMAT` default to `text` for the single-node
  SME posture (an explicit `LOG_FORMAT` always wins). All log output —
  including subsystem loggers built via `logging.NewStdLogger`, which
  previously wrote prefixed text to **stdout** — now goes to a single
  stream, **stderr**, matching the std `log` package default and the
  gateway's own `log.Fatalf` startup diagnostics. A deployment that
  routes stdout and stderr separately should collect logs from stderr.
- `metrics/` — self-contained Prometheus text-format exporter
  (`zkof_request_duration_seconds`, `zkof_cache_hit_total`,
  `zkof_dedup_*`, `zkof_provider_errors_total`,
  `zkof_active_requests`).
- `repair/` — automated repair queue polling a `HealthSignalSource`
  (Ceph mgr health adapter shipped); EC re-encode on degradation.
- `tracing/` — minimal span-based API + HTTP middleware adding
  `tenant_id` / `bucket` / `method` / `backend` attributes per
  request.

## Deployment modes

The same gateway binary runs in three deployment shapes; the storage
backend, metadata DB, and billing sink are wired by the
configuration:

### Dev / Demo (Docker Compose)

- Single container, zero external dependencies.
- `local_fs_dev` filesystem backend.
- In-memory manifest, tenant, content-index, and placement stores.
- Logger billing sink.
- Pre-loaded demo tenant credentials.
- `docker compose up --build` is the entire setup.

See [demo/README.md](../demo/README.md) for full instructions.

### Cloud: AWS + Linode + Wasabi

- **AWS** (control plane): RDS PostgreSQL 16 (multi-AZ, encrypted),
  KMS CMK with annual rotation, IAM roles, CloudWatch dashboards
  and alarms. Provisioned via `deploy/aws/`. **No customer data
  flows through AWS.**
- **Linode** (data plane): per-region NodeBalancer + g6-dedicated
  fleet of gateway instances + NVMe block volumes for the disk
  cache. Provisioned via `deploy/linode/`.
- **Wasabi** (storage backend): per-region buckets named
  `zkof-{region}-{env}` with scoped IAM policy + CORS for
  presigned URLs. Provisioned via `deploy/wasabi/`.
- ClickHouse (managed or self-hosted) for the billing sink.

### Hybrid: + Ceph RGW local DC

- Adds local DC cells provisioned via `deploy/local-dc/`:
  cephadm-bootstrapped Ceph Reef cluster, OSD HDD nodes with
  NVMe BlueStore WAL+DB, RGW front, gateway fleet.
- Migration engine drains data off Wasabi onto local cells via
  `migration/dual_write/`, `lazy_read_repair/`, and
  `background_rebalancer/`.
- Cross-cell async replication via `migration/cross_cell/`.
- Block-level dedup at the Ceph RADOS tier complements the
  gateway's object-level dedup for B2B tenants on dedicated cells.

See [STORAGE_INFRA.md](STORAGE_INFRA.md) for the full
deployment-model → storage-backend mapping and the per-adapter
status table.

### Kubernetes (Helm + HPA)

- The same gateway binary runs as a Kubernetes `Deployment` fronted by
  a `HorizontalPodAutoscaler`, replacing the hand-sized Linode fleet
  (`deploy/linode/`) with auto-scaling and self-healing. Shipped as a
  Helm chart under `deploy/helm/zk-object-fabric/`.
- An init container renders the gateway `config.json` from a ConfigMap
  template by expanding `${...}` placeholders with values from the
  `Secret` (`envsubst`), mirroring the Wasabi production convention in
  `deploy/wasabi/`. `internal/config.Load` reads a single JSON file and
  does not expand env vars, so credentials are injected this way rather
  than as plain env vars. Only the init container mounts the `Secret` as
  env vars (for `envsubst`); the gateway container reads them from the
  rendered config, so they are not exposed in its process environment.
- A production deploy (`config.env: production`) with a persistent
  metadata store must set the manifest-body encryption key
  (`config.encryption.manifestBodyKey`); the gateway refuses to boot
  otherwise (`enforceProductionManifestEncryption`). The key is mounted
  as a file and the chart fails fast at template time if it is missing.
- Readiness (`GET /internal/ready`) gates Service traffic; liveness
  (`GET /internal/health`) restarts wedged pods; a `preStop` hook POSTs
  `/internal/drain` so rolling deploys drain in-flight requests before
  SIGTERM. The HPA targets 70 % CPU (min 2 / max 20 by default).
- The NVMe hot-object cache is a per-pod ephemeral volume by default so
  no replica is pinned to a single node; a `PodDisruptionBudget` keeps
  `minAvailable: 1`. See [../deploy/helm/README.md](../deploy/helm/README.md).

## Port mapping

| Port    | Service                |
| ------- | ---------------------- |
| `:8080` | S3-compatible API      |
| `:8081` | Console API + admin    |

The S3 data plane (`:8080`) and the management plane (`:8081`) run
on separate listeners on the same gateway binary. In production the
console listener is typically not exposed publicly — operators
access it through the AWS control plane VPC or a bastion host.

Internal endpoints exposed by `internal/health/`:

- `GET /internal/health` — liveness
- `GET /internal/ready` — readiness (used by NodeBalancer health check)
- `POST /internal/drain` — graceful drain for rolling deploys

The Prometheus exporter is mounted at `GET /internal/metrics`.

## See also

- [PROPOSAL.md](PROPOSAL.md) §3.4 — `StorageProvider` interface.
- [PROPOSAL.md](PROPOSAL.md) §3.6 — control plane on AWS.
- [PROPOSAL.md](PROPOSAL.md) §3.7 — three-layer data plane.
- [PROPOSAL.md](PROPOSAL.md) §3.8 — encryption modes.
- [PROPOSAL.md](PROPOSAL.md) §3.14 — intra-tenant dedup design.
- [PROPOSAL.md](PROPOSAL.md) §4 — migration engine.
- [PROPOSAL.md](PROPOSAL.md) §6 — cell architecture.
- [STORAGE_INFRA.md](STORAGE_INFRA.md) — deployment-model mapping.
- [INTEGRATION.md](INTEGRATION.md) — external app dedup integration.
- [S3_COMPATIBILITY.md](S3_COMPATIBILITY.md) — supported S3 API surface.
