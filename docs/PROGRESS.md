# ZK Object Fabric — Progress

- **Project**: ZK Object Fabric
- **License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).
- **Status**: Phase 1 — Architecture Proof
- **Last updated**: 2026-04-22 (Phase 1 complete; Wasabi adapter wired on AWS SDK v2)

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

**Status**: `NOT STARTED`

**Goal**: a single-cell prototype that can PUT, GET, HEAD, DELETE,
LIST, and range-read encrypted objects end-to-end, backed by Wasabi
via the Linode gateway, with the migration engine wired up for a
dry-run cut-over to a local DC cell.

Checklist:

- [ ] S3-compatible gateway on Linode (Go).
- [ ] Client-side encryption SDK.
- [ ] Encrypted manifest storage in the AWS control plane.
- [ ] Storage provider adapter framework (`wasabi`, `local_fs_dev`,
      stubs for `backblaze_b2`, `cloudflare_r2`, `aws_s3`).
- [ ] Placement engine (provider + region + country + storage_class).
- [ ] Wasabi durable origin wired up as the primary backend.
- [ ] Linode hot cache (L0 / L1) with promotion rules.
- [ ] Node health monitor for the Linode gateway fleet.
- [ ] Basic billing counters (per-tenant storage-seconds, PUTs,
      GETs, egress bytes).
- [ ] Range GET support, range-aligned cache chunks.
- [ ] Hot-object promotion from Wasabi to Linode cache.
- [ ] Multi-tenant isolation layer.
- [ ] Migration engine: dual-write, lazy migration on read,
      background rebalancer (exercised against a `local_fs_dev`
      target).
- [ ] Implement S3 compliance test suite (`tests/s3_compat/`) and
      run against `wasabi` and `local_fs_dev` adapters.
- [ ] Validate S3 API behavior during a simulated Wasabi →
      `local_fs_dev` migration (zero behavioral differences).
- [ ] Benchmark execution (PUT / GET p50 / p95 / p99, cache hit
      ratio, Wasabi origin egress ratio vs stored bytes,
      small-object overhead, LIST performance at 10M / 100M / 1B
      objects).

---

## Phase 3: Beta Cell (Weeks 10–15)

**Status**: `NOT STARTED`

**Goal**: stand up a real beta deployment on the AWS + Linode +
Wasabi stack with paying / design-partner customers on both B2C and
B2B paths, plus a first local DC cell for early hybrid customers.

Checklist:

- [ ] Production AWS control plane (RDS, IAM, CloudWatch,
      ClickHouse or equivalent).
- [ ] Production Linode gateway fleet, multi-region.
- [ ] Production Wasabi buckets (per region) wired as the durable
      origin.
- [ ] NVMe cache nodes (L0 / L1) on Linode.
- [ ] First local DC cell: 6–12 storage nodes, 300 TB – 1 PB raw
      capacity, HDD durable nodes (L2), NVMe cache, gateway fleet.
- [ ] 25–100 Gbps aggregate public bandwidth across Linode + local
      DC.
- [ ] Abuse throttling and per-tenant bandwidth budgets.
- [ ] Tenant console (React) for onboarding, billing, placement
      policy, and key management.
- [ ] B2C self-service onboarding flow.
- [ ] B2B dedicated cell provisioning.
- [ ] Beta customer onboarding (backup, SaaS assets, AI datasets,
      media libraries, sovereign storage).
- [ ] End-to-end migration dry run: move a beta bucket from Wasabi
      to the first local cell without customer-visible changes.
- [ ] Run S3 compliance test suite against `ceph_rgw` adapter —
      100% pass required before production traffic.
- [ ] Run S3 compliance test suite during a live Wasabi → Ceph RGW
      migration with beta customers.

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

## Phase 4: Production & Scale (Post-Beta)

**Status**: `NOT STARTED`

**Goal**: move from a single beta deployment to a production,
multi-cell fabric with published product tiers and operational
maturity. Wasabi remains the cloud overflow / DR backend; owned local
DC cells become the primary origin.

Checklist:

- [ ] Cell architecture (multi-cell, 2–20 PB per cell).
- [ ] Cross-cell replication (opt-in, policy-driven).
- [ ] Hardware procurement engine for high-density HDD nodes.
- [ ] DC and power strategy.
- [ ] Global peering and transit.
- [ ] Automated repair and drive replacement.
- [ ] Abuse, DDoS, and legal response operations.
- [ ] Observability stack (metrics, traces, logs at scale).
- [ ] Capacity forecasting and supply planning.
- [ ] Region-specific compliance (GDPR, HIPAA, FedRAMP, etc).
- [ ] Published public product tiers (ZK Archive, ZK Standard, ZK
      Hot, ZK Dedicated, ZK Sovereign).
- [ ] At-scale migration: drain remaining Wasabi-backed tenants off
      the cloud origin onto local cells where their placement policy
      requires it.

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
