# ZK Object Fabric — Phase Summary

This is a one-page index of the project's phase plan and current
status. It exists so a reader can answer "what is shipped, what is
in progress, and where is the detail?" without scrolling through the
full progress tracker.

- For the **detailed phase checklist** and changelog, see
  [PROGRESS.md](PROGRESS.md).
- For the **technical design** (manifest format, encryption envelope,
  placement DSL, dedup, migration engine, cell architecture), see
  [PROPOSAL.md](PROPOSAL.md).
- For the **as-built architecture** (directory layout, components,
  deployment modes, port mapping), see [ARCHITECTURE.md](ARCHITECTURE.md).
- For the **dedup integration patterns** external apps use, see
  [INTEGRATION.md](INTEGRATION.md).
- For the **deployment-model → storage-backend** mapping, see
  [STORAGE_INFRA.md](STORAGE_INFRA.md).

| Phase | Title | Status |
| --- | --- | --- |
| 1 | Architecture Proof | COMPLETE |
| 2 | Prototype | COMPLETE |
| 3 | Beta Cell | IMPLEMENTATION COMPLETE (production validation pending) |
| 3.5 | Intra-Tenant Deduplication | IMPLEMENTATION COMPLETE (production validation pending) |
| 4 | Production & Scale | ~75% implementation scaffold complete |

---

## Phase 1: Architecture Proof

**Status**: COMPLETE

**Goal**: lock the architecture on the AWS control plane + Linode
data plane + Wasabi storage backend stack, ratify the
provider-neutral manifest, and produce enough formal specification
that Phase 2 engineers can implement without re-debating core
decisions.

**Key deliverables (brief)**:

- Provider-neutral object manifest format and encryption envelope.
- Placement-policy DSL and erasure-coding profiles (6+2, 8+3, 10+4).
- S3 compatibility subset spec (PUT, GET, HEAD, DELETE, LIST,
  multipart, range GET) as the phase-invariant contract.
- Multi-tenancy schema (contract type, license tier, keys,
  placement default, budgets, abuse, billing).
- Migration-engine spec (dual-write, lazy migration on read,
  background rebalancer, state machine).
- Linode cache design (NVMe sizing, promotion / eviction policies)
  and Wasabi fair-use guardrails.
- Benchmark suite spec (PUT/GET p50/p95/p99, cache-hit ratio,
  Wasabi origin egress ratio, LIST scaling).

**Design decision**: Phase 2+ local-DC base → **Ceph RGW** (production
maturity, operational tooling, EC support). SeaweedFS retained as
documented fallback. AGPL options ruled out by license; Garage ruled
out by lack of EC.

See [PROGRESS.md](PROGRESS.md) for the detailed checklist.

---

## Phase 2: Prototype

**Status**: COMPLETE

**Goal**: a single-cell prototype that PUTs, GETs, HEADs, DELETEs,
LISTs, and range-reads encrypted objects end-to-end against Wasabi
via the Linode gateway, with the migration engine wired up for a
dry-run cut-over to a local DC cell.

**Key deliverables (brief)**:

- S3-compatible gateway in Go (`api/s3compat/`) with handlers for
  PUT / GET / HEAD / DELETE / LIST / multipart / range GET / presigned
  URLs / SigV4 / multipart lifecycle.
- Pluggable storage providers wired through a single
  `StorageProvider` interface: `wasabi`, `s3_generic`, `local_fs_dev`.
- Client SDK with XChaCha20-Poly1305 chunked encryption and per-object
  envelope DEKs.
- Hot object cache (`cache/hot_object_cache/`) — memory + disk LRU
  with promotion worker.
- Migration engine state machine (`migration/`) with dual-write +
  lazy read repair + background rebalancer.
- S3 compliance test suite (`tests/s3_compat/`) running cross-adapter.

See [PROGRESS.md](PROGRESS.md) for the detailed checklist.

---

## Phase 3: Beta Cell

