# Threat Model — `zk-object-fabric`

This document is the shared starting point for both audit packages
in this directory. It is intentionally short: it lists the actors,
trust boundaries, and the asset-→-attacker mapping the code is
designed to defend against. The two audit packages
(`audit-package-security.md`, `audit-package-cryptography.md`) cite
back into this file by section number when they explain why a given
mitigation is in the code.

---

## 1. Actors

| # | Actor | Trust | Notes |
|---|---|---|---|
| A1 | **Tenant client** (SDK / `aws-cli` / app code) | Untrusted | Holds tenant access keys, optionally the tenant CMK in Strict ZK mode. |
| A2 | **Gateway operator** (the team running `cmd/gateway`) | Semi-trusted | Has shell access to the gateway host. In Strict ZK mode they cannot decrypt object bodies; in `ManagedEncrypted` mode they hold the CMK and can. |
| A3 | **Backend storage provider** (Wasabi / Ceph / B2 / R2) | Untrusted | Sees only ciphertext pieces (Strict ZK) or AEAD-sealed pieces (`ManagedEncrypted`). Object metadata is NOT in the provider — only opaque piece IDs. |
| A4 | **Metadata-DB admin** (Postgres operator) | Untrusted in Strict ZK, semi-trusted in `ManagedEncrypted` | Can read every row of `manifests`. The `BodyEncryptor` (see `metadata/manifest_store/postgres/body_encryptor.go`) seals the JSON body so a DB-only compromise does not expose object keys or piece locations. |
| A5 | **KMS / Vault operator** | Trusted (out of scope) | Owns the root CMK material. A compromise here is equivalent to losing the entire tenant data set; we do not attempt to defend against it. |
| A6 | **Cross-tenant attacker** | Untrusted | A legitimate tenant trying to read another tenant's data through any code path (manifest lookup, dedup oracle, presigned URL, EC repair, cache promotion). |
| A7 | **Network adversary on the public API edge** | Untrusted | Can observe and modify any byte on the wire before TLS terminates at the gateway. |
| A8 | **Network adversary on the internal control plane** | Untrusted | Between gateway and Postgres / cache / billing sink. Mutual TLS across the internal control plane is the intended mitigation and is not yet enforced end-to-end; today this attacker is partially in scope (gateway-Postgres uses TLS, gateway-cache may be loopback only). |

## 2. Assets

| Asset | Confidentiality | Integrity | Availability |
|---|---|---|---|
| Object plaintext (Strict ZK mode) | A1 only | A1 only | Best-effort (multi-backend) |
| Object plaintext (`ManagedEncrypted` mode) | A1 + A2 | A1 + A2 | Best-effort (multi-backend) |
| Tenant DEK (wrapped) | A1 + (A5 via CMK) | A1 + A2 | Backed up with manifest |
| Tenant CMK | A1 (Strict ZK) or A2 (`ManagedEncrypted`); always also A5 | Same | A5 |
| Manifest JSON (object key, piece map, sizes) | Tenant + A2 + (A4 in `ManagedEncrypted`) | Tenant + A2 | Postgres HA |
| Tenant access keys (SigV4) | A1 + A2 | A1 + A2 | Required for every request |
| Billing ledger | A1 (own row) + A2 | A1 (own row) + A2 | ClickHouse |
| Audit/access log | A2 | A2 (append-only) | ClickHouse |

## 3. Trust boundaries

```
┌───────────────────────────────────────────────────────────────────┐
│  Tenant network                                                   │
│                                                                   │
│  ┌───────────────┐                                                │
│  │ SDK (Strict   │── encrypts in process ──> ciphertext + DEK     │
│  │ ZK only)      │                                                │
│  └───────────────┘                                                │
└────────│──────────────────────────────────────────────────────────┘
         │ TLS 1.2+ to public API edge
         ▼
┌───────────────────────────────────────────────────────────────────┐
│  Gateway host (cmd/gateway/main.go)                               │
│                                                                   │
│  ┌────────────┐  ┌──────────────────┐  ┌────────────────────────┐ │
│  │ Auth       │->│ S3 handler       │->│ Encryption pipeline     │ │
│  │ (SigV4)    │  │ (api/s3compat)   │  │ (api/s3compat/encryption│ │
│  └────────────┘  └──────────────────┘  │  _pipeline.go)          │ │
│                                        └────────────────────────┘ │
│  ┌────────────┐  ┌──────────────────┐  ┌────────────────────────┐ │
│  │ Rate /     │  │ Manifest store   │  │ Wrapper (CMK)           │ │
│  │ abuse      │  │ (postgres)       │  │ local / KMS / Vault     │ │
│  └────────────┘  └──────────────────┘  └────────────────────────┘ │
└────────│───────────────│──────────────────────────│───────────────┘
         │ TLS           │ TLS to Postgres          │ TLS to KMS/Vault
         ▼               ▼                          ▼
   Backend storage  Postgres manifest DB     KMS / Vault Transit
   (Wasabi/Ceph/    (BodyEncryptor seals     (CMK lives here in
    B2/R2)          rows in Strict ZK)        production)
```

The dashed-line direction is "data flows down". The trust boundary
sits between every horizontal line. Strict ZK mode pushes the
plaintext boundary all the way up into the tenant SDK; the gateway
sees only ciphertext, the backend never sees plaintext, and the
Postgres body column is also sealed.

## 4. Out-of-scope threats

The audit packages do **not** claim defence against:

- **Compromise of the KMS / Vault root** (A5). Equivalent to losing
  every CMK; the project has no recovery story for this and does
  not try to invent one.
- **Side-channel attacks on the gateway host** (cache timing,
  Rowhammer, etc.) Mitigation is operational (run gateways on
  dedicated hosts) and out of the code's scope.
- **Quantum cryptanalysis of XChaCha20-Poly1305 or HKDF-SHA256.**
  See the cryptography package §10 for the post-quantum migration
  plan; current code uses non-PQ primitives.
- **Denial of service against the public S3 edge.** Rate limiting
  (`internal/auth/rate_limit.go`) and abuse guard
  (`internal/auth/abuse.go`) mitigate single-tenant abuse; a true
  DDoS requires a CDN / scrubbing layer that lives in front of the
  gateway.
- **Live-process memory disclosure.** Multipart session DEKs are
  scrubbed at session end (`api/s3compat/multipart/...`) but a
  core dump captured during an active session can reveal the
  in-memory DEK. Out of scope for the gateway; mitigated by
  operational hardening of the host.

## 5. Mapping to audit packages

| Threat | Defended by | Audit package |
|---|---|---|
| Cross-tenant read via auth | SigV4 + per-tenant `TenantStore` | security §3 |
| Cross-tenant read via dedup | Per-tenant HKDF salt in `DeriveConvergentDEK` | crypto §5 |
| Tampered ciphertext in backend | XChaCha20-Poly1305 AEAD per chunk | crypto §3, §4 |
| Postgres-only admin reading manifests | `BodyEncryptor` AAD-bound rows | crypto §7, security §5 |
| Plaintext CMK on disk in production | `enforceProductionLocalCMK` fail-closed guard | security §6 |
| Presigned-URL replay outside window | `MaxClockSkew` + `X-Amz-Expires` enforcement | security §3.3 |
| Chunked PUT signature reuse | `VerifyChunkSignature` chained HMACs | crypto §6, security §3.4 |
| Manifest-body lift-and-shift across rows | `BodyContext` AAD binding | crypto §7 |
| Wrapped DEK lift-and-shift across CMKs | `dekKeyID` binding inside `WrappedDEK` | crypto §8 |
