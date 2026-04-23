# Storage Infrastructure — Deployment-Model Mapping

This document maps the ZK Object Fabric's storage infrastructure to
the three deployment models the product supports. It is the operator-
facing complement to [PROPOSAL.md](PROPOSAL.md) §2 and §3.

It answers two questions:

1. "Given a tenant's deployment model, which backends do we put their
   ciphertext on?"
2. "What does the provider adapter matrix look like today, and which
   adapters land in which phase?"

## Deployment models

| Model | Who owns the data plane | Who owns the control plane | Primary backend | Secondary / DR | Cache |
| --- | --- | --- | --- | --- | --- |
| **B2C**  | ZK Object Fabric      | ZK Object Fabric | Wasabi (Phase 1 primary) → Ceph RGW pooled cells (Phase 2+) | Backblaze B2 or Cloudflare R2 | Linode NVMe (L0 / L1) |
| **B2B**  | ZK Object Fabric (dedicated cell) | ZK Object Fabric | Ceph RGW dedicated cell with EC 8+3 or 10+4 | Second Ceph cell in a different failure domain | Co-located NVMe per cell |
| **BYOC** | Customer | ZK Object Fabric (SaaS) | Customer's own S3-compatible backend (AWS S3, GCP, Azure via S3 shim) | Customer responsibility | Customer responsibility |

### B2C — multi-tenant shared fabric

- **Primary (Phase 1)**: Wasabi. See PROPOSAL.md §2.1 for why.
- **Primary (Phase 2+)**: Ceph RGW pooled cells. See PROPOSAL.md §4 for
  the Wasabi → local-DC migration playbook. Ceph RGW is the
  recommended production base: S3-compatible, LGPL-2.1, and mature at
  the 2–20 PB cell sizes the fabric targets.
- **Alternatives**: Backblaze B2 ($6 / TB-month storage, cheaper than
  Wasabi but with non-zero egress) and Cloudflare R2 (zero egress,
  ideal for hot-read workloads).
- **Cache**: Linode NVMe at the edge (L0) and per-gateway (L1). The
  Wasabi fair-use guardrails (see
  [providers/wasabi/guardrails.go](../providers/wasabi/guardrails.go))
  drive hot-object promotion into the cache so per-tenant origin
  egress stays ≤ 1× stored bytes per month.

### B2B — dedicated cells with sovereign placement

- **Primary**: Ceph RGW in a dedicated cell per tenant (or per small
  cohort of tenants sharing a compliance boundary). Storage is Reed-
  Solomon erasure-coded at 8+3 or 10+4 — see
  [metadata/erasure_coding/profile.go](../metadata/erasure_coding/profile.go)
  for the shipped profiles.
- **Secondary**: A second Ceph cell in a different failure domain for
  the 2-copy durability contract, plus optional off-site replication
  to Wasabi or AWS S3 (via the `aws_s3` adapter) for DR.
- **Placement**: `placement_policy` enforces country and region
  allow-lists. Sovereign tenants never route through the B2C pool.
- **Cache**: Co-located NVMe tier inside the cell. No cross-cell cache
  traffic.

### BYOC — customer-owned backend, ZK Object Fabric as SaaS

- **Primary**: Customer's own S3-compatible service. The fabric ships
  `aws_s3`, `cloudflare_r2`, `backblaze_b2`, and `storj` adapters
  today; any S3-compatible backend can be plugged in by reusing
  `providers/s3_generic` (Ceph RGW, MinIO, GCS via the Google S3
  adapter, Azure Blob via the S3 compatibility shim). The `storj`
  adapter is special-cased: it does not embed `s3_generic.Provider`
  and instead drives Storj through `storj.io/uplink`, so BYOC
  tenants pointed at a Storj satellite share the ZK semantics of
  the rest of the fabric without a second S3 hop.
- **Control plane**: Hosted by the fabric. No customer data flows
  through AWS (see the contract test at
  [tests/control_plane/no_data_test.go](../tests/control_plane/no_data_test.go)).
- **Use cases**: Regulated customers who already hold a backend
  contract (for example, an enterprise with a long-lived AWS S3
  commitment) and want the fabric's ZK semantics + S3 gateway on top
  of their existing spend.

## Provider adapter matrix

