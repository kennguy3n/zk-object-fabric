# ZK Object Fabric — Docker Demo

A one-command, zero-external-dependency demo of the ZK Object Fabric
gateway. It boots the S3-compatible data plane on `:8080` and the
tenant console API on `:8081`, backed by the `local_fs_dev`
filesystem provider and an in-memory manifest / tenant store.

This is **development-only**: manifest and tenant bindings live in
memory and are lost on container restart, though object bodies
persist in the `zk-data` Docker volume mounted at `/data/objects`.
It is the same S3 API surface Phase 1 (Wasabi), Phase 2 (Ceph RGW),
and Phase 3 (owned DC) serve — downstream apps can point at it now
and keep the same bucket name, object key, and URL when the
backend is swapped out under the gateway.

## Quick start

```bash
docker compose up --build
```

| Port    | Service           |
| ------- | ----------------- |
| `:8080` | S3-compatible API |
| `:8081` | Console API       |

Pre-loaded demo credentials (see `demo/tenants.json`):

| Tenant        | Access Key         | Secret Key         | Tenant ID            |
| ------------- | ------------------ | ------------------ | -------------------- |
| Demo Tenant   | `demo-access-key`  | `demo-secret-key`  | `demo-tenant-001`    |
| kmail-dev     | `kmail-access-key` | `kmail-secret-key` | `kmail-tenant-001`   |

## Try it with the AWS CLI

```bash
aws --endpoint-url http://localhost:8080 \
    --region us-east-1 \
    s3 mb s3://mybucket

echo "hello" > hello.txt
aws --endpoint-url http://localhost:8080 \
    --region us-east-1 \
    s3 cp hello.txt s3://mybucket/

aws --endpoint-url http://localhost:8080 \
    --region us-east-1 \
    s3 ls s3://mybucket/
```

Configure the client with the demo credentials either inline
(`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` env vars) or via
`aws configure`.

## Downstream integration examples

### kmail (Stalwart blob store)

kmail's local `docker-compose.yml` wires Stalwart's
[S3 blob store](https://stalw.art/docs/storage/s3) at
`http://zk-fabric:8080` when the two compose stacks share a
network. The Stalwart config points at the `kmail-dev` tenant:

```toml
[store."blob"]
type       = "s3"
endpoint   = "http://zk-fabric:8080"
region     = "us-east-1"
bucket     = "kmail-blobs"
access-key = "kmail-access-key"
secret-key = "kmail-secret-key"
path-style = true
```

The kmail compose file publishes the gateway to host ports
`9080` / `9081` (avoiding collision with the kmail BFF on `8080`)
and runs a one-shot init container that creates the
`kmail-blobs` bucket before Stalwart starts.

### zk-drive (direct S3 / presigned URLs)

zk-drive uses the same S3 endpoint for presigned GET / PUT URLs
against a per-workspace bucket. The demo credentials work for
local development; production deploys mint per-tenant keys via
the console API (`POST /api/tenants/{id}/keys`).

## Configuration

Everything the gateway needs lives in two files that the
container mounts under `/app/demo`:

- `demo/config.json` — gateway listener addresses, provider
  selection (`local_fs_dev` rooted at `/data/objects`), and
  billing sink (logger stdout). See
  [`internal/config/config.go`](../internal/config/config.go)'s
  `Config` struct and `config.Default()` for the full schema.
- `demo/tenants.json` — pre-loaded HMAC credentials bound to
  tenant records. See
  [`internal/auth/tenant_store.go`](../internal/auth/tenant_store.go)'s
  `LoadBindingsFromJSON` for the expected shape.

To add tenants, append to `demo/tenants.json` and rebuild the
image. Each binding is a `{ access_key, secret_key, tenant }`
tuple; the tenant sub-document mirrors
[`metadata/tenant/tenant.go`](../metadata/tenant/tenant.go).

## Limits

- **In-memory state**: tenant bindings and object manifests
  live in process memory. Restarting the container wipes them.
  Rebind credentials by editing `demo/tenants.json` and
  restarting.
- **No TLS**: the demo serves plain HTTP. Production deploys
  terminate TLS at a reverse proxy or load balancer in front
  of the gateway.
- **No erasure coding / caching**: `local_fs_dev` writes
  one-piece objects straight to disk. Production runs
  Reed-Solomon erasure coding on top of cloud / Ceph / DC
  providers; see `metadata/erasure_coding/`.
- **No billing**: the demo uses `billing.LoggerSink`, which
  prints every usage event to stdout. ClickHouse wiring
  (`billing.ClickHouseSink`) is optional and configured via
  `config.billing.clickhouse_url`.
