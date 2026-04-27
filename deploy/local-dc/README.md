# Local DC Cell — Ceph RGW

Phase 3 stand-up for the first local data-center cell. This is the
B2B / sovereign deployment model: 6–12 storage nodes running Ceph
with the RADOS Gateway (RGW) for S3-compatible access, fronted by
the same gateway fleet as Phase 2 but routing placement to a local
`ceph_rgw` provider rather than Wasabi.

## Architecture

```
                   ┌─────────────────────┐
                   │ Gateway Fleet (3-5) │
                   │  + NVMe HotCache    │
                   └──────────┬──────────┘
                              │ S3
                              ▼
                ┌─────────────────────────────┐
                │  Ceph RGW (3 instances)     │
                └──────────────┬──────────────┘
                               │ RADOS
                               ▼
        ┌───────────────────────────────────────────┐
        │  Ceph OSDs (6-12 nodes, HDD durable + NVMe│
        │  cache tier or DiskCache on gateway side) │
        └───────────────────────────────────────────┘
```

## Sizing (beta cell)

| Component        | Count          | Storage                        | Network     |
| ---------------- | -------------- | ------------------------------ | ----------- |
| Mon              | 3              | 100 GB SSD per node            | 10 Gbps     |
| Manager          | 2              | shared with mons               | 10 Gbps     |
| RGW              | 3              | 200 GB SSD per node            | 25 Gbps     |
| OSD (HDD)        | 6 (beta) – 12  | 12× 18 TB HDD per node         | 25 Gbps front + 25 Gbps cluster |
| Cache (NVMe)     | bake into OSD nodes (BlueStore WAL/DB on NVMe) | 1.6 TB NVMe per OSD node | — |
| Gateway fleet    | 3              | 1 TB NVMe cache per node       | 25 Gbps     |

Beta target: 300 TB raw / 100 TB usable (3× replication) or
~225 TB usable on EC 6+2 — start with replication for the beta,
flip to EC for the production cell once the rebalancer has been
exercised in production.

## Layout

| Path                                  | Purpose                                                                |
| ------------------------------------- | ---------------------------------------------------------------------- |
| `cephadm/install.sh`                  | Bootstrap a Ceph Reef cluster via cephadm.                             |
| `cephadm/cluster.yaml`                | Service spec for mon/mgr/rgw/osd placement.                            |
| `ansible/hosts.example.ini`           | Ansible inventory template.                                            |
| `ansible/playbook.yml`                | Idempotent OS hardening + cephadm install across the inventory.        |
| `gateway_config.example.json`         | Snippet wiring `config.providers.ceph_rgw` into the gateway.           |
| `monitoring/prometheus.yml`           | Prometheus scrape config for ceph_exporter + RGW + the fabric gateway. |
| `monitoring/grafana_dashboard.json`   | Grafana dashboard: per-OSD utilization, RGW p99 latency, RADOS ops/s.  |

## Quick start (Reef demo, single node)

For the Phase 3 development / compliance loop the snapshot already
ships the `quay.io/ceph/demo:latest-reef` image. The
[`ceph_rgw` knowledge note](../../docs/PROGRESS.md) describes how
to start a single-node demo cluster on `:8888` for the
`TestSuite_CephRGW` compliance run; the production cluster below
uses cephadm against real hardware.

## Quick start (production cluster)

```bash
# On the bootstrap node
sudo ./cephadm/install.sh \
  --cluster-name zkof-beta-cell-01 \
  --public-network 10.10.0.0/24 \
  --cluster-network 10.20.0.0/24 \
  --rgw-realm zkof --rgw-zonegroup zkof --rgw-zone zkof-beta

# Add storage nodes
for host in osd-{01..06}; do
  sudo cephadm shell -- ceph orch host add $host
done

# Apply service spec
sudo cephadm shell -- ceph orch apply -i cephadm/cluster.yaml
```

After the cluster reaches `HEALTH_OK`, create the bucket the
gateway will use:

```bash
sudo cephadm shell -- radosgw-admin user create \
  --uid=zkof-gateway --display-name="ZK Object Fabric Gateway"

sudo cephadm shell -- radosgw-admin user info --uid=zkof-gateway \
  | jq '.keys[0]'   # → access_key + secret_key for the gateway

# create bucket via S3 API (or radosgw-admin bucket create)
aws --endpoint-url https://rgw.zkof-beta-cell-01.local:7480 \
    s3api create-bucket --bucket zkof-beta-cell-01-primary
```

## Wiring into the gateway

```json
{
  "providers": {
    "ceph_rgw": {
      "endpoint":   "https://rgw.zkof-beta-cell-01.local:7480",
      "region":     "local-dc-01",
      "bucket":     "zkof-beta-cell-01-primary",
      "access_key": "${CEPH_RGW_AK}",
      "secret_key": "${CEPH_RGW_SK}",
      "cell":       "beta-cell-01",
      "country":    "US"
    }
  }
}
```

Tenants whose placement policy targets this cell simply set
`provider: ceph_rgw` (and optionally pin `cell`, `country`).
The migration state machine drives the
`wasabi_primary → dual_write → local_primary_wasabi_backup →
local_primary_wasabi_drain → local_only` transition for any
existing tenant moving onto the cell — see
[`docs/runbooks/beta-onboarding.md`](../../docs/runbooks/beta-onboarding.md).

## Network requirements