**Status**: IMPLEMENTATION COMPLETE — production validation pending
(see the **Production Readiness** section in
[PROGRESS.md](PROGRESS.md#production-readiness)).

**Goal**: stand up a real beta deployment on the AWS + Linode +
Wasabi stack with paying / design-partner customers on both B2C and
B2B paths, plus a first local DC cell for early hybrid customers.

**Key deliverables (brief)**:

- Production AWS control plane (Terraform modules under `deploy/aws/`
  for RDS, IAM, KMS, CloudWatch dashboards/alarms).
- Production Linode gateway fleet (`deploy/linode/`: Terraform +
  NodeBalancer + cloud-init + systemd unit + Caddy TLS).
- Multi-region Wasabi origin (`deploy/wasabi/` idempotent bucket
  provisioner + per-bucket IAM policy + CORS).
- Production CMK wrappers: AWS KMS (`kms_wrapper.go`), HashiCorp
  Vault Transit (`vault_wrapper.go`), local-file dev fallback.
- First local DC cell scaffolding (`deploy/local-dc/`: cephadm
  bootstrap + Ansible host prep + Prometheus scrape + RGW config).
- Tenant console (React + Vite under `frontend/`) + console API
  (`api/console/`) on `:8081` with admin auth.
- Postgres-backed `AuthStore`, `PlacementStore`, `ManifestStore`,
  `DedicatedCellStore`, and `LegalHoldStore`.
- Live Wasabi → Ceph RGW migration compliance gate
  (`tests/s3_compat/live_migration_test.go`).
- Beta runbooks (`docs/runbooks/`): CMK rotation, beta onboarding,
  tenant setup, BYOC setup.
- Nightly BYOC compliance CI (`.github/workflows/byoc-compliance.yml`).
- Reference downstream integrations: kmail, zk-drive, Kapp Business
  Suite (all `managed`-mode), KChat (`client_side` convergent).

See [PROGRESS.md](PROGRESS.md) for the detailed checklist.

---

## Phase 3.5: Intra-Tenant Deduplication

**Status**: IMPLEMENTATION COMPLETE — production validation pending
(see the **Production Readiness** section in
[PROGRESS.md](PROGRESS.md#production-readiness)).

**Goal**: add object-level and block-level intra-tenant deduplication
to reduce storage costs for B2C community (viral / shared files) and
B2B org (company-wide documents) workloads. Cross-tenant dedup is
permanently excluded.

**Key deliverables (brief)**:

- `ContentHash` field on `ObjectManifest`; `DedupPolicy` on
  `PlacementPolicy`.
- `ContentIndexStore` interface (`metadata/content_index/`) with
  in-memory and Postgres implementations; race-safe `Register`,
  atomic `DecrementRef`, `CHECK ref_count >= 0`.
- Pattern B (gateway convergent encryption) and Pattern C
  (client-side convergent encryption) PUT paths in
  `api/s3compat/dedup.go`.
- `DeriveConvergentDEK` (HKDF-SHA256, tenant-salted) and
  `ConvergentNonce` (HKDF-derived deterministic nonce) in
  `encryption/client_sdk/`.
- Reference-counted DELETE (`api/s3compat/handler.go#Delete`).
- Multipart dedup at `CompleteMultipartUpload` (single-piece +
  multi-piece via the `piece_ids` JSONB column; deferred convergent
  consolidation for `managed` / `public_distribution`).
- Console API at `/api/v1/tenants/{tid}/buckets/{bucket}/dedup-policy`
  with the `object+block` Ceph-RGW + dedicated-cell guardrail.
- Ceph RGW block-level dedup operator guide.
- Dedup billing dimensions: `dedup_hits`, `dedup_bytes_saved`,
  `dedup_ref_count`.
- S3 compliance + benchmark coverage:
  `tests/s3_compat/dedup_test.go`, B2C-80%-dup and B2B-60%-dup
  benchmark scenarios.
- External-app integration patterns documented in
  [INTEGRATION.md](INTEGRATION.md).

See [PROGRESS.md](PROGRESS.md) for the detailed checklist.

---

## Phase 4: Production & Scale

**Status**: IN PROGRESS — **~75% implementation scaffold complete**
(9 of 12 checklist items implemented; none yet production-validated —
see the **Production Readiness** section in
[PROGRESS.md](PROGRESS.md#production-readiness))

**Goal**: move from a single beta deployment to a production,
multi-cell fabric with published product tiers and operational
maturity. Wasabi remains the cloud overflow / DR backend; owned
local DC cells become the primary origin.

**Key deliverables (brief)** — items shipped:

- Cell architecture (`internal/cellops/registry.go`,
  `automated_provisioner.go`).
- Cross-cell replication (`migration/cross_cell/replicator.go`,
  policy-driven, dest-side `Backend` rewrite).
- Automated repair queue (`internal/repair/repair_queue.go`,
  Ceph-mgr health adapter, EC re-encode).
- Abuse / DDoS / legal-response operations (`internal/auth/ddos_shield.go`,
  `legal_response.go`, Cloudflare provider, fully wired DELETE-path
  legal-hold check).
- Observability (`internal/metrics/prometheus.go`,
  `internal/tracing/tracing.go`).
- Capacity forecasting (`billing/forecasting.go` linear-fit
  forecaster + console handler at
  `GET /api/v1/cells/{cellId}/forecast`).
- Region-specific compliance — **GDPR pre-flight only** (audit trail
  + residency enforcer + legal response, all wired end-to-end);
  HIPAA / FINRA / SEC are explicitly out of Phase 4 scope.
- Published product tiers (`metadata/tenant/tier_config.go`,
  `api/console/tier_handler.go`, `frontend/src/pages/TiersPage.tsx`).
- At-scale fleet migration (`migration/fleet_orchestrator.go`,
  console handlers `GET /api/v1/migrations` and
  `GET /api/v1/migrations/{jobId}`).

**Open items (3 of 12)** — operational, outside this repo:

- Hardware procurement engine for high-density HDD nodes.
- DC and power strategy.
- Global peering and transit.

See [PROGRESS.md](PROGRESS.md) for the detailed checklist.
