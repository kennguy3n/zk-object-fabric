# ZK Object Fabric — Progress

- **Project**: ZK Object Fabric
- **License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).
- **Status**: Phase 3 — Beta Cell (in progress)
- **Last updated**: 2026-04-25 (Console listKeys reliability: the `bindingLister` interface in `cmd/gateway/main.go` and `ListBindingsByTenantID` on both `internal/auth/postgres_tenant_store.go` and `internal/auth/tenant_store.go` (MemoryTenantStore) now return `([]TenantBinding, error)`; `consoleTenantAdapter.ListAPIKeys` propagates the error so the `GET /api/tenants/{id}/keys` handler in `api/console/handler.go` returns HTTP 500 on a Postgres outage instead of masking it as an empty 200 OK — closing the open review finding from PR #21. PR #26 (Kapp Business Suite integration docs on `devin/1777098201-kapp-integration-docs`) and PR #24 (S3 SigV4 query-string presigned URL auth on `devin/1776984915-presigned-url-auth`, enabling zk-drive ↔ zk-object-fabric integration) landed alongside the encryption wiring summarized below. Encryption wiring landed on `devin/1777080229-encryption-wiring`: the client SDK's XChaCha20-Poly1305 construction is now applied on every S3 write path — single-piece PUT in `api/s3compat/handler.go`, erasure-coded PUT in `api/s3compat/erasure_coding.go` (shards are encoded over ciphertext so partial-shard recovery leaks nothing), and multipart PUT in `api/s3compat/multipart_handler.go` (one session-level DEK generated at `CreateMultipartUpload` and reused by every `UploadPart`, so concatenated parts still frame-decrypt on GET) — with matching decrypt on every read path. `managed` / `public_distribution` tenant policies seal a fresh DEK with the gateway-configured CMK via `client_sdk.LocalFileWrapper`, record the wrapped DEK + algorithm on `metadata.EncryptionConfig`, and keep plaintext bytes out of every backend piece; `client_side` (Strict ZK) refuses PUTs lacking `X-Amz-Meta-Zk-Encryption` and streams ciphertext verbatim on GET. `cmd/gateway/main.go#buildGatewayEncryption` constructs the wiring from `config.encryption.cmk_path` / `cmk_uri`; the Postgres manifest store grew an optional `BodyEncryptor` (`metadata/manifest_store/postgres/body_encryptor.go`) that seals the manifest JSON with a separate gateway-held key when `config.encryption.manifest_body_key_path` is set. End-to-end coverage lives in `tests/s3_compat/encryption_test.go` (managed round-trip, wrong-CMK fail-closed, Strict ZK reject + passthrough, object-key opacity, `Encryption.Mode` always populated across managed / public_distribution / client_side / legacy, erasure-coded and multipart managed encryption, cross-size backend inspection for plaintext leaks, legacy / no-policy backward compatibility, and the manifest-body AEAD construction). Phase 3 deliverable added to the checklist for the KMS / Vault wrapper that replaces `LocalFileWrapper` in production. Previous landings: Console & auth hardening on `devin/1776919356-console-auth-hardening`: (1) Admin-token bearer check on the console API is now documented and end-to-end wired via `cmd/gateway/main.go#buildAdminAuth` + `api/console/handler.go#Config.AdminAuth`; auth routes (`/api/v1/auth/signup`, `/api/v1/auth/login`, `/api/v1/auth/verify`) bypass `AdminAuth` because they live on the `AuthHandler.Register` mux routes, not the tenant-scoped `dispatch()`. (2) Frontend `ApiClient` route reconciliation — `frontend/src/api/client.ts` now pins a `tenantBaseUrl` via a new `setTenantScope(tenantId)` method so every tenant-scoped call resolves to `/api/tenants/{id}/usage|buckets|keys|placement|dedicated-cells`, matching the backend mux registered in `api/console/handler.go`. `frontend/src/auth/AuthContext.tsx` seeds the scope from the login/signup response so a page refresh and a fresh login both produce the same shape. (3) Backend routes added in `api/console/handler.go`: `GET/POST /api/tenants/{id}/buckets` and `DELETE /api/tenants/{id}/buckets/{name}` backed by a new `BucketStore` interface with a process-local `MemoryBucketStore` in `api/console/memory_resources.go`; `GET /api/tenants/{id}/keys` and `DELETE /api/tenants/{id}/keys/{accessKey}` backed by a new `APIKeyLister` interface that `consoleTenantAdapter` in `cmd/gateway/main.go` implements by type-asserting a new `bindingLister` interface against `MemoryTenantStore` / `PostgresTenantStore` (each grew `ListBindingsByTenantID` + `RemoveBinding` methods); `GET /api/tenants/{id}/dedicated-cells` backed by `DedicatedCellStore` + `MemoryDedicatedCellStore`. `parsePath` was generalized to 3-segment routes so `keys/{accessKey}` and `buckets/{name}` dispatch cleanly. (4) Postgres-backed `PlacementStore` remains wired via `cmd/gateway/main.go#buildPlacementStore`, which now stands alongside the Postgres tenant store documented in earlier entries. (5) CAPTCHA + billing on signup — `internal/config/config.go#ConsoleConfig` grew `CaptchaProvider` + `CaptchaSecret` fields; `cmd/gateway/main.go#buildAuthHooks` now prefers config-driven `hcaptcha` wiring and falls back to `HCAPTCHA_SECRET` env, warning when an unknown `captcha_provider` is set. `frontend/src/pages/SignupPage.tsx` mounts an hCaptcha widget only when `VITE_HCAPTCHA_SITEKEY` is set, so dev and self-hosted builds without a CAPTCHA license still work. `billing/metering.go` added a `TenantCreated` dimension; `api/console/auth_handler.go#AuthConfig` grew a `BillingSink` field and the signup handler emits `billing.UsageEvent{Dimension: TenantCreated, Delta: 1}` after a successful commit so the ClickHouse pipeline starts tracking a tenant from creation time rather than first S3 request. `cmd/gateway/main.go#startConsoleAPI` passes the gateway's billing sink through to `console.Config.BillingSink`. (6) CI — `.github/workflows/e2e.yml` boots the gateway with the `local_fs_dev` backend on `:8080`/`:8081` and runs the Playwright console suite with `CONSOLE_E2E=1`; `.github/workflows/storj-compliance.yml` runs `TestSuite_Storj` nightly + on `workflow_dispatch`, gated on `STORJ_ACCESS_GRANT` + `STORJ_BUCKET` secrets. (7) Docs — `docs/STORAGE_INFRA.md` gained a `storj` row on the provider-adapter matrix, a paragraph noting Storj does not embed `s3_generic.Provider`, and a BYOC mention. Previously, 2026-04-23 (Phase 3 wiring landed on `devin/1776911932-zk-fabric-phase3-wiring`: (1) Storj adapter wired end-to-end — `storj.io/uplink v1.14.0` added to `go.mod`; `internal/config/config.go` grew a `StorjConfig {AccessGrant, Bucket, SatelliteAddress}` nested into `ProvidersConfig`; `providers/storj/uplink_bridge.go` adapts `*uplink.Project` to the narrow `UplinkProject` interface; `cmd/gateway/main.go#buildProviderRegistry` now opens a live Storj project when `cfg.Providers.Storj.AccessGrant != ""` and registers it as `registry["storj"]`, with `pickDefaultBackend` updated to include Storj in its preference order; and `TestSuite_Storj` in `tests/s3_compat/suite_test.go` mirrors the `TestSuite_CephRGW` pattern, gated on `STORJ_ACCESS_GRANT` + `STORJ_BUCKET`. (2) Postgres-backed control-plane stores — `internal/auth/postgres_tenant_store.go` implements the full `TenantStore` interface (`LookupByAccessKey`, `CreateTenant`, `DeleteTenant`, `AddBinding`, `LookupByTenantID`, `Size`) against a `(tenants, tenant_bindings)` schema defined in `internal/auth/schema.sql`; `api/console/postgres_placement.go` implements `PlacementStore` against a `placement_policies` table defined in `api/console/schema.sql` using a single-row-per-tenant UPSERT; `cmd/gateway/main.go#buildTenantStore` and `buildPlacementStore` now switch to the Postgres implementations when `cfg.ControlPlane.MetadataDSN` is set and fall back to the in-memory stores for dev. To make this interface-polymorphic, `internal/auth/rate_limit.go#TenantBudgetsLookup` and `internal/auth/abuse.go#TenantLookupFromStore` were retyped from `*MemoryTenantStore` to `TenantStore`. (3) Admin authenticator on the console API — `api/console/handler.go#Config` grew an `AdminAuth func(*http.Request) bool` field and `Handler.dispatch` returns 401 `admin authorization required` when the predicate fails; `internal/config/config.go#ConsoleConfig` grew an `AdminToken string`; `cmd/gateway/main.go#buildAdminAuth` wires a constant-time Bearer-token comparison (via `crypto/subtle.ConstantTimeCompare`) sourced from `cfg.Console.AdminToken`, logging a dev-only warning when the token is unset. (4) B2C production hooks — `api/console/hcaptcha.go` ships `NewHCaptchaVerifier` that POSTs to the hCaptcha siteverify API via stdlib `net/http`; `api/console/email_verification.go` ships `NewSESEmailSender` backed by stdlib `net/smtp` with AWS SES SMTP credentials sourced from env (`AWS_SES_SMTP_HOST` etc.) and builds an RFC 5322 verification-link email; `cmd/gateway/main.go#buildAuthHooks` wires both into `AuthHooks`. `api/console/auth_handler.go` grew `IsVerified` / `MarkVerified` on `AuthStore` + `MemoryAuthStore`, a new `POST /api/v1/auth/verify` handler, an `AuthHooks.ResolveOAuth` hook with proper rollback semantics when an OAuth token fails to resolve, and a `verified` flag on the stored auth row. `api/s3compat/handler.go#Config` grew a `VerifiedCheck func(tenantID string) (verified, tracked bool)` that gates the first `PUT` with a 403 `EmailNotVerified` when the tenant is tracked but not yet verified; untracked tenants (JSON-loaded) bypass the gate. (5) Frontend route remapping — `frontend/src/api/client.ts` base URL moved from `/api/v1` to `/api` for tenant-scoped routes, with auth endpoints preserving `/api/v1/auth/` via a dedicated `authBaseUrl` and `requestAt` helper; `frontend/tests/e2e/signup.spec.ts` added covering signup → token → dashboard; `frontend/tests/e2e/placement.spec.ts` updated to match the new `/api/placement-policies/` request prefix; `frontend/playwright.config.ts` documented the `/api/*` proxy wiring. (6) Task 6 — live B2/R2/AWS S3 compliance runs — documented as pending; skip gates for `B2_ENDPOINT`, `R2_BUCKET`, `AWS_S3_BUCKET` remain in `tests/s3_compat/suite_test.go` and the full suite is green against `local_fs_dev` and can be run by operators once throwaway credentials are provisioned. Previously, on 2026-04-22: All Devin Review findings across PRs #8–#17 were resolved and merged: the two PR #11 findings — `cache/hot_object_cache/disk_cache.go#Get` corruption-recovery racing a concurrent `Put()` and `cmd/gateway/main.go#buildHotObjectCache` exiting instead of falling back to `NewMemoryCache` when `NewDiskCache` fails — were fixed in PR #15 (commit `32bce5d4`); the PR #17 findings — CDN-header pattern (`Cf-Connecting-Ip` / `Cf-Ray` / `X-Amz-Cf-Id` / `Fastly-Client-Ip` / `X-Forwarded-For`), `internal/config/config.go` `CachePath` default, `api/console/handler.go#putPlacement` 64 KiB body cap, the double `countingWriter` wrap on the erasure-coded GET path, and `cmd/gateway/main.go#consoleTenantAdapter.AddAPIKey` silently overwriting a colliding access key — were fixed in commit `55c2725e` on PR #17. Phase 3 follow-ups landed: (1) `api/s3compat/erasure_coding.go#getMultipart` now pre-fetches every piece body into memory and only then writes HTTP 200 + `Content-Length`, matching `getErasureCoded`'s pattern — a mid-assembly `GetPiece` failure now surfaces as a clean 502 `BackendGetFailed` instead of truncating the wire response. (2) `internal/auth/abuse.go` scaffolds a per-tenant abuse guard that layers egress-budget enforcement from `tenant.Budgets.EgressTBMonth`, a 2x-of-rolling-average egress-rate anomaly detector, and a CDN-shielding gate (`tenant.Abuse.CDNShielding == "enabled"` requires `Cf-Connecting-Ip` / `Cf-Ray` / `X-Amz-Cf-Id` / `Fastly-Client-Ip` / `X-Forwarded-For`). Wired into the middleware chain in `cmd/gateway/main.go` in front of `s3compat.Handler`; the existing `internal/auth/rate_limit.go` budget/anomaly counters stay on for RPS enforcement and the anomaly detector they already cover. Coverage in `internal/auth/abuse_test.go`. (3) `api/console/` scaffolds the tenant-console backend API — `GET /api/tenants/{id}`, `GET /api/tenants/{id}/usage`, `POST /api/tenants/{id}/keys`, and `GET` / `PUT /api/tenants/{id}/placement` — on its own mux, bound to `:8081` by default (separate from the S3 data plane on `:8080`) via `cfg.Console.ListenAddr`. The ClickHouse billing sink is detected via type-assertion and satisfies `console.UsageQuery` when available; otherwise a no-op usage stub ships. `api/console/memory_placement.go` provides the Phase 3 in-memory `PlacementStore`. Coverage in `api/console/handler_test.go`. Earlier 2026-04-22 entry: Phase 3 abuse-throttling + BYOC adapters + tenant console landed: (1) `internal/auth/rate_limit.go` now enforces the per-tenant `budgets.egress_tb_month` ceiling alongside the existing RPS limiter, and layers a sliding-window anomaly detector (EWMA baseline, configurable alert multiplier, optional throttle-on-anomaly) that emits `AbuseBudgetExhausted` and `AbuseAnomalyAlert` events via the billing sink. Coverage in `internal/auth/rate_limit_test.go` across budget-exhaustion, baseline-convergence, anomaly-alert, and throttle-cooldown scenarios. (2) BYOC cloud provider adapters fleshed out: `providers/aws_s3/s3.go`, `providers/backblaze_b2/b2.go`, and `providers/cloudflare_r2/r2.go` now carry production-ready Config validation, `NewWithClient` test seams, and provider-accurate `CostModel` + `PlacementLabels` values. All three embed `*s3_generic.Provider` so PUT/GET/HEAD/DELETE/LIST are inherited from the shared SigV4 + AWS SDK v2 implementation; Cloudflare R2 derives its account-scoped endpoint from `AccountID` and defaults to path-style addressing. Per-adapter unit tests in `providers/{aws_s3,backblaze_b2,cloudflare_r2}/*_test.go`. (3) Tenant console scaffolded under `frontend/` (Vite + React + TypeScript). Ships login / signup, dashboard (storage / request / egress stats from `/api/v1/usage`), bucket management, API-key management (access key + one-time secret reveal on create), placement-policy YAML editor with a structured summary, and a dedicated-cells page gated on `contract_type ∈ {b2b_dedicated, sovereign}`. Talks to the gateway's `/api/v1/` management API exclusively (separate from the S3-compat routes). (4) Docs: `internal/config/config.go` now defaults `CachePath` to `""` so developer and test environments get the in-memory cache without the DiskCache-fallback warning; operators set `config.gateway.cache_path` (or `ZKOF_GATEWAY_CACHE_PATH`) to enable NVMe-backed caching in production. Status banners in `README.md` and `docs/PROPOSAL.md` now correctly report Phase 3. Earlier 2026-04-22 entry: Phase 3 PR #11 + #12 review-finding fixes landed: (1) `cache/hot_object_cache/disk_cache.go#Get` now captures the index `*list.Element` before the first unlock and, in the corruption-recovery branch, only evicts the entry when the index still points at the same element — a concurrent `Put()` that replaced the entry between the unlock and re-lock is no longer erased by the recovering Get. Regression coverage in `cache/hot_object_cache/disk_cache_test.go#TestDiskCache_ConcurrentPutDuringCorruptGet`. (2) `api/s3compat/erasure_coding.go#getMultipart` now logs the tenant / bucket / key / part / piece ID and the bytes already delivered when a mid-stream `GetPiece` failure truncates the response, with a code comment documenting that this is best-effort because the HTTP headers are already committed. Billing emissions for `GetRequests` and `EgressBytes` on the `written` counter were already in place and are preserved. (3) `cmd/gateway/main.go#buildHotObjectCache` now degrades gracefully: if `cfg.Gateway.CachePath` is set but `NewDiskCache` fails (bad volume, permission error, corrupt warm-up) the gateway logs a warning and falls back to `NewMemoryCache` instead of exiting, so a single bad NVMe disk does not take the node offline. Earlier 2026-04-22 entry: Phase 3 Ceph RGW compliance landed: the full `TestSuite_CephRGW` subtest matrix — PUT / GET / HEAD / DELETE, ranged GET (prefix / middle / tail), LIST prefix, idempotent DELETE, missing-key 404, presigned GET, multipart-like overwrite, multipart round-trip, multipart abort, and 6+2 erasure-coded round-trip — all pass against a live Ceph Reef RGW at `http://127.0.0.1:8888` (`zkof-ceph-compliance` bucket). To get the AWS SDK v2 client to talk to a non-AWS S3 endpoint with a non-seekable `io.Reader` piece body, `providers/s3_generic/generic.go#PutPiece` now per-call swaps in `v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware` (UNSIGNED-PAYLOAD signing); this keeps the SigV4 envelope intact without forcing the body to be seekable, which `handler.go` and `multipart_handler.go` cannot guarantee for request-sourced streams. Backend integrity is still verified by ETag on the gateway side. Phase 3 foundations (`DiskCache`, `ClickHouseSink`, health monitor, BYOC adapter entrypoints) and Phase 3 multipart + EC remain landed from PRs 1 + 2. PR #19 review findings are all resolved and Storj is wired into the gateway via `cmd/gateway/main.go#buildProviderRegistry` under `registry["storj"]` when `config.providers.storj.access_grant` is set.)

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

**Status**: `COMPLETE`

**Goal**: a single-cell prototype that can PUT, GET, HEAD, DELETE,
LIST, and range-read encrypted objects end-to-end, backed by Wasabi
via the Linode gateway, with the migration engine wired up for a
dry-run cut-over to a local DC cell.

Checklist:

- [x] S3-compatible gateway on Linode (Go) — `cmd/gateway/main.go`
      now wires a full `s3compat.Config`: Postgres-backed manifest
      store (or in-memory fallback), `wasabi` + `local_fs_dev`
      providers, the placement engine, the HMAC authenticator, the
      in-memory hot object cache, and the logger billing sink.
      Request routing in `api/s3compat/handler.go` covers PUT, GET,
      HEAD, DELETE, LIST, range GET, and presigned URLs, with the
      hot cache consulted on the GET path.
- [x] Client-side encryption SDK — `encryption/client_sdk/sdk.go`
      implements chunked XChaCha20-Poly1305 encrypt/decrypt (16 MiB
      chunks so range reads can decrypt a single chunk); DEK
      generation in `keygen.go`; CMK-agnostic wrap / unwrap in
      `wrap.go`; round-trip + wrong-key coverage in `sdk_test.go`.
- [x] Gateway-side encryption wiring — `api/s3compat/handler.go`,
      `api/s3compat/erasure_coding.go`, and
      `api/s3compat/multipart_handler.go` now apply the SDK on
      every write path (single-piece PUT, erasure-coded PUT, and
      multipart `UploadPart`) and mirror the decrypt on every read
      path. `managed` and `public_distribution` tenant policies
      generate a fresh DEK per object (per-session for multipart),
      seal it with the gateway-configured CMK via
      `client_sdk.LocalFileWrapper`, record the wrapped DEK on the
      manifest (`metadata.EncryptionConfig.WrappedDEK` +
      `WrapAlgorithm`), and keep plaintext bytes out of every
      backend piece. `client_side` (Strict ZK) refuses PUTs
      lacking the `X-Amz-Meta-Zk-Encryption` header and streams
      ciphertext through untouched. The new `GatewayEncryption`
      struct is constructed in `cmd/gateway/main.go` from
      `config.encryption.cmk_path` / `cmk_uri`; the Postgres
      manifest store grew an optional `BodyEncryptor`
      (`metadata/manifest_store/postgres/body_encryptor.go`) that
      seals the manifest JSON with a separate gateway-held key
      when `config.encryption.manifest_body_key_path` is set. End-
      to-end coverage lives in
      `tests/s3_compat/encryption_test.go` (managed round-trip,
      wrong-CMK fail-closed, Strict ZK reject + passthrough,
      object-key opacity, Encryption.Mode always populated,
      erasure-coded and multipart managed encryption,
      cross-size backend inspection for plaintext leaks, legacy /
      no-policy backward compatibility, and the manifest body
      AEAD construction).
- [x] Encrypted manifest storage in the AWS control plane —
      Postgres-backed `ManifestStore` implementation in
      `metadata/manifest_store/postgres/store.go` (opaque JSONB
      bodies, index on `(tenant_id, bucket, object_key_hash,
      version_id)`), wired into `cmd/gateway/main.go` behind the
      `postgres` build tag; in-memory store used for dev + tests.
- [x] Storage provider adapter framework (`wasabi`, `local_fs_dev`,
      stubs for `backblaze_b2`, `cloudflare_r2`, `aws_s3`) — `wasabi`
      wired on AWS SDK v2; `ceph_rgw`, `backblaze_b2`,
      `cloudflare_r2`, and `aws_s3` adapters scaffolded with Config,
      constructor, Capabilities, CostModel, and PlacementLabels.
- [x] Placement engine (provider + region + country + storage_class)
      — `metadata/placement_policy/engine.go` filters eligible
      providers by policy constraints and picks the cheapest using
      `StorageProvider.CostModel()`. Coverage in `engine_test.go`
      across B2C pooled, B2B dedicated, and BYOC tenant paths.
- [x] Wasabi durable origin wired up as the primary backend —
      `cmd/gateway/main.go` registers `wasabi` in the provider map
      and sets it as the placement-engine default when no
      tenant-specific policy overrides it.
- [x] Linode hot cache (L0 / L1) with promotion rules —
      `cache/hot_object_cache/memory_cache.go` implements an LRU
      with hot-pin region, size/byte accounting, and stats; the
      promotion worker in `promotion_worker.go` consumes signals off
      the handler's non-blocking `SignalBus` and populates the cache
      against the configured `PromotionPolicy`.
- [~] Node health monitor for the Linode gateway fleet — **deferred
      to Phase 3**. Phase 2 relies on the existing liveness endpoint
      plus external process supervision; a purpose-built health
      monitor (per-cell quorum, cache-tier drain, graceful gateway
      replacement) is tracked as a Phase 3 deliverable alongside the
      production Linode fleet stand-up.
- [x] Basic billing counters (per-tenant storage-seconds, PUTs,
      GETs, egress bytes) — `billing/logger_sink.go` is a
      structured-log `BillingSink` wired into `s3compat.Config`; the
      handler emits `Stored`, `Puts`, `Gets`, `EgressBytes`,
      `CacheHits`, and `CacheMisses` events per request.
- [x] Range GET support, range-aligned cache chunks — handler's GET
      path parses `Range` headers and hands them to the provider
      via `GetOptions`; cache keys align with piece IDs so chunked
      reads populate / serve from the same entry as the full GET.
- [x] Hot-object promotion from Wasabi to Linode cache — GET-path
      cache miss publishes a `PromotionSignal` onto the non-blocking
      `SignalBus`; the promotion worker evaluates the signal against
      policy and, on promotion, calls `provider.GetPiece` and
      `cache.Put`.
- [x] Multi-tenant isolation layer — `internal/auth/authenticator.go`
      verifies AWS Signature V4 against a per-tenant access key and
      returns `tenantID`; `internal/auth/tenant_store.go` supplies an
      in-memory `TenantStore` with JSON loading;
      `internal/auth/rate_limit.go` applies per-tenant token-bucket
      limits sourced from the tenant's `Budgets.RequestsPerSec`.
- [x] Migration engine: dual-write, lazy migration on read,
      background rebalancer (exercised against a `local_fs_dev`
      target) — `migration/dual_write/dual_write.go` mirrors writes
      to primary + secondary and falls back on reads;
      `migration/lazy_read_repair/repair.go` copies missing pieces
      from the old backend onto the new one during GETs and updates
      the manifest; `migration/background_rebalancer/rebalancer.go`
      advances manifests through the
      `wasabi_primary → dual_write → local_primary_wasabi_backup →
      local_primary_wasabi_drain → local_only` state machine with
      bandwidth limits. Coverage in each package's `_test.go`.
      **Lazy read-repair is now wired into the gateway GET path**
      via `s3compat.Config.ReadRepair` — when the primary backend
      fails to serve a piece for a manifest whose `MigrationState`
      names a distinct new primary (Generation > 1), the handler
      falls back to `lazy_read_repair.ReadRepair.Repair()` and
      serves the repaired body. **The background rebalancer is now
      started as an optional background worker** by
      `cmd/gateway/main.go` when `config.migration.targets` is
      non-empty; the goroutine shares the promotion worker's
      shutdown context so SIGTERM drains both cleanly.
- [x] Implement S3 compliance test suite (`tests/s3_compat/`) and
      run against `wasabi` and `local_fs_dev` adapters — AWS SDK v2
      test client in `tests/s3_compat/suite_test.go` exercises PUT,
      GET, HEAD, DELETE, LIST, range GET, DELETE idempotency,
      missing-key 404s, presigned GETs, and multipart-like
      overwrite semantics. Reusable `Run(t, Setup)` harness so any
      provider can be plugged in.
- [x] Validate S3 API behavior during a simulated Wasabi →
      `local_fs_dev` migration (zero behavioral differences) —
      `tests/s3_compat/migration_test.go` runs the full compliance
      suite through a `DualWriteProvider` topology and separately
      asserts that every PUT lands on both backends and that GETs
      transparently fall back to the secondary when the primary
      fails.
- [x] Benchmark execution (PUT / GET p50 / p95 / p99, cache hit
      ratio, Wasabi origin egress ratio vs stored bytes,
      small-object overhead, LIST performance at 10M / 100M / 1B
      objects) — `tests/benchmark/runner.go` implements
      `ProviderRunner` and `RunSuite`, driving each scenario's
      request mix against a `StorageProvider`, recording per-target
      metrics, and emitting a JSON `Report` for CI consumption.
      Repair time and network-cost metrics are included as
      first-class `Result` entries for the live-driver follow-up.

---

## Phase 3: Beta Cell (Weeks 10–15)

**Status**: `IN PROGRESS`

**Goal**: stand up a real beta deployment on the AWS + Linode +
Wasabi stack with paying / design-partner customers on both B2C and
B2B paths, plus a first local DC cell for early hybrid customers.

Checklist:

- [ ] Production AWS control plane (RDS, IAM, CloudWatch,
      ClickHouse or equivalent). ClickHouse billing sink
      (`billing/clickhouse_sink.go`, schema in `billing/schema.sql`)
      is implemented and wired into `cmd/gateway/main.go` under
      `config.billing.clickhouse_url`; remaining work is the
      operator-side cluster provisioning and the rest of the
      control-plane surface (RDS, IAM, CloudWatch).
- [ ] Production Linode gateway fleet, multi-region.
- [ ] Production Wasabi buckets (per region) wired as the durable
      origin.
- [ ] Production KMS / Vault wrapper for the gateway's CMK —
      replace `client_sdk.LocalFileWrapper` (Phase 2 default,
      plaintext master key on disk) with a `KMSWrapper` /
      `VaultWrapper` that resolves `encryption.CustomerMasterKeyRef`
      against AWS KMS or HashiCorp Vault transit. The handler
      already consumes the `client_sdk.Wrapper` interface, so the
      substitution is behind `cmd/gateway/main.go#buildGatewayEncryption`;
      the remaining work is the wrapper implementation, the
      config plumbing (`config.encryption.cmk_uri` is already
      parsed and passed through), and operator runbooks for
      rotating the CMK across a live deployment.
- [x] NVMe cache nodes (L0 / L1) on Linode. `DiskCache`
      implementing `HotObjectCache` lives in
      `cache/hot_object_cache/disk_cache.go`, rebuilds its index
      from disk on restart, supports TTL + capacity eviction + hot
      pinning, and is wired into `cmd/gateway/main.go` via
      `config.gateway.cache_path`. Coverage in
      `cache/hot_object_cache/disk_cache_test.go` (round-trip,
      restart-persistence, TTL expiry, capacity eviction, orphan
      cleanup, oversize rejection).
- [ ] First local DC cell: 6–12 storage nodes, 300 TB – 1 PB raw
      capacity, HDD durable nodes (L2), NVMe cache, gateway fleet.
- [ ] 25–100 Gbps aggregate public bandwidth across Linode + local
      DC.
- [~] Abuse throttling and per-tenant bandwidth budgets — split
      across `internal/auth/rate_limit.go` (production) and
      `internal/auth/abuse.go` (scaffolded, runtime enforcement
      pending broader rollout). `rate_limit.go` layers three
      enforcement bands on every request: the per-tenant
      token-bucket RPS limit (`budgets.requests_per_sec` +
      `burst_requests`), a monthly egress ceiling sourced from
      `budgets.egress_tb_month`, and a sliding-window anomaly
      detector with a configurable EWMA baseline and alert
      multiplier; budget exhaustion returns HTTP 429 and emits
      `AbuseBudgetExhausted`, anomalies emit `AbuseAnomalyAlert`
      and, when `ThrottleOnAnomaly` is set, throttle for a
      cooldown window. Coverage in `rate_limit_test.go`.
      `abuse.go` scaffolds a sibling `AbuseGuard` middleware that
      re-reads `tenant.Budgets.EgressTBMonth` and
      `tenant.Abuse.CDNShielding` directly off the tenant record,
      adds the CDN-shielding gate (rejects direct-to-origin
      requests for shielded tenants with HTTP 403), and exposes a
      2x-of-rolling-average egress-rate anomaly path that emits
      the same billing dimensions. Wired into `cmd/gateway/main.go`
      in front of `s3compat.Handler` alongside the rate limiter.
      Coverage in `abuse_test.go`. Runtime enforcement (per-region
      tuning of `AnomalyMultiplier` / `AnomalyWindow` /
      `AnomalyCooldown`, production alert routing beyond the
      billing sink, and throttle-on-anomaly rollout) is pending.
- [~] Tenant console (React) for onboarding, billing, placement
      policy, and key management. Vite + React + TypeScript
      scaffold landed under `frontend/` with login / signup,
      dashboard (storage / requests / egress), bucket management,
      API-key management (access key + one-time secret reveal on
      create), placement-policy YAML editor with a structured
      summary, and a dedicated-cells page gated on
      `contract_type ∈ {b2b_dedicated, sovereign}`. Backend API
      scaffolded in `api/console/` — `GET /api/tenants/{id}`,
      `GET /api/tenants/{id}/usage`, `POST /api/tenants/{id}/keys`,
      and `GET` / `PUT /api/tenants/{id}/placement` — on its own
      HTTP mux bound to `:8081` (separate from the S3 data plane
      on `:8080`) via `cfg.Console.ListenAddr`. `console.UsageQuery`
      is satisfied by the ClickHouse billing sink when available;
      a no-op stub ships otherwise. Coverage in `handler_test.go`.
      Remaining: wire the React client at `frontend/src/api/` from
      the current `/api/v1/` prefix onto the new `/api/tenants/`
      routes, add an admin authenticator in front of the console
      mux, a Postgres-backed `PlacementStore` (the Phase 3
      scaffold uses `console.MemoryPlacementStore`), SSE on a
      `/api/tenants/{id}/usage/stream` endpoint, and Playwright
      e2e tests.
- [~] B2C self-service onboarding flow. Frontend signup form +
      `POST /api/v1/auth/signup` client wired in `frontend/`; the
      gateway-side handler is still deferred to the control-plane
      workstream (billing integration, email verification, CAPTCHA).
- [~] B2B dedicated cell provisioning. Console surface shipped
      (`frontend/src/pages/B2BPage.tsx`) that lists dedicated cells
      from `/api/v1/dedicated-cells` for tenants whose
      `contract_type` is `b2b_dedicated` or `sovereign`. The
      operator-side provisioning workflow (hardware allocation,
      cell bring-up) is still part of the Phase 3 ops backlog.
- [ ] Beta customer onboarding (backup, SaaS assets, AI datasets,
      media libraries, sovereign storage).
- [ ] End-to-end migration dry run: move a beta bucket from Wasabi
      to the first local cell without customer-visible changes.
- [x] Run S3 compliance test suite against `ceph_rgw` adapter —
      100% pass required before production traffic. Executed
      against a live Ceph Reef RGW (quay.io/ceph/demo:latest-reef,
      `127.0.0.1:8888`, bucket `zkof-ceph-compliance`); the full
      `Run(t, Setup)` subtest matrix in
      `tests/s3_compat/suite_test.go` passes (PUT/GET/HEAD/DELETE,
      range GET prefix/middle/tail, LIST, idempotent DELETE,
      missing-key 404, presigned GET, multipart-like overwrite,
      multipart round-trip, multipart abort, and 6+2 erasure-
      coded round-trip). Adapter fix: `providers/s3_generic/
      generic.go#PutPiece` now per-call applies
      `v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware` so
      the AWS SDK v2 signer accepts a non-seekable `io.Reader`
      body against non-AWS S3-compatible endpoints (Ceph RGW,
      Backblaze B2, Cloudflare R2). Captured test log:
      `tests/s3_compat/ceph_compliance.log`. Companion entrypoints
      `TestSuite_BackblazeB2`, `TestSuite_CloudflareR2`, and
      `TestSuite_AWSS3` gate BYOC / cloud adapter validation on
      the same env-var pattern and inherit the same PutPiece fix.
- [ ] Run S3 compliance test suite during a live Wasabi → Ceph RGW
      migration with beta customers.
- [x] Gateway fleet node health monitor (deferred from Phase 2):
      per-cell quorum, cache-tier drain, graceful gateway
      replacement. Implemented in `internal/health/health.go` with
      `GET /internal/health`, `GET /internal/ready`, `POST
      /internal/drain` endpoints, `Monitor.Track()` for in-flight
      gating, `Monitor.Drain()` bounded by `DrainTimeout`, and
      optional cache eviction on drain. Wired into
      `cmd/gateway/main.go` as a background goroutine alongside
      the rebalancer and promotion worker; SIGTERM triggers
      `Drain()` before `signalBus.Close()`. Coverage in
      `internal/health/health_test.go` for quorum transitions,
      drain readiness flip, in-flight gating, and timeout
      handling.
- [x] Phase 3 billing metering backend. `ClickHouseSink` in
      `billing/clickhouse_sink.go` ingests usage events via
      ClickHouse HTTP `INSERT FORMAT JSONEachRow`, batches by size
      + interval, retries 5xx with exponential backoff, and
      drains on `Close()`. Schema in `billing/schema.sql` ships
      `usage_events` (MergeTree) + `usage_counters`
      (SummingMergeTree). Coverage in
      `billing/clickhouse_sink_test.go` for batch-size flush,
      close flush, 5xx retry, and config validation.
- [x] BYOC / cloud adapter compliance entrypoints.
      `TestSuite_BackblazeB2`, `TestSuite_CloudflareR2`, and
      `TestSuite_AWSS3` added in `tests/s3_compat/suite_test.go`
      following the `TestSuite_CephRGW` pattern, each gated on
      the provider's `*_ENDPOINT` / `*_BUCKET` env vars so CI
      stays green without credentials.
- [x] Real S3 multipart upload support. `CreateMultipartUpload`,
      `UploadPart`, `CompleteMultipartUpload`, `AbortMultipartUpload`,
      and `ListMultipartUploads` implemented in
      `api/s3compat/multipart_handler.go`, backed by
      `api/s3compat/multipart/store.go` (in-memory `Store` with
      tenant-scoped listing, part-ETag validation, and idempotent
      abort). Per-part pieces are addressed by a deterministic
      `{uploadID}-p{partNumber:05d}` piece ID; the `Complete`
      aggregate ETag follows the S3 `MD5(part_md5s)-N` convention.
      The GET path in `api/s3compat/erasure_coding.go#getMultipart`
      concatenates pieces in ascending `PartNumber` order.
      Handler routing in `api/s3compat/handler.go#dispatch` covers
      `?uploads`, `?uploadId=...&partNumber=...`, and
      `?uploadId=...` variants. Coverage in
      `api/s3compat/multipart/store_test.go` and integration tests
      `MultipartRoundTrip` + `MultipartAbort` in
      `tests/s3_compat/suite_test.go`.
- [x] Erasure coding wired into the write path for local DC
      backends. `PlacementPolicy.ErasureProfile` diverts PUTs to
      `api/s3compat/erasure_coding.go#putErasureCoded`, which
      shards the body into k+m Reed-Solomon pieces per stripe via
      the clean-room encoder in
      `metadata/erasure_coding/encoder.go` (codec:
      `github.com/klauspost/reedsolomon`, MIT). Profiles are
      registered in `metadata/erasure_coding/registry.go`
      (`DefaultRegistry` ships 6+2, 8+3, 10+4, 12+4, 16+4 per
      `StandardProfiles`). Each shard lands as its own piece
      carrying `StripeIndex`, `ShardIndex`, and `ShardKind`
      metadata; `getErasureCoded` reconstructs the plaintext and
      tolerates up to `ParityShards` missing shards per stripe.
      `cmd/gateway/main.go` wires the default registry into
      `s3compat.Config.ErasureCoding`. Coverage in
      `metadata/erasure_coding/encoder_test.go` (pad, round-trip,
      single + multi-shard loss) and `ErasureRoundTrip` in
      `tests/s3_compat/suite_test.go`.
- [x] Storj BYOC provider adapter wired into gateway. `providers/storj/storj.go`
      implements `StorageProvider` via the native `storj.io/uplink` library
      (not the S3 gateway). `providers/storj/uplink_bridge.go` bridges
      `*uplink.Project` to the adapter's `UplinkProject` interface.
      `cmd/gateway/main.go` registers the provider under `"storj"` when
      `config.providers.storj.access_grant` is set. PR #19 review findings
      (ContentType guard, ListObjects page cap, OAuth login fast-path,
      VerifiedCheck gate, verify endpoint auth, Vite proxy) are all resolved.
- [x] Lightweight Docker demo container. `Dockerfile` (multi-stage:
      Go build + Vite frontend build + Alpine runtime),
      `docker-compose.yml`, `demo/config.json`, `demo/tenants.json`,
      and `.dockerignore`. The container runs the gateway in dev mode
      (`local_fs_dev` backend, in-memory manifest store, logger
      billing sink) with the S3 API on `:8080` and the console API
      on `:8081`. Pre-loaded demo tenant credentials (`demo-access-key`
      and the `kmail-access-key` pair scoped to tenant
      `kmail-tenant-001`) allow immediate use with any S3-compatible
      client. Verified as the backend for kmail's Stalwart blob store
      — the same S3 API that serves Phase 1 Wasabi and Phase 2 Ceph
      RGW deploys now serves kmail's local dev stack. Object data
      persists in the `zk-data` Docker volume; tenant and manifest
      state is in-memory only.
- [x] Kapp Business Suite integration. The Kapp `kapp-fab` repo now
      provisions a per-tenant HMAC credential pair plus a dedicated
      bucket against the fabric console API at `:8081` during its
      tenant setup wizard, and runs every file attachment upload /
      download through the fabric in `managed` encryption mode so
      ERP file attachments inherit per-tenant zero-knowledge
      encryption. Joins kmail and zk-drive as a reference downstream
      integration alongside the existing Stalwart blob store path.
      Co-deploys cleanly via `docker-compose.yml` — Kapp talks to the
      fabric on the same Compose network with no extra plumbing.

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
