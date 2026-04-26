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

| Model | Who owns the data plane | Who owns the control plane | Primary backend | Secondary / DR | Cache | Dedup |
| --- | --- | --- | --- | --- | --- | --- |
| **B2C**  | ZK Object Fabric      | ZK Object Fabric | Wasabi (Phase 1 primary) → Ceph RGW pooled cells (Phase 2+) | Backblaze B2 or Cloudflare R2 | Linode NVMe (L0 / L1) | Object-level (intra-tenant, ContentIndex) |
| **B2B**  | ZK Object Fabric (dedicated cell) | ZK Object Fabric | Ceph RGW dedicated cell with EC 8+3 or 10+4 | Second Ceph cell in a different failure domain | Co-located NVMe per cell | Object-level + block-level (Ceph dedup tier) |
| **BYOC** | Customer | ZK Object Fabric (SaaS) | Customer's own S3-compatible backend (AWS S3, GCP, Azure via S3 shim) | Customer responsibility | Customer responsibility | Object-level (intra-tenant, if dedup policy enabled) |
| **Dev / Demo** | Developer laptop (Docker) | ZK Object Fabric (in-memory) | `local_fs_dev` (filesystem) | None | In-memory LRU (L0) | Object-level (in-memory ContentIndex) |

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
- **Dedup**: Object-level intra-tenant dedup via the gateway's
  `ContentIndex`. For `managed` / `public_distribution` modes, the
  gateway derives a convergent DEK from `BLAKE3(plaintext)` (Pattern B).
  For `client_side` convergent mode, the client SDK derives the
  convergent DEK; the gateway deduplicates on `BLAKE3(ciphertext)`
  without seeing plaintext (Pattern C). See
  [INTEGRATION.md](INTEGRATION.md).

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
- **Dedup**: Object-level intra-tenant dedup via `ContentIndex` (same
  as B2C), plus block-level dedup via Ceph's native dedup pool tier
  with content-defined chunking. The combination catches identical files
  (object-level) and similar files (block-level). See
  `deploy/local-dc/README.md` for Ceph dedup tier operator guide.

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

### Dev / Demo — Docker container, zero dependencies

- **Primary**: `local_fs_dev` — a filesystem-backed adapter that
  stores pieces under `/data/objects` inside the container. Backed by
  a Docker volume for persistence across restarts.
- **Control plane**: In-memory. Tenant records loaded from
  `demo/tenants.json` at startup; manifests stored in the in-memory
  `ManifestStore`. No Postgres required.
- **Billing**: `LoggerSink` — structured-log events to stdout.
- **Cache**: In-memory LRU (`memory_cache.go`). No disk cache
  configured by default; operators can set `gateway.cache_path` in
  `demo/config.json` to enable `DiskCache`.
- **Use cases**: Local development, integration testing for downstream
  services (zk-drive, kmail), CI pipelines, demos. Not suitable for
  production — tenant state is ephemeral and there is no replication
  or durability beyond the local filesystem.
- **Quick start**: `docker compose up --build` from the repo root.
  S3 API on `:8080`, console on `:8081`. Demo credentials in
  `demo/tenants.json`.

## Provider adapter matrix

