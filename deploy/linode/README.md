# Linode Gateway Fleet

Phase 3 stand-up for the production ZK Object Fabric gateway fleet
on Linode. The gateway is the S3-compatible data plane that fronts
the AWS control plane (RDS, KMS, ClickHouse) and the Wasabi origin.

## Deployment model

- **Compute**: Linode dedicated CPU instances with attached NVMe
  volumes. Sizing depends on cell capacity; the beta target is
  `g6-dedicated-8` (8 vCPU, 16 GB RAM, NVMe).
- **Regions**: us-east, us-west, eu-central. Add additional
  regions by replicating the Terraform module with a new
  `region` variable.
- **TLS termination**: Caddy (Caddyfile shipped here). Caddy's
  automatic HTTPS uses Let's Encrypt; a wildcard certificate per
  region is recommended in production.
- **Load balancing**: Linode NodeBalancer in each region, plus
  GeoDNS (or AWS Route 53 latency routing) across regions. The
  load balancer health-checks the gateway's
  `GET /internal/ready` endpoint.

## Layout

| Path                         | Purpose                                                                   |
| ---------------------------- | ------------------------------------------------------------------------- |
| `terraform/`                 | Linode + Cloud Manager Terraform module for one region.                   |
| `systemd/zk-gateway.service` | systemd unit running the gateway binary as a non-root user.               |
| `caddy/Caddyfile`            | Caddy reverse proxy config: TLS termination + HSTS + `/internal/*` block. |
| `scripts/install_gateway.sh` | One-shot installer that fetches the gateway tarball, installs, and starts.|
| `scripts/health_check.sh`    | Local health verifier called by the NodeBalancer health probe.            |

## Quick start (per region)

```bash
cd terraform
terraform init
terraform workspace new us-east
terraform apply -var-file=us-east.tfvars
```

After the Linode instances boot, the cloud-init script from
`scripts/install_gateway.sh` is automatically run; the gateway
comes up listening on `:8080` (S3 data plane) and `:8081`
(console API). Caddy fronts both with TLS.

## Health checks

The gateway exposes three internal endpoints (see
[`internal/health/health.go`](../../internal/health/health.go)):

| Endpoint            | Purpose                                                          |
| ------------------- | ---------------------------------------------------------------- |
| `GET /internal/health` | Liveness — does the process respond at all?                   |
| `GET /internal/ready`  | Readiness — is the cell in quorum and is this node serving? |
| `POST /internal/drain` | Drain — start refusing new requests, wait for in-flight to complete, then exit gracefully. |

The NodeBalancer should poll `/internal/ready` on a 5-second
interval with a 2-of-3 healthy threshold. The Caddyfile blocks
external clients from reaching `/internal/*` so only the
NodeBalancer (and the on-host operator) can hit it.

## Drain / replace procedure

1. `curl -X POST http://localhost:8081/internal/drain` (the
   gateway flips to NotReady so the NodeBalancer pulls it out of
   rotation, completes in-flight requests, then exits).
2. `systemctl stop zk-gateway` (idempotent — drain already
   exited).
3. Pull the new binary; restart `zk-gateway`.
4. Verify `/internal/ready` returns 200 before adding the node
   back.

A rolling fleet replacement is one node at a time so quorum is
preserved (a 3-node cell tolerates 1 node down; a 5-node cell
tolerates 2). The cell health monitor refuses writes if quorum
drops below `QuorumThreshold` (configured in
`config.health.quorum_threshold`).

## Multi-region NodeBalancer pattern

```
┌──────── GeoDNS / Route 53 latency routing ─────────┐
│                                                     │
▼                       ▼                       ▼
us-east NB         us-west NB           eu-central NB
│                       │                       │
└──── 3 gateways ───────┴──── 3 gateways ────────┘
        ↓                       ↓                       ↓
    AWS RDS (control plane)  ←─────────────  Wasabi region origin
```

The NodeBalancer terminates TLS to the gateway via internal
network so the gateway only needs an unprivileged port; Caddy
on the gateway is optional in this topology and only used when
the gateway is exposed directly (e.g., a sovereign cell that
does not use Linode NodeBalancer).

## Sizing guidance

| Tier         | Compute             | Cache (NVMe) | Cell quorum | Throughput target |
| ------------ | ------------------- | ------------ | ----------- | ----------------- |
| Beta         | 3× g6-dedicated-8   | 500 GB / node| 2 of 3      | 1.5 GB/s aggregate|
| Production   | 5× g6-dedicated-16  | 1 TB / node  | 3 of 5      | 5 GB/s aggregate  |
| High-egress  | 7× g6-dedicated-32  | 2 TB / node  | 4 of 7      | 12 GB/s aggregate |

NVMe sizing is dictated by the cache hit-ratio target and the
hot-set size; aim for `cache_path` to fit roughly the top 5% of
objects by recency.
