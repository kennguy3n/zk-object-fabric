# zk-object-fabric Helm chart

Deploys the zk-object-fabric S3-compatible gateway as a horizontally
auto-scaled, self-healing Kubernetes workload. Replaces the manual
Terraform + cloud-init + systemd fleet (`deploy/linode/`) with a
Deployment + HorizontalPodAutoscaler so operators no longer hand-size the
fleet.

Chart location: `deploy/helm/zk-object-fabric`.

## What it ships

| Resource | Purpose |
| --- | --- |
| `Deployment` | Gateway pods. Init container renders the config; main container runs the gateway with readiness/liveness probes and a `preStop` drain hook. |
| `HorizontalPodAutoscaler` | Scales 2–20 replicas at 70 % CPU (all configurable). |
| `Service` | ClusterIP (default) exposing the S3 port, plus the console port when enabled. |
| `ConfigMap` | Gateway `config.json` template rendered from values, with `${...}` placeholders for secret fields. |
| `Secret` | Wasabi keys, metadata DSN, Vault token, console admin token, and (when set inline) the manifest-body encryption key. |
| `PersistentVolumeClaim` | NVMe hot-object cache (only for `cache.mode=pvc`). |
| `PodDisruptionBudget` | Keeps `minAvailable: 1` during voluntary disruptions. |
| `ServiceAccount` | Dedicated SA (annotate for AWS IRSA → KMS). |
| `Ingress` | Optional, TLS-capable, gated by `ingress.enabled`. |

## How configuration works

The gateway reads a **single JSON config file** passed via `-config`
(`cmd/gateway/main.go`; `internal/config.Load`). `config.Load` does **not**
expand environment variables, so credentials cannot be injected as plain
env vars into the JSON.