| Adapter | Package | Phase | Role | Status |
| --- | --- | --- | --- | --- |
| `wasabi`        | [`providers/wasabi`](../providers/wasabi)               | 1 | B2C primary (cold origin) | Wired on AWS SDK v2; registered as default provider in `cmd/gateway`; exercised by the S3 compliance suite via `s3_generic` fake. 90-day minimum-storage guardrails shipped. |
| `local_fs_dev`  | [`providers/local_fs_dev`](../providers/local_fs_dev)   | 1 | Dev / conformance loopback | Wired; drives the Phase 2 S3 compliance suite (`tests/s3_compat`), the migration suite, and the benchmark runner. |
| `s3_generic`    | [`providers/s3_generic`](../providers/s3_generic)       | 1 | Shared S3-compatible base | Wired on AWS SDK v2. ETag normalization (PR #6). |
| `ceph_rgw`      | [`providers/ceph_rgw`](../providers/ceph_rgw)           | 2 | B2B / sovereign primary | Scaffold — Config, constructor, Capabilities, CostModel, PlacementLabels. Passes conformance against a fake S3 backend; Phase 3 wires a real RGW cluster. |
| `backblaze_b2`  | [`providers/backblaze_b2`](../providers/backblaze_b2)   | 2 | B2C alternative          | Scaffold — Config, constructor, descriptive methods. |
| `cloudflare_r2` | [`providers/cloudflare_r2`](../providers/cloudflare_r2) | 2 | B2C hot-egress backend   | Scaffold — Config, constructor, descriptive methods. |
| `aws_s3`        | [`providers/aws_s3`](../providers/aws_s3)               | 2 | BYOC / DR-only           | Scaffold — Config, constructor, descriptive methods. |
| `storj`         | [`providers/storj`](../providers/storj)                 | 2 | BYOC / ZK reference      | Wired end-to-end via `storj.io/uplink v1.14.0`; `uplink_bridge.go` adapts `*uplink.Project` to the `UplinkProject` interface; registered in `cmd/gateway/main.go#buildProviderRegistry` when `cfg.Providers.Storj.AccessGrant` is set; `TestSuite_Storj` in `tests/s3_compat/suite_test.go` gated on `STORJ_ACCESS_GRANT` + `STORJ_BUCKET`; nightly CI in `.github/workflows/storj-compliance.yml`. |

`wasabi`, `ceph_rgw`, `backblaze_b2`, `cloudflare_r2`, and `aws_s3`
all embed `s3_generic.Provider` so the API surface is identical; only
Capabilities, CostModel, and PlacementLabels differ. `storj` is the
deliberate exception — it does not embed `s3_generic.Provider` and
instead adapts `*uplink.Project` through `providers/storj/uplink_bridge.go`
so the fabric's ZK envelope composes cleanly with Storj's native
reed-solomon segment distribution.

## Reference implementations we draw on

- **Ceph RGW** (LGPL-2.1) — recommended Phase 2+ local-DC base. Mature
  at cell-sized deployments; battle-tested S3-compatible surface. See
  <https://docs.ceph.com/en/latest/radosgw/>.
- **Storj** (AGPL-3.0) — the canonical reference for zero-knowledge
  distributed storage and Reed-Solomon placement. We study it for
  design patterns (manifest shape, metainfo API, erasure coding
  policy) but **do not** vendor its code: AGPL would infect the
  fabric's control plane. See <https://github.com/storj/storj>.
- **SeaweedFS** (Apache-2.0) — lower operational complexity than
  Ceph; a viable alternative for faster Phase 2 iteration at the cost
  of smaller community and fewer large-scale proof points. See
  <https://github.com/seaweedfs/seaweedfs>.
- **MinIO** (AGPL-3.0) — frequently requested by operators but AGPL
  limits our ability to embed or vendor. Usable via the `s3_generic`
  adapter for BYOC customers who already run MinIO.

## Erasure-coding profiles

The [shipped profiles](../metadata/erasure_coding/profile.go) target
the four deployment points below. All use a 4 MiB stripe.

| Profile | Data / Parity | Raw overhead | Target |
| ------- | ------------- | ------------ | ------ |
| 6+2     | 6+2           | 1.333x       | Smallest B2C pool (entry cells) |
| 8+3     | 8+3           | 1.375x       | Default B2B (sovereign) production profile |
| 10+4    | 10+4          | 1.4x         | High-durability B2B, long-tail archival |
| 12+4    | 12+4          | 1.333x       | Wide B2B cell, prioritises COGS over fan-out |
| 16+4    | 16+4          | 1.25x        | Cold/archival B2B cell |

## See also

- [PROPOSAL.md §2](PROPOSAL.md) — commercial envelope and cost model
- [PROPOSAL.md §3.4](PROPOSAL.md) — StorageProvider interface contract
- [PROPOSAL.md §3.9](PROPOSAL.md) — placement DSL and erasure-coding
- [PROPOSAL.md §4](PROPOSAL.md) — migration engine
- [PROGRESS.md](PROGRESS.md) — phase-gated tracker
