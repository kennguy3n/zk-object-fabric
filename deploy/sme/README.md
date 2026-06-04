# SME Single-Pool Deployment

Run the entire ZK Object Fabric gateway stack on **one VM with Wasabi as
the only external dependency**. This is the entry-point deployment for
small/medium operators serving up to ~5K pooled tenants who do not want
to operate AWS + Linode + a managed ClickHouse fleet.

One command brings up the whole pool:

```bash
docker compose -f deploy/sme/docker-compose.production.yml up -d
```

## What you get

| Component      | Role                                                       |
| -------------- | ---------------------------------------------------------- |
| **traefik**    | Edge proxy: auto-TLS (Let's Encrypt), HTTP→HTTPS redirect, rate limiting, blocks `/internal/*` from the public internet |
| **gateway** ×2 | The S3 data plane (`:8080`) + console API (`:8081`), two replicas behind Traefik for zero-downtime upgrades |
| **postgres** 16 | Control-plane metadata store (tenants, manifests, multipart, bucket config, audit) |
| **clickhouse** | Billing/usage sink                                         |
| **console-web** | nginx serving the React console SPA and proxying `/api` to the gateway |
| **bootstrap**  | One-shot: generates key material, applies every schema, arms Row-Level Security, creates the ClickHouse db |
| **frontend-init** | One-shot: copies the console bundle out of the gateway image for nginx |

Object bytes live in **Wasabi** (S3-compatible). The VM only holds
control-plane metadata, the hot cache, and billing rows.

> **No Redis.** The gateway has no Redis integration today; it uses an
> in-process rate limiter, and Traefik adds an edge rate-limit on top.
> Adding a Redis container would be dead weight, so it is intentionally
> omitted.

## Sizing

A 4 vCPU / 8 GB VM with a 100+ GB SSD comfortably runs this pool. Postgres
and ClickHouse are the memory consumers; the gateway replicas are light
because object bytes stream to Wasabi rather than landing on local disk.

## Zero to running

### 1. Provision a VM

Any Linode / Hetzner / DigitalOcean (or other) Ubuntu 22.04+ VM with a
public IPv4 works. Open inbound **80** and **443** (and your SSH port).
For data-at-rest encryption of the Postgres volume, put the Docker data
root (or a dedicated mount) on a **LUKS-encrypted** block device — the
compose file initialises Postgres with `--data-checksums` but the
at-rest guarantee comes from full-disk encryption you control on the
host.

### 2. Point DNS at the VM

Create A/AAAA records **before first boot** (Let's Encrypt validates over
HTTP-01):

| Record               | Points to     | Serves       |
| -------------------- | ------------- | ------------ |
| `example.com`        | VM public IP  | Console      |
| `console.example.com`| VM public IP  | Console (alias) |
| `s3.example.com`     | VM public IP  | S3 endpoint  |

### 3. Install Docker

```bash
curl -fsSL https://get.docker.com | sh
```

This installs Docker Engine and the Compose v2 plugin.

### 4. Clone the repo

```bash
git clone https://github.com/kennguy3n/zk-object-fabric.git
cd zk-object-fabric
```

### 5. Fill in `.env`

```bash
cp deploy/sme/.env.example deploy/sme/.env
# edit deploy/sme/.env
```

Generate the secrets with `openssl`:

```bash
openssl rand -hex 32   # ADMIN_TOKEN
openssl rand -hex 24   # POSTGRES_PASSWORD
openssl rand -hex 24   # ZKOF_APP_PASSWORD   (must differ from the above)
openssl rand -hex 24   # CLICKHOUSE_PASSWORD
```

Set `DOMAIN_NAME`, `LETSENCRYPT_EMAIL`, and the `WASABI_*` values for your
bucket. Pick exactly one CMK backend (`KMS_CMK_URI` for AWS KMS, or
`VAULT_ADDR`+`VAULT_TOKEN` for Vault Transit, or a `cmk://local/...` file
for a zero-dependency start). See the comments in `.env.example`.

### 6. Bring up the pool

```bash
docker compose -f deploy/sme/docker-compose.production.yml up -d
```

First boot builds the gateway image, runs the `bootstrap` job (schemas +
RLS + keys), and waits for the gateway replicas to pass readiness on
their internal listener. Watch progress:

```bash
docker compose -f deploy/sme/docker-compose.production.yml logs -f bootstrap gateway
```

### 7. Access the console

Open `https://example.com`. Authenticate to the console API with the
`ADMIN_TOKEN` you generated. The S3 endpoint is at `https://s3.example.com`.

Smoke-test the S3 endpoint with any S3 client (point it at
`https://s3.example.com`, path-style addressing):

```bash
aws --endpoint-url https://s3.example.com s3 ls
```

## How config is wired

The gateway reads a JSON config (`internal/config`), which does **not**
expand environment variables. So `gateway/entrypoint.sh` renders
`gateway/config.json.tmpl` with `envsubst` at container start — the same
pattern `demo/entrypoint.sh` uses for `tenants.json` — and execs the
gateway against the rendered file. Secrets stay in `.env` / mounted key
files and never land in a committed config.

The deployment runs with `env=production`, which turns on the gateway's
safety guards. The `bootstrap` job exists specifically to satisfy them:

- **Least-privilege DB role.** The gateway connects as `zkof_app`
  (`NOSUPERUSER NOBYPASSRLS`) so Postgres Row-Level Security actually
  takes effect. A superuser would silently bypass every policy.
- **Manifest body encryption.** The manifest store seals each manifest
  with XChaCha20-Poly1305 before INSERT, so the `manifests.body` column
  is `BYTEA` (JSONB rejects opaque ciphertext — see
  `metadata/manifest_store/postgres/store.go`). The 32-byte key is
  generated into the shared secrets volume.
- **Console JWT signing key.** An RSA key is generated once so all
  replicas mint/verify the same session tokens.

## Operations

### Backups

`backup.sh` dumps Postgres (via the running container) and uploads a
gzip to `s3://$WASABI_BUCKET/backups/postgres/`, pruning anything older
than `RETENTION_DAYS` (default 14):

```bash
./deploy/sme/backup.sh
```

Run it from cron for daily backups:

```cron
17 3 * * *  /path/to/zk-object-fabric/deploy/sme/backup.sh >> /var/log/zkof-backup.log 2>&1
```

Object data itself is already durable in Wasabi; this backs up the
control-plane metadata that ties objects to tenants.

### Zero-downtime upgrades

`upgrade.sh` builds/pulls the new image and rolls the replicas one at a
time: each replica is drained via `POST /internal/drain` (Traefik's
health check then pulls it from rotation while in-flight requests
finish), removed, and replaced on the new image — the surviving replica
serves traffic throughout.

```bash
./deploy/sme/upgrade.sh
# or pin an image:
ZKOF_IMAGE=ghcr.io/kennguy3n/zk-object-fabric:v1.2.3 ./deploy/sme/upgrade.sh
```

### Let's Encrypt staging (optional)

To validate DNS/firewall without burning the production rate limit, add
the staging CA server under the resolver in `traefik/traefik.yml`:

```yaml
    acme:
      caServer: https://acme-staging-v02.api.letsencrypt.org/directory
```

Delete the `traefik-acme` volume and restart once you switch back to
production so a trusted certificate is issued.

## Troubleshooting

- **Gateway restarts / never ready:** `docker compose ... logs gateway`.
  In `env=production` the gateway refuses to boot if a guard is unmet
  (missing DSN, non–least-privilege role, missing manifest key). The
  `bootstrap` job must complete first — check its logs.
- **TLS not issued:** confirm DNS resolves to the VM and 80/443 are open;
  Let's Encrypt HTTP-01 needs `:80` reachable. Check `docker compose ...
  logs traefik`.
- **Billing rows missing:** check the `bootstrap` log for the ClickHouse
  schema step and `docker compose ... logs clickhouse`.