This chart follows the repo's own production convention
(`deploy/wasabi/gateway_config.example.json` uses `${...}` placeholders;
`deploy/wasabi/README.md`: *"let the gateway init container pull them at
boot"*):

1. `templates/configmap.yaml` renders `config.json.tmpl` from values, with
   `${WASABI_ACCESS_KEY}`, `${WASABI_SECRET_KEY}`, `${METADATA_DSN}`,
   `${VAULT_TOKEN}`, `${CONSOLE_ADMIN_TOKEN}` placeholders for the secret
   fields.
2. The `render-config` **init container** loads those env vars from the
   Secret and runs `envsubst` (shipped in the Alpine runtime image via
   `gettext`, see the repo `Dockerfile`) to write the final
   `config.json` into a shared `emptyDir`.
3. The gateway container starts with
   `gateway -config /run/zk-object-fabric/config.json`.

This keeps secret values out of the ConfigMap while still producing the
single merged file the gateway needs. Only the `render-config` init container
mounts the Secret as env vars (it needs them for `envsubst`); the gateway
container has no `envFrom`, so the credentials are not exposed in its process
environment — it reads them from the rendered `config.json`.

Two consequences of the `envsubst` approach to be aware of:

- **Secret values must be JSON-string-safe.** They are substituted verbatim
  into a JSON document, so a literal `"`, `\`, or newline in a value (most
  likely a `METADATA_DSN` password) produces malformed `config.json` and the
  gateway crash-loops on parse. Keep DSN passwords URL-encoded (the libpq URI
  standard); alphanumeric Wasabi keys and UUID/JWT tokens are inherently safe.
- **Rotating credentials needs a pod roll.** The values are baked into
  `config.json` by the init container at start-up, so the pod template carries
  a `checksum/secret` annotation: when `secret.create: true`, a
  `helm upgrade` that changes a credential rolls the pods automatically. When
  credentials live in an externally-managed Secret (`secret.create: false` /
  `existingSecret`), Helm cannot hash its contents, so rotate with a reloader
  (e.g. [Stakater Reloader](https://github.com/stakater/Reloader)) or
  `kubectl rollout restart deploy/<release>-zk-object-fabric`.

## Manifest-body encryption key (required in production)

With `config.env: production` and a persistent metadata store (a Postgres
`metadataDsn` **or** the embedded SQLite store, which is on by default),
`cmd/gateway` refuses to boot unless `encryption.manifest_body_key_path` is
set — otherwise tenant manifest JSON would be stored as plaintext
(`enforceProductionManifestEncryption`). The gateway reads that key as a
**file** (`os.ReadFile`), so it cannot be injected through the `envsubst`
flow above; the chart mounts it from a Secret instead.

Provide a 32-byte XChaCha20-Poly1305 key via `config.encryption.manifestBodyKey`:

- `value` — base64 of the 32 raw bytes (`head -c 32 /dev/urandom | base64`).
  The chart stores it in its **own dedicated Secret**
  (`<release>-manifest-body-key`), separate from the gateway credentials
  Secret, and only volume-mounts it. Keeping it out of the credentials Secret
  (which is consumed via `envFrom`) avoids a per-pod Kubernetes warning about
  the non-identifier key name `manifest-body.key`, and decouples it from
  `secret.create` — so an inline `value` works even with `secret.create: false`.
- `existingSecret` / `existingSecretKey` — reference a key in a Secret you
  manage out-of-band (recommended for production).

When either is set, the chart mounts the key (read-only, mode `0400`) at
`manifestBodyKey.mountPath` and points `encryption.manifest_body_key_path`
at it automatically. The key is **durable**: rotating it makes previously
sealed manifests unreadable, so set it once and manage it out-of-band.

To surface the misconfiguration early, the chart **fails at `helm install`/
`helm template` time** (not as a pod crash-loop) when `env: production` is set
with a persistent store but no key. The default `config.env` is `development`
so a zero-config render stays valid; production is an explicit opt-in.

## Health, ports & drain (grounded in the code)

| Endpoint | Path | Source |
| --- | --- | --- |
| Liveness | `GET /internal/health` | `internal/health/health.go` `ServeMux` |
| Readiness | `GET /internal/ready` (503 unless `StateReady`) | `internal/health/health.go` `handleReady` |
| Drain | `POST /internal/drain` | `internal/health/health.go` `handleDrain` |

> The liveness path is `/internal/health`, **not** `/internal/healthz`. The
> chart uses the path the code actually mounts
> (`internal/health/health.go` `ServeMux`).

| Port | Service | Default |
| --- | --- | --- |
| S3 data plane | `gateway.listen_addr` | `:8080` |
| Console / admin | `console.listen_addr` | `:8081` (opt-in) |
| Internal health/ready/drain | `health.listen_addr` | `:29090` |
| Prometheus metrics | `metrics.path` on the S3 port | `/internal/metrics` |

The `preStop` hook POSTs to `/internal/drain` (via `wget --post-data`, since
HTTP probes can only issue GET) to flip the pod `NotReady` and let the
Service drain in-flight requests before SIGTERM. The hook's wget timeout is
`terminationDrainTimeoutSeconds` (default 35), a plain integer kept separate
from the Go-duration `config.health.drainTimeout` (default 30s) so it never
has to parse formats like `1m`/`2m30s`. Keep the ordering
`config.health.drainTimeout ≤ terminationDrainTimeoutSeconds <
terminationGracePeriodSeconds` (default 45s) so wget waits for the
server-side drain to finish and the kubelet does not kill the pod mid-drain.

## The hot-object cache and autoscaling

The hot-object cache (`gateway.cache_path`) is per-node NVMe storage. A
single `ReadWriteOnce` PVC shared by a Deployment would pin every replica to
one node and break HPA scale-out, so the chart defaults to a **per-pod
generic ephemeral volume** (`cache.mode=ephemeral`): each replica gets its
own NVMe-backed PVC bound to the pod's lifetime. A cache is disposable, so
losing it on reschedule is harmless — the gateway cold-starts.

`cache.mode`:

- `ephemeral` (default) — per-pod ephemeral PVC. Correct for autoscaling.
- `pvc` — one shared `PersistentVolumeClaim`. Only safe for a single replica
  or a `ReadWriteMany` storage class.
- `emptyDir` — throwaway node storage.

The cache volume is mounted at `cache.dataDir` (default
`/var/lib/zk-object-fabric`), the gateway's single writable directory when
`securityContext.readOnlyRootFilesystem: true` (the default). The hot-object
cache (`cache.path`) lives under it, as do the single-node embedded SQLite DB
(`config.controlPlane.embeddedDbPath`) and the `local_fs_dev` backend
(`config.providers.localFsDev.rootPath`) — so those dev/single-node writes
succeed against a read-only root filesystem. The embedded DB only persists
across reschedule when `cache.mode=pvc`.

## Install

```bash
helm install zk-fabric ./deploy/helm/zk-object-fabric -f my-values.yaml
```

Minimal `my-values.yaml` for a Wasabi-backed production deploy:

```yaml
image:
  repository: ghcr.io/kennguy3n/zk-object-fabric
  tag: "0.1.0"

config:
  env: production
  encryption:
    cmkUri: "cmk://aws-kms/arn:aws:kms:us-east-1:123456789012:key/abcd-…"
    kmsRegion: us-east-1
    # Required in production with a persistent metadata store. Reference a
    # Secret holding the raw 32-byte key (recommended), or set value: <base64>.
    manifestBodyKey:
      existingSecret: zk-fabric-manifest-key
      existingSecretKey: manifest-body.key
  providers:
    wasabi:
      enabled: true
      endpoint: https://s3.us-east-1.wasabisys.com
      region: us-east-1
      bucket: zkof-us-east-1-prod

cache:
  mode: ephemeral
  ephemeral:
    storageClass: nvme       # an NVMe-backed StorageClass
    size: 100Gi

autoscaling:
  minReplicas: 3
  maxReplicas: 30
  targetCPUUtilizationPercentage: 70

secret:
  # Prefer an externally managed Secret (Vault / SOPS / External Secrets):
  create: false
  existingSecret: zk-fabric-credentials
  # …or, for a quick trial, set create: true and pass keys via --set:
  #   --set secret.wasabiAccessKey=… --set secret.wasabiSecretKey=…
  #   --set secret.metadataDsn='postgres://…'
```

With `secret.create: false` you **must** set `secret.existingSecret`; the chart
fails at `helm install`/`helm template` time otherwise (without it the init
container would reference a Secret that is never created and the pod would fail
with `CreateContainerConfigError`).

The credentials `existingSecret` must expose these keys: `WASABI_ACCESS_KEY`,
`WASABI_SECRET_KEY`, `METADATA_DSN`, `VAULT_TOKEN`, `CONSOLE_ADMIN_TOKEN`. The
manifest-body key lives in its own Secret (`manifestBodyKey.existingSecret`)
because it is mounted as a file, not an env var; create it with e.g.
`kubectl create secret generic zk-fabric-manifest-key
--from-file=manifest-body.key=<(head -c 32 /dev/urandom)`.

### Dev / no-credentials trial

```yaml
config:
  env: development
  providers:
    wasabi:
      enabled: false
    localFsDev:
      enabled: true
cache:
  mode: emptyDir
secret:
  create: true   # empty placeholders are fine in development
```

This works with the default `readOnlyRootFilesystem: true`: the embedded
SQLite DB and `local_fs_dev` root both sit under `cache.dataDir`, which is
the (writable) `emptyDir` mount here.

## Validate before installing

```bash
helm lint ./deploy/helm/zk-object-fabric
helm template zk-fabric ./deploy/helm/zk-object-fabric -f my-values.yaml
```

## Upgrade / uninstall

```bash
helm upgrade zk-fabric ./deploy/helm/zk-object-fabric -f my-values.yaml
helm uninstall zk-fabric
```

When `autoscaling.enabled=true` the Deployment intentionally omits
`replicas` so the HPA is the sole authority and a `helm upgrade` does not
fight it.