- 25 Gbps per OSD node front network (client traffic).
- 25 Gbps per OSD node cluster network (replication / EC encode).
- 10 Gbps minimum for mon / mgr / RGW.
- 25–100 Gbps aggregate uplink to the gateway fleet (matches
  Phase 3 target).

## Monitoring

The shipped Prometheus config scrapes:

- Ceph mgr's built-in Prometheus exporter.
- RGW metrics (Prometheus exporter sidecar).
- Gateway `/internal/ready` and the fabric's metric endpoint
  (Phase 4).

Grafana dashboard ships with three rows: cluster health (mons,
PGs, recovery throughput), client traffic (RGW ops/s, p99
latency, S3 4xx/5xx rates), and storage (per-OSD utilization,
host-level reweighting alerts).

## Failure domains

- Beta cell tolerates 1 OSD-node loss with replication 3.
- EC 6+2 production layout tolerates 2 OSD-node losses per stripe.
- Mon quorum is 2 of 3.
- RGW is stateless; loss of any single instance is masked by the
  gateway fleet's retry path.

## See also

- [`deploy/cell-provisioner/`](../cell-provisioner/) — operator-side
  cell provisioning that drives `POST /api/tenants/{id}/dedicated-cells`.
- [`docs/runbooks/beta-onboarding.md`](../../docs/runbooks/beta-onboarding.md).

## Block-level deduplication (Phase 3.5 §3.14)

The Ceph RGW build of zk-object-fabric supports an *opt-in*
intra-tenant block-level dedup tier on top of the gateway's
object-level dedup. RADOS Pacific+ exposes a content-defined
chunking dedup pool that fingerprints chunks below the object
boundary; combined with the gateway's `level: "object+block"`
policy this lets a tenant share storage even when only the first
few MiB of two objects are identical (e.g. log-shipping,
scheduled-backup deltas, mostly-immutable archives).

### Enabling on a tenant

1. **Provision a dedicated cell** for the tenant. Object+block
   dedup requires per-tenant pool isolation: cross-tenant blocks
   are *never* shared. The `POST /api/tenants/{id}/dedicated-cells`
   endpoint creates the dedicated cell record; the operator then
   provisions the underlying Ceph pool layout via the cell
   provisioner.
2. **Create the dedup pool tier**. The recommended layout is a
   replicated `cache` tier in front of the object data pool with
   `dedup-tier` enabled and CDC chunking set to 64 KiB:
   ```
   ceph osd pool create tenant-{id}.rgw.buckets.data 256 256 replicated
   ceph osd pool create tenant-{id}.dedup-cdc 64 64 replicated
   ceph osd tier add tenant-{id}.rgw.buckets.data tenant-{id}.dedup-cdc
   ceph osd tier cache-mode tenant-{id}.dedup-cdc dedup
   ceph osd pool set tenant-{id}.dedup-cdc dedup_cdc_chunk_size 65536
   ceph osd pool set tenant-{id}.dedup-cdc dedup_cdc_algorithm fastcdc
   ```
3. **Mark the bucket** with `object+block` dedup via the console:
   ```
   curl -X POST -H 'Content-Type: application/json' \
        -d '{"enabled":true,"scope":"intra_tenant","level":"object+block"}' \
        $CONSOLE/api/v1/tenants/$TID/buckets/$BUCKET/dedup-policy
   ```
   The console rejects `object+block` unless the tenant has a
   dedicated cell *and* the bucket's placement resolves to a
   Ceph RGW backend (the `bucketResolvesToCephRGW` guardrail in
   `api/console/dedup_handler.go`).

### Per-tenant pool isolation

Cross-tenant block-level dedup is permanently excluded from the
product per docs/PROPOSAL.md §1.2. Every tenant runs with its
own data pool, dedup CDC pool, and CMK; the Ceph object IDs are
prefixed with the tenant ID so a misconfigured pool does not
silently start sharing chunks across tenants. Operators MUST
verify pool ownership annotations match the tenant before
attaching the dedup tier:
```
ceph osd pool ls detail | grep dedup-cdc | grep tenant-$TID
```

### Monitoring dedup ratio

The `ceph-mgr dashboard` exposes per-pool dedup ratios under
**Pools → tenant-{id}.dedup-cdc → Statistics**. The gateway's
ClickHouse billing pipeline (see `billing/`) emits the matching
counters per tenant:

| Dimension                      | Source                                        |
|--------------------------------|-----------------------------------------------|
| `dedup_hits`                   | `api/s3compat/dedup.go` (gateway PUT path)    |
| `dedup_bytes_saved`            | `api/s3compat/dedup.go` (gateway PUT path)    |
| `dedup_ref_count`              | `api/s3compat/handler.go` Delete path         |
| `ceph_dedup_chunk_pool_bytes`  | `ceph-mgr` Prometheus exporter (Phase 4)      |

Operators reconcile gateway-reported `dedup_bytes_saved` against
the Ceph-reported chunk pool size to surface configuration drift
(gateway thinks it deduped, Ceph didn't actually share the
block) within the daily ops review.

### Disabling

Disabling object+block dedup is a one-call flip:
```
curl -X DELETE $CONSOLE/api/v1/tenants/$TID/buckets/$BUCKET/dedup-policy
```
The gateway stops registering new content_index entries
immediately; existing entries are reference-counted down by the
DELETE path until they reach zero. The Ceph dedup tier may be
torn down once `dedup_ref_count == 0` for every (tenant_id,
content_hash) row that pointed at the pool.
