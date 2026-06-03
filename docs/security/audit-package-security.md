# Security Audit Package — `zk-object-fabric`

| Field | Value |
|---|---|
| Document version | 2026-05-30 (WS1.3) |
| Source commit | branch HEAD of PR #77 (recorded in the bundle's `MANIFEST.txt` at build time; merge-base was `dac9ef3` on `main`) |
| Audience | Third-party security firm (Trail of Bits / Cure53 / NCC Group) |
| Companion | [`audit-package-cryptography.md`](audit-package-cryptography.md), [`threat-model.md`](threat-model.md) |
| Scope | Authentication, authorization, multi-tenancy isolation, manifest sealing, CMK handling, presigned-URL semantics, S3 SigV4 plumbing |

The audit team should read [`threat-model.md`](threat-model.md)
first. Every `path:line` reference below is grep-verified against
the branch state captured in the corresponding `make audit-bundle`
tarball's `MANIFEST.txt`. If this document is read against a later
commit on `main`, line numbers will drift — re-pin to the exact
tree by hashing the shipped sources against `MANIFEST.txt`, or
regenerate the bundle from that later commit. The contract is
*SHA-anchored source*, not SHA-anchored line numbers. The package
is intentionally a single file so an auditor can read it linearly
and pivot into source only when they want detail.

## Table of contents

