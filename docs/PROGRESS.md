# Uney ZK Object Fabric — Progress

- **Project**: Uney ZK Object Fabric
- **License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).
- **Status**: Phase 1 — Architecture Proof
- **Last updated**: 2026-04-21

This document is a phase-gated tracker. Each phase has an explicit
checklist and a decision gate. Do not skip to the next phase until the
current phase's gate has been met.

For the technical design, see [PROPOSAL.md](PROPOSAL.md).

---

## Phase 1: Architecture Proof (Weeks 1–3)

**Status**: `IN PROGRESS`

**Goal**: lock the architecture, choose the open-source base, and produce
enough formal specification that Phase 2 engineers can implement without
re-debating core decisions.

Checklist:

- [ ] Select open-source base (Ceph RGW vs SeaweedFS — AGPL options ruled out)
- [ ] Define object manifest format
- [ ] Define encryption envelope (per-object DEK, encrypted manifest, CMK support)
- [ ] Define placement policy DSL (YAML schema)
- [ ] Define erasure coding profiles (6+2, 8+3, 10+4)
- [ ] Define S3 compatibility subset (PUT, GET, HEAD, DELETE, LIST, multipart, range GET)
- [ ] Define benchmark suite (PUT/GET latency percentiles, cache hit ratio, repair time, network cost)
- [ ] Define multi-tenancy model (tenant isolation, billing, abuse controls)
- [ ] Decision gate: base selection

### Phase 1 decision gate: base selection

AGPL options are ruled out because Uney ships under a proprietary
license. Garage is ruled out because it does not support erasure coding
and therefore cannot meet Phase 2's EC durable-origin requirement.

| Requirement                              | Pick        |
| ---------------------------------------- | ----------- |
| Maximum production maturity              | Ceph RGW    |
| Faster custom product build              | SeaweedFS   |

---

## Phase 2: Prototype (Weeks 4–9)

**Status**: `NOT STARTED`

**Goal**: a single-cell prototype that can PUT, GET, HEAD, DELETE, LIST,
and range-read encrypted objects end-to-end, with EC durability, a
working placement engine, node health monitoring, and a basic repair
worker.

Checklist:

- [ ] S3-compatible gateway
- [ ] Client-side encryption SDK
- [ ] Encrypted manifest storage
- [ ] Placement engine
- [ ] 8+3 or 10+4 EC durable origin
- [ ] Node health monitor
- [ ] Repair worker
- [ ] Basic billing counters (per-tenant)
- [ ] Range GET support
- [ ] Hot-object promotion to cache
- [ ] Multi-tenant isolation layer
- [ ] Benchmark execution (PUT/GET p50/p95/p99, cache hit ratio, repair time, small-object overhead, list performance at 10M/100M/1B objects)

---

## Phase 3: Beta Cell (Weeks 10–15)

**Status**: `NOT STARTED`

**Goal**: stand up a real beta cell with paying / design-partner
customers on both B2C and B2B paths.

Checklist:

- [ ] Deploy 3 locations
- [ ] 6–12 storage nodes
- [ ] 300 TB–1 PB raw capacity
- [ ] 25–100 Gbps aggregate public bandwidth
- [ ] NVMe cache nodes (L0/L1)
- [ ] HDD durable nodes (L2)
- [ ] S3 gateway fleet
- [ ] Abuse throttling
- [ ] Per-tenant bandwidth budgets
- [ ] B2C self-service onboarding flow
- [ ] B2B dedicated cell provisioning
- [ ] Beta customer onboarding (backup, SaaS assets, AI datasets, media libraries, sovereign storage)

### Avoid early customers with

- Random high-egress public download traffic.
- Tiny-object, billions-scale workloads (unless packed into containers).
- Heavy compliance requirements before Uney has completed its audits.
- Latency-critical transactional workloads (Uney targets object storage,
  not a transactional KV).

---

## Phase 4: Production & Scale (Post-Beta)

**Status**: `NOT STARTED`

**Goal**: move from a single beta cell to a production, multi-cell
fabric with published product tiers and operational maturity.

Checklist:

- [ ] Cell architecture (multi-cell, 2–20 PB per cell)
- [ ] Cross-cell replication
- [ ] Hardware procurement engine
- [ ] DC and power strategy
- [ ] Global peering
- [ ] Automated repair and drive replacement
- [ ] Abuse, DDoS, and legal response operations
- [ ] Observability stack
- [ ] Capacity forecasting
- [ ] Region-specific compliance
- [ ] Public product tiers (Archive, Standard, Hot, Dedicated, Sovereign)

---

## Appendix: Key Metrics to Track

| Metric                              | Target                              | Phase   |
| ----------------------------------- | ----------------------------------- | ------- |
| PUT p99 latency                     | TBD                                 | Phase 2 |
| GET p99 latency (cache hit)         | TBD                                 | Phase 2 |
| GET p99 latency (EC reconstruct)    | TBD                                 | Phase 2 |
| Cache hit ratio (hot tier)          | > 90%                               | Phase 3 |
| Repair time (single node loss)      | TBD                                 | Phase 2 |
| Storage COGS / TB-month             | < $3.00 at 1 PB                     | Phase 3 |
| Erasure overhead                    | 1.375× (8+3) or 1.4× (10+4)         | Phase 2 |
