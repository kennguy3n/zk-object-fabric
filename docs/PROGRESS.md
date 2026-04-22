# ZK Object Fabric — Progress

- **Project**: ZK Object Fabric
- **License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).
- **Status**: Phase 1 — Architecture Proof
- **Last updated**: 2026-04-21 (Phase 1 scaffold landed)

This document is a phase-gated tracker. Each phase has an explicit
checklist and a decision gate. Do not skip to the next phase until the
current phase's gate has been met.

For the technical design, see [PROPOSAL.md](PROPOSAL.md).

---

## Phase 1: Architecture Proof (Weeks 1–3)

**Status**: `IN PROGRESS`

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
- [ ] Confirm that no customer data flows through AWS (contract test
      on control-plane API surface).
- [ ] Select the Phase 2+ local-DC base (Ceph RGW vs SeaweedFS —
      AGPL options are ruled out).
- [x] Define the provider-neutral object manifest format (implemented
      in `metadata/manifest.go` with JSON round-trip coverage in
      `metadata/manifest_test.go`).
- [x] Define the encryption envelope (per-object DEK, encrypted
      manifest, CMK support) — implemented in
      `encryption/envelope.go`.
- [x] Define the placement policy DSL (YAML schema) — implemented in
      `metadata/placement_policy/policy.go`.
- [ ] Define erasure-coding profiles for Phase 2+ (6+2, 8+3, 10+4).
      Note: Phase 1 uses Wasabi's native durability; EC is not needed
      until Phase 2+.
- [ ] Define the S3 compatibility subset (PUT, GET, HEAD, DELETE,
      LIST, multipart, range GET).
- [ ] Define the benchmark suite (PUT / GET latency percentiles,
      cache hit ratio, repair time, Wasabi origin egress ratio,
      network cost).
- [ ] Define the multi-tenancy model (tenant isolation, billing,
      abuse controls).
- [x] Define the migration engine spec (dual-write, lazy migration on
      read, background rebalancer, migration state machine) — state
      machine in `migration/state.go` with transition coverage in
      `migration/state_test.go`.
- [ ] Specify the Linode cache design (NVMe / block storage sizing,
      promotion rules, range-aligned chunking).
- [ ] Specify Wasabi fair-use guardrails (egress budgets, per-tenant
      cache hit ratio targets, 90-day minimum storage handling).
- [ ] Decision gate: Phase 2+ base selection.

### Phase 1 decision gate: base selection

AGPL options are ruled out because ZK Object Fabric ships under a
proprietary license. Garage is ruled out because it does not support
erasure coding and therefore cannot meet Phase 2+'s EC
durable-origin requirement.

| Requirement                              | Pick        |
| ---------------------------------------- | ----------- |
| Maximum production maturity              | Ceph RGW    |
| Faster custom product build              | SeaweedFS   |

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