1. [Scope and review goals](#1-scope-and-review-goals)
2. [Architecture orientation](#2-architecture-orientation)
3. [Authentication path (SigV4)](#3-authentication-path-sigv4)
4. [Tenant isolation and authorization](#4-tenant-isolation-and-authorization)
5. [Manifest store at rest](#5-manifest-store-at-rest)
6. [CMK handling and the production fail-closed guard](#6-cmk-handling-and-the-production-fail-closed-guard)
7. [Rate limiting and abuse guard](#7-rate-limiting-and-abuse-guard)
8. [Known limitations and explicit out-of-scope items](#8-known-limitations-and-explicit-out-of-scope-items)
9. [Test posture](#9-test-posture)
10. [Suggested attack ideas](#10-suggested-attack-ideas)

---

## 1. Scope and review goals

We are asking the auditor to test the gateway code path as it
would run in production: a stateless `cmd/gateway/main.go` process
fronting the S3 API, with Postgres for manifests and a CMK held in
AWS KMS or HashiCorp Vault Transit. The cryptography itself is
covered in the companion package; this package focuses on the
plumbing around the crypto.

Concretely we want the audit to give us a yes/no answer on each of
these claims:

| # | Claim | Code primary |
|---|---|---|
| C1 | SigV4 header and presigned signatures cannot be replayed beyond `MaxClockSkew` (default 15 min). | `internal/auth/authenticator.go:177`, `internal/auth/authenticator.go:411-433` |
| C2 | Cross-tenant access is impossible via the auth layer — the `AuthResult.TenantID` returned to every handler is derived from the signing access key alone. | `internal/auth/authenticator.go:108-141`, `internal/auth/tenant_store.go` |
| C3 | A Postgres-only admin compromise (A4 in threat model) cannot read manifest bodies (object keys, piece IDs, sizes) when the `BodyEncryptor` is configured. | `metadata/manifest_store/postgres/body_encryptor.go:104`, `metadata/manifest_store/postgres/store.go:88-104` |
| C4 | An operator cannot accidentally run production with the plaintext local-file CMK. | `cmd/gateway/main.go:235-243`, `cmd/gateway/main.go:627-668` |
| C5 | A presigned URL with `X-Amz-Expires` cannot outlive its expiry, regardless of clock skew. | `internal/auth/authenticator.go:310-409` |
| C6 | An aws-chunked streaming PUT cannot smuggle bytes by reusing a previous chunk's signature. | `internal/auth/authenticator.go:484-505` |
| C7 | A presigned URL signed for `GET /tenantA/secret` cannot be redirected to `/tenantB/secret` by query-parameter manipulation. | `internal/auth/authenticator.go:39-65`, `internal/auth/authenticator.go:664-742` |

## 2. Architecture orientation

### 2.1 Repo layout (security-relevant only)

```
cmd/gateway/main.go                   # process entrypoint, CMK loader, fail-closed guards
api/s3compat/                         # S3 REST surface
  ├─ handler.go                       # request dispatch
  ├─ encryption.go                    # mode selection (Strict ZK vs ManagedEncrypted)
  ├─ encryption_pipeline.go           # central DEK / Wrap / EncryptedSize helpers
  └─ multipart/                       # session DEK lifecycle + scrubbing
api/console/                          # management API (admin token surface)
internal/auth/                        # SigV4, rate limit, abuse guard
  ├─ authenticator.go                 # HMACAuthenticator + DefaultStrategies
  ├─ rate_limit.go                    # per-tenant token bucket + alerting
  ├─ abuse.go                         # CDN allowlist + egress budget + alerting
  ├─ ddos_shield.go                   # slowloris / per-IP cap
  ├─ tenant_store.go                  # access-key → tenant resolution
  └─ legal_response.go                # legal-hold short-circuit
encryption/                           # AEAD framing + CMK wrap (covered by crypto package)
metadata/manifest_store/postgres/     # at-rest manifest store + BodyEncryptor
```

### 2.2 Two operating modes

The gateway supports two encryption modes per tenant, set in the
tenant config and exposed via the `EncryptionMode` enum
(`encryption/envelope.go:12-49`):

- **`client_side`** (Strict ZK) — the tenant's SDK encrypts in
  process. The gateway sees ciphertext only and treats it as
  opaque bytes. The gateway never sees plaintext, the DEK, or the
  CMK.
- **`managed_encrypted`** — the gateway holds the per-object DEK,
  wraps it with the tenant's CMK, and seals the object bytes.
  This is the mode that lets the gateway perform server-side
  features (range decryption, range GET, EC repair) but it widens
  the trust boundary to include the gateway operator.

The relevant predicate is `IsGatewayEncrypted`
(`api/s3compat/encryption_pipeline.go:30-33`). Reviewers should
verify it cannot accidentally return `true` for `client_side`.

## 3. Authentication path (SigV4)

The authenticator is a strategy dispatch — header-signed requests
and query-string presigned URLs each have their own implementation
of the `AuthStrategy` interface
(`internal/auth/authenticator.go:142-155`). The default list is:

```go
// DefaultStrategies — internal/auth/authenticator.go:161-169
PresignedV4Strategy{},
HeaderV4Strategy{},
```

`PresignedV4Strategy.Matches` (line 300-308) returns `true` iff the
request URL contains `X-Amz-Signature`; otherwise the header
strategy runs. The dispatcher itself is `AuthenticateEx`
(`internal/auth/authenticator.go:199-228`) — every handler in
`api/s3compat` calls this and uses `AuthResult.TenantID`.

### 3.1 What the audit should verify on the header path

`HeaderV4Strategy.Authenticate` (line 242-291):

1. Parses the `Authorization` header into the scope tuple
   `(date, region, service)` via `parseAuthHeader`
   (line 540-577).
2. Looks up the access key in the `TenantStore` and gets the
   secret key + tenant ID.
3. Rebuilds the canonical request on a `cloneForSigning`-d
   copy (line 435-451) and recomputes the signature via
   `signRequest` + `deriveSigningKey` (line 579-631).
4. Compares signatures with `hmac.Equal` (constant time).
5. Verifies the `X-Amz-Date` (or `Date`) header is within
   `MaxClockSkew` (default 15 min — line 177) of `a.Clock()`
   via `parseSigningTimestamp` (line 411-433).

Specific failure modes the audit should attempt:

- **Skew bypass**: send a request with `X-Amz-Date` set to "now +
  16 minutes". Must reject.
- **Region-shift**: sign with `region=us-east-1`, present to a
  gateway configured for `us-west-2`. The scope in the
  Authorization header is part of the signed key derivation
  (`deriveSigningKey`, line 626-631) so a region change will
  produce a different signing key. Must reject.
- **Service-shift**: sign with `service=s3`, change to
  `service=glacier`. Same derivation; must reject.
- **Path canonicalisation**: send `/tenantA/secret` and
  `/tenantA//secret` and `/tenantA/./secret`. Confirm
  `canonicalURI` (line 664-688) normalises consistently with the
  AWS SigV4 spec.

### 3.2 What the audit should verify on the presigned path

`PresignedV4Strategy.Authenticate` (line 310-409):

1. Extracts `X-Amz-Algorithm`, `X-Amz-Credential`,
   `X-Amz-SignedHeaders`, `X-Amz-Date`, `X-Amz-Expires`, and
   `X-Amz-Signature` from the query string.
2. Validates `X-Amz-Algorithm == "AWS4-HMAC-SHA256"`.
3. Parses the credential scope and runs the same key derivation
   as the header path.
4. Strips `X-Amz-Signature` from the canonical query
   (`stripQueryParam`, line 507-523), rebuilds the canonical
   request, recomputes the signature, and compares.
5. Enforces the expiry. The exact predicate at
   `internal/auth/authenticator.go:368` is
   `now.After(reqTime.Add(time.Duration(expiresSec)*time.Second + skew))`.
   That means the effective lifetime of a presigned URL is
   `X-Amz-Expires + MaxClockSkew`, **not** `X-Amz-Expires` alone
   — a `X-Amz-Expires=60` URL is rejected only when `now` is more
   than `60s + MaxClockSkew` (default `75s`) after the signing
   timestamp. This is intentional: `MaxClockSkew` accounts for the
   spread between signing-host clock and gateway-host clock, and
   applying it symmetrically on both bounds (future-dated and
   past-expiry) prevents legitimate URLs from being prematurely
   rejected when the gateway runs slightly fast. The auditor should
   confirm this is the desired contract and that callers building
   short-lived URLs (e.g. `X-Amz-Expires=10`) are aware their URLs
   may be honoured for up to `10s + MaxClockSkew`.

Specific failure modes the audit should attempt:

- **Expiry bypass**: present a URL whose elapsed lifetime is
  `X-Amz-Expires + MaxClockSkew + 1s` past the signing timestamp.
  Must reject. Presenting one only `X-Amz-Expires + 1s` past must
  STILL ACCEPT — that is by design (see point 5 above) and a 60s URL
  presented at 61s is inside the skew window.
- **Path swap**: take a URL signed for `/tenantA/secret`, change
  the request path to `/tenantB/secret`, keep the same signature.
  The canonical URI is part of the signed string; must reject.
- **Header drop**: a URL signed with `SignedHeaders=host;range`
  must reject if the `Range` header is dropped from the inbound
  request.
- **Query reorder**: SigV4 requires lexicographically sorted query
  params for canonicalisation (`canonicalQuery`, line 690-742).
  Confirm reordering does NOT change the signature outcome (i.e.
  the implementation sorts on the verifier's side too).

### 3.3 Clock-skew window

The default 15-minute window matches AWS's documented behaviour.
`Authenticate` uses `a.Clock()` (injected for tests) — production
falls back to `time.Now` (line 199-211). Reviewers should confirm
there is no path that uses `Clock()` for one half of the
comparison and `time.Now` for the other. (Grep-confirmed: every
read goes through `parseSigningTimestamp` which receives the
clock as an argument.)

### 3.4 Chunked-upload signatures

aws-chunked streaming PUTs sign each chunk with an HMAC chained
off the previous chunk's signature. The seed signature is
verified by `HeaderV4Strategy.Authenticate`; the per-chunk
signatures are authenticated by `VerifyChunkSignature`, which
wraps `ComputeChunkSignature` (returns the expected SigV4 chunk
signature) plus a `crypto/subtle.ConstantTimeCompare` against the
`receivedSig` from the chunk header. The intended call shape is

```go
expected, err := auth.VerifyChunkSignature(prevSig, chunkData, signingKey, ts, scope, receivedSig)
if err != nil { /* abort upload, return 403 */ }
prevSig = expected
```

so the comparison and the chain-advance are a single typed
operation that cannot accidentally degrade to a `==`. Note that
the consuming handler in `api/s3compat/` is tracked as a separate
workstream (see crypto §6.2 "Note for the auditor") — the
function pair is fully tested in `internal/auth/`, but no
production call site invokes it yet.

Specific failure modes the audit should attempt:

- **Chunk replay**: take a captured `chunkData + signature` pair
  from one upload and splice it into a different upload. The
  `prevSig` chain anchors back to the seed signature so the
  splice must fail to verify.
- **Chunk truncation**: drop the final `0\r\n` terminator chunk
  to see if the handler accepts a short upload. (Behaviour
  expectation: PUT fails because the closing signature does not
  verify against a 0-byte chunk.)

## 4. Tenant isolation and authorization

### 4.1 Tenant ID provenance

Every request that the handler accepts has gone through
`Authenticator.AuthenticateEx`, which returns an `AuthResult`
struct whose `TenantID` field is sourced **only** from the
`TenantStore` lookup against the signing access key
(`internal/auth/authenticator.go:108-141`). There is no path
where a request header or query parameter can override this — the
audit should attempt to find one.

### 4.2 Tenant-keyed lookups in the manifest store

`metadata/manifest_store/postgres/store.go` enforces tenant
scoping in two complementary ways:

1. The composite key `ManifestKey{TenantID, Bucket,
   ObjectKeyHash}` is part of every Put/Get/Delete signature,
   and the SQL statements WHERE-filter on `tenant_id`.
2. The `BodyEncryptor`'s AAD is derived from the same key
   (`bodyContextAAD` in `body_encryptor.go:66-86`). A
   manifest body sealed for tenant A cannot open under tenant B's
   `BodyContext` — even if a Postgres admin swaps the row.

We have no application-layer middleware that auto-injects
`tenant_id` into every query yet (Workstream 3.4 item). The audit
should look for code paths that perform raw SQL without going
through `Store.Get`/`Put`/`List` and flag any that don't
explicitly scope by tenant_id.

### 4.3 Admin / console API surface

`api/console/` has separate auth — it currently uses a flat
`AdminToken` bearer check. **This is a known weakness** —
Workstream 3.2 will replace it with RS256/ES256 JWTs. The audit
should treat the current console API as a privileged endpoint
that MUST be network-restricted, not as a tenant-isolated
surface.

## 5. Manifest store at rest

`metadata/manifest_store/postgres/body_encryptor.go` is the
at-rest seal for the manifest JSON. The implementation
(`AEADBodyEncryptor`, line 88-160) uses
XChaCha20-Poly1305 with a gateway-held 32-byte key. The audit
package for cryptography covers the AEAD itself; this package
focuses on the plumbing.

### 5.1 What the auditor should verify

- The encryptor is **always** configured when the gateway runs in
  production. There is no fail-open path: if `Encrypt` returns an
  error, `Store.Put` returns an error and the request fails
  (line 88-104 of `store.go`).
- Legacy unsealed rows are openable. The encryptor's `Decrypt`
  accepts ciphertext sealed with an empty `BodyContext` so a
  forward-rolling deployment does not have to re-encrypt every
  row up-front. The audit should confirm that *new* writes always
  use the full context (grep-verified in `store.go:92-100`).
- The key the encryptor is loaded from is wired separately from
  any per-tenant CMK. A compromise of one tenant's CMK does not
  expose another tenant's manifest body.

### 5.2 What the auditor should attempt

- **Row swap**: take a row from `(tenantA, bucketA, hashA)`,
  rewrite the row's primary-key columns to
  `(tenantB, bucketB, hashB)`, and read via `Get`. The
  `BodyContext` AAD must reject the decrypt with an "AEAD
  authentication failed" error, not return tenant A's body to
  tenant B.
- **Body splice**: take half the ciphertext from row A and half
  from row B. AEAD frame parsing must reject.

## 6. CMK handling and the production fail-closed guard

`cmd/gateway/main.go:74-95` declares a CLI flag
`--allow-local-cmk` that defaults to `false`. When unset the
gateway calls `enforceProductionLocalCMK`
(`cmd/gateway/main.go:235-243`) at startup; if the configured CMK
URI is `cmk://local/...` AND `Env == "production"` AND
`AllowLocalCMK == false`, the process exits with
`errProductionLocalCMK` (`cmd/gateway/main.go:627-668`).

The error message is intentionally explicit about the operational
intent:

> gateway: env=production but the local file CMK wrapper is in
> use; this exposes every tenant DEK to anyone with gateway disk
> access. Use AWS KMS (kms://) or HashiCorp Vault Transit
> (vault://). For HSM-fuse deployments where the local file path
> maps to hardware-backed key material, set `--allow-local-cmk`
> or `encryption.allow_local_cmk=true` to override this guard.

The auditor should:

- Verify the override is reachable only through `--allow-local-cmk`
  / `encryption.allow_local_cmk` and not through any environment
  variable parsing path.
- Confirm there is no code path that constructs a
  `LocalFileWrapper` (`encryption/client_sdk/wrap.go:39-122`)
  without going through the gateway boot — the audit can grep
  `LocalFileWrapper{` and confirm every callsite is in a test
  file, the boot path, or behind the override.
- Verify the `isLocalFileCMK` detector covers all forms of the
  `cmk://local/...` URI (with and without trailing slash, with
  query parameters, etc.).

## 7. Rate limiting and abuse guard

### 7.1 Rate limiter (`internal/auth/rate_limit.go`)

The rate limiter is a per-tenant token bucket
(`RateLimiter.Allow`, line 216-259) with Redis-backed shared
state. The current implementation is **fail-open** when Redis is
unreachable — Workstream 2.4 will add a `fail_closed` mode.

The auditor should:

- Verify the limiter is invoked **before** the request body is
  read on every PUT/GET path. A naive implementation that
  invokes after body-read would let a tenant exhaust gateway
  bandwidth even while rate-limited.
- Confirm the egress tracker (`AllowEgress`, line 261-292) and
  per-tenant `egressTracker` (line 135-142) cannot be bypassed
  via a Range GET that requests a tiny range but pulls a large
  body through CDN-style edge headers.

### 7.2 Abuse guard (`internal/auth/abuse.go`)

The abuse guard layers on top of the rate limiter and trips
**alerts** (not blocks) based on anomaly tracking
(`abuseAnomalyTracker`, line 149-158). It is wired to the
`AlertSink` interface in the rate limiter (line 55-62) so a
single alert pipeline can fan out to webhooks / billing / PagerDuty.

The auditor should:

- Verify the alert path itself cannot be DoS'd by a tenant
  intentionally tripping abuse thresholds (the alert window is
  `1 hour` by default — `anomalyWindow` line 481-486 — and uses
  `maybeAlert` deduping line 394-444).
- Confirm `tenantEgressBudgetBytes` (line 463-471) correctly
  reports an explicitly-zero budget as "blocked" vs unset as
  "unlimited".

### 7.3 DDoS shield (`internal/auth/ddos_shield.go`)

Pre-auth defence: a per-IP slowloris detector + connection cap.
Runs before SigV4 verification so an attacker who can't
authenticate cannot tie up auth-handling goroutines. The audit
should attempt:

- **Slowloris**: open N connections, write 1 byte/sec of the SigV4
  header. The shield should close them.
- **TLS handshake bomb**: open and close TLS without sending a
  request. The shield should track that as a connection event
  and trip the per-IP cap.

## 8. Known limitations and explicit out-of-scope items

- **No mTLS between gateway and Postgres / cache / billing.** The
  TLS we use is server-auth only. mTLS is on the Workstream 3.1
  roadmap. The audit should flag this in the report but should
  **not** treat it as a finding against the current code — it is
  an acknowledged gap.
- **Console API uses a flat bearer token.** RS256/ES256 JWT
  migration is on Workstream 3.2. Same as above: flag but not a
  novel finding.
- **Postgres Row Level Security (Workstream 3.4) — in progress.**
  The application layer enforces tenant scoping on every query
  (`metadata/manifest_store/postgres/store.go`); the audit should
  look for any path that bypasses the `Store` wrapper. RLS is now
  armed as defence-in-depth on the **manifests**, **content_index**,
  **bucket_config** (versioning, object lock, CORS, lifecycle), and
  **multipart** (`multipart_uploads` + `multipart_parts`) tables:
  each tenant-scoped statement runs in a transaction that binds
  a transaction-local `zkof.tenant_id` GUC, and a FORCE'd
  `tenant_isolation` policy re-checks it. The mechanism is centralised
  in `internal/rlsdb` (GUC binding + the single-source policy
  DDL), with per-table operator references in
  `metadata/manifest_store/postgres/rls.sql`,
  `metadata/content_index/postgres/rls.sql`,
  `metadata/bucket_config/postgres/rls.sql`, and
  `api/s3compat/multipart/rls.sql`. The audited cross-tenant
  readers (`ScanManifests` for the AAD migration sweep, `ListTenants`
  for orphan GC, `ListLifecycle` for the background lifecycle evaluator,
  and the multipart expiry sweeper's `sweepExpired` enumeration)
  bind a `zkof.scan_all` read-only bypass that the
  `WITH CHECK` clause deliberately omits, so no sweep can write across
  tenants — the multipart sweeper re-binds each upload's own tenant
  before deleting it. `multipart_parts` carries a denormalised
  `tenant_id` (copied from its owning upload) so the same uniform policy
  applies; a cross-tenant `UploadPart`/`Complete`/`Abort` therefore sees
  zero rows and returns `NoSuchUpload` (404) rather than a 403 existence
  oracle. RLS only applies to a non-superuser, non-`BYPASSRLS` role,
  so the gateway refuses to boot in production on a privileged metadata
  connection (`cmd/gateway/main.go` `checkProductionRLSRole`). The
  remaining tenant tables (the console auth/refresh/mfa stores) reuse the
  same substrate in follow-ups.
- **CMK is held by the gateway process in `ManagedEncrypted` mode.**
  This is the documented trust model — in Strict ZK mode the
  gateway never sees the CMK; in `ManagedEncrypted` mode the
  gateway operator IS in the trust boundary. The audit should not
  flag this as a finding; it should flag any path that **leaks**
  the CMK material to a less-privileged location (logs, error
  messages, telemetry).

## 9. Test posture

Static analysis (run on every PR via CI in
`.github/workflows/ci.yml`):

- `go vet ./...`
- `staticcheck ./...`
- `govulncheck ./...`

Test suites the auditor can run for orientation:

- `go test -race ./internal/auth/...` — SigV4, presigned URL,
  chunked upload, rate limit, abuse guard.
- `go test -race ./metadata/manifest_store/postgres/...` — body
  encryptor round-trip + AAD binding tests.
- `go test -race ./encryption/...` — wrap / unwrap, KMS /Vault
  mock paths.
- `go test -race ./api/s3compat/...` — handler + multipart
  session DEK scrubbing.

Fault-injection / chaos tests
(`tests/chaos/`, PR #76) cover provider failures
and manifest store failures; the load harness
(`tests/benchmark/`, PR #75) covers SLA-bound latency and
sustained throughput. Neither suite is a security test; the audit
should treat them as evidence that the data path is resilient,
not as evidence of security correctness.

## 10. Suggested attack ideas

Non-exhaustive list to seed the audit's threat-modelling session:

1. **Signature confusion**: present a header-signed request whose
   `Authorization` header is also reproduced as a query
   parameter. Which strategy wins, and does the loser's
   verification still run?
2. **TenantStore poisoning**: if a tenant can register an access
   key with the prefix of another tenant's, does the `Store`
   resolver pick the wrong one?
3. **Range-GET on a sealed manifest body**: confirm range reads
   are only valid on object bodies, not on the manifest JSON.
4. **Multi-part upload session smuggling**: a PUT that starts a
   multipart upload, switches the upload ID mid-session, and
   completes against a different upload's parts. The multipart
   handler in `api/s3compat/multipart/` should reject any cross-
   session part — confirm by code reading.
5. **Cache-poisoning via hot-object promotion**: a tenant pushes
   ciphertext to the hot tier, then changes the manifest's
   ciphertext pointer to a different piece. On promotion, does
   the cache serve stale-or-spoofed bytes?
6. **Convergent dedup oracle**: see the crypto package §5 for
   the per-tenant HKDF salt that makes this attack structurally
   impossible across tenants. Within a single tenant the oracle
   IS exploitable — but only by an attacker who already has
   access to that tenant's data, which is not a new privilege.
   Confirm the salt is bound correctly.
7. **`X-Amz-Content-SHA256: UNSIGNED-PAYLOAD` on a chunked PUT**:
   AWS S3 explicitly forbids this combination. Confirm the
   handler rejects it.
8. **Legal-hold short-circuit**: `internal/auth/legal_response.go`
   exists to short-circuit deletes for tenants under legal hold.
   Confirm a tenant cannot force-delete via a presigned URL that
   was generated before the hold was applied.