| Adapter | Package | Phase | Role | Status |
| --- | --- | --- | --- | --- |
| `wasabi`        | [`providers/wasabi`](../providers/wasabi)               | 1 | B2C primary (cold origin) | Wired on AWS SDK v2; registered as default provider in `cmd/gateway`; exercised by the S3 compliance suite via `s3_generic` fake. 90-day minimum-storage guardrails shipped. |
| `local_fs_dev`  | [`providers/local_fs_dev`](../providers/local_fs_dev)   | 1 | Dev / conformance loopback | Wired; drives the Phase 2 S3 compliance suite (`tests/s3_compat`), the migration suite, and the benchmark runner. Also serves as the backend for the Docker demo container (`demo/config.json`). |
| `s3_generic`    | [`providers/s3_generic`](../providers/s3_generic)       | 1 | Shared S3-compatible base | Wired on AWS SDK v2. ETag normalization (PR #6). |
| `ceph_rgw`      | [`providers/ceph_rgw`](../providers/ceph_rgw)           | 2 | B2B / sovereign primary | Scaffold — Config, constructor, Capabilities, CostModel, PlacementLabels. Passes conformance against a fake S3 backend; Phase 3 wires a real RGW cluster. |
| `backblaze_b2`  | [`providers/backblaze_b2`](../providers/backblaze_b2)   | 2 | B2C alternative          | Wired; pending live compliance validation — Config, constructor, descriptive methods; registered in `cmd/gateway/main.go#buildProviderRegistry` when `cfg.Providers.BackblazeB2.Endpoint` is set. |
| `cloudflare_r2` | [`providers/cloudflare_r2`](../providers/cloudflare_r2) | 2 | B2C hot-egress backend   | Wired; pending live compliance validation — Config, constructor, descriptive methods; registered in `cmd/gateway/main.go#buildProviderRegistry` when `cfg.Providers.CloudflareR2.AccountID` or `cfg.Providers.CloudflareR2.Endpoint` is set. |
| `aws_s3`        | [`providers/aws_s3`](../providers/aws_s3)               | 2 | BYOC / DR-only           | Wired; pending live compliance validation — Config, constructor, descriptive methods; registered in `cmd/gateway/main.go#buildProviderRegistry` when `cfg.Providers.AWSS3.Region` is set. |
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

## Intra-tenant deduplication

Deduplication in the ZK Object Fabric operates **exclusively within a
single tenant**. Cross-tenant dedup is permanently excluded — sharing
physical pieces across tenants would create a privacy side channel
(tenant A could probe to learn whether tenant B holds a given file)
and is incompatible with the per-tenant key isolation contract. Two
scenarios are supported: **B2C community** (viral files, shared media
inside a single SME or community tenant) and **B2B org** (company-wide
documents, shared attachments inside a single enterprise tenant).

### Object-level dedup (all backends)

The gateway computes a `BLAKE3` content hash on every PUT, looks up
`ContentIndex(tenant_id, hash)`, skips the backend write on a match,
and increments a refcount. The mechanism is backend-agnostic: it
works identically on Wasabi, Ceph, Storj, Backblaze B2, Cloudflare
R2, AWS S3, and `local_fs_dev`. DELETE is reference-counted — the
physical piece is only removed when the refcount reaches zero, at
which point the `ContentIndex` row is dropped.

### Block-level dedup (Ceph RGW only)

Ceph's native dedup pool tier with content-defined chunking runs at
the RADOS layer underneath the RGW S3 surface. It is transparent to
the gateway — no code changes, no manifest changes. Block-level dedup
is enabled only for B2B dedicated cells; the operator configures the
dedup tier on the Ceph pool. See `deploy/local-dc/README.md` for the
operator guide.

### Dedup by deployment model

| Model | Object-level | Block-level | DR copy deduped? |
| --- | --- | --- | --- |
| **B2C** | Yes (ContentIndex) | No (Wasabi has no native dedup) | No (full object on DR) |
| **B2B** | Yes (ContentIndex) | Yes (Ceph dedup tier) | No (full object on second cell) |
| **BYOC** | Yes (ContentIndex) | Depends on customer backend | Customer responsibility |
| **Dev / Demo** | Yes (in-memory) | No | N/A |

### Encryption requirements

Dedup requires the encryption mode to produce stable ciphertext for
identical plaintext. The mapping from encryption mode to dedup
pattern is:

| Encryption mode | Dedup pattern | Notes |
| --- | --- | --- |
| `managed` | Pattern B (gateway convergent) | Gateway derives DEK from `BLAKE3(plaintext)`. |
| `public_distribution` | Pattern B (gateway convergent) | Same as `managed`; presigned-URL distribution. |
| `client_side` + convergent | Pattern C (client convergent) | Client SDK derives DEK; gateway dedups on `BLAKE3(ciphertext)`. |
| `client_side` + random | No dedup | Random DEK per object — ciphertext is unique. |

See [INTEGRATION.md](INTEGRATION.md) for the external app
integration guide.

### MLS compatibility

MLS forward secrecy (FS) and post-compromise security (PCS) are
**message-channel** properties — they govern the chat/messaging key
ratchet and are fully preserved. File encryption (DEK derivation,
ciphertext storage) is decoupled from the MLS message channel, so
convergent dedup on stored files does not weaken MLS FS/PCS on the
message stream. See [INTEGRATION.md](INTEGRATION.md) §7 for details.

## See also

- [PROPOSAL.md §2](PROPOSAL.md) — commercial envelope and cost model
- [PROPOSAL.md §3.4](PROPOSAL.md) — StorageProvider interface contract
- [PROPOSAL.md §3.9](PROPOSAL.md) — placement DSL and erasure-coding
- [PROPOSAL.md §3.14](PROPOSAL.md) — intra-tenant deduplication design
- [PROPOSAL.md §4](PROPOSAL.md) — migration engine
- [INTEGRATION.md](INTEGRATION.md) — external app integration guide (KChat, non-MLS apps)
- [PROGRESS.md](PROGRESS.md) — phase-gated tracker
- [demo/README.md](../demo/README.md) — Docker demo quick-start and downstream integration guide
