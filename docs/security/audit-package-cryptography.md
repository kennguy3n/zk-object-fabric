# Cryptography Audit Package — `zk-object-fabric`

| Field | Value |
|---|---|
| Document version | 2026-05-30 (WS1.4) |
| Source commit | `dac9ef3` (`main`) |
| Audience | Third-party cryptography auditor |
| Companion | [`audit-package-security.md`](audit-package-security.md), [`threat-model.md`](threat-model.md) |
| Scope | AEAD construction, AAD design, DEK / KEK / CMK hierarchy, KDF use, RNG use, nonce handling, convergent-encryption parameters, SigV4 HMAC chaining |

Read [`threat-model.md`](threat-model.md) first. Every `path:line`
reference is grep-verified against the source commit; any drift
means a later change has landed and the bundle should be
regenerated (`make audit-bundle`).

## Table of contents

1. [Scope and review goals](#1-scope-and-review-goals)
2. [Cryptographic primitives in use](#2-cryptographic-primitives-in-use)
3. [Object body AEAD: per-chunk framing](#3-object-body-aead-per-chunk-framing)
4. [Per-chunk AAD construction](#4-per-chunk-aad-construction)
5. [Convergent encryption — HKDF DEK derivation and deterministic nonces](#5-convergent-encryption--hkdf-dek-derivation-and-deterministic-nonces)
6. [SigV4 HMAC and chunked-upload chain](#6-sigv4-hmac-and-chunked-upload-chain)
7. [Manifest body AEAD with key-bound AAD](#7-manifest-body-aead-with-key-bound-aad)
8. [DEK wrap envelope (local file / KMS / Vault)](#8-dek-wrap-envelope-local-file--kms--vault)
9. [Randomness and nonce sources](#9-randomness-and-nonce-sources)
10. [Known limitations, PQC plan, and explicit out-of-scope items](#10-known-limitations-pqc-plan-and-explicit-out-of-scope-items)
11. [Suggested attack ideas](#11-suggested-attack-ideas)

---

## 1. Scope and review goals

Concrete claims we need the auditor to confirm or refute:

| # | Claim | Code primary |
|---|---|---|
| C1 | Per-chunk XChaCha20-Poly1305 framing is misuse-resistant: nonces are unique within a `(DEK, mode)` pair, and a chunk lifted from one object cannot decrypt inside another. | `encryption/client_sdk/sdk.go:118-149`, `encryption/client_sdk/sdk.go:183-201`, `encryption/client_sdk/sdk.go:202-213` |
| C2 | The chunk-AAD binding (`tenant_id|bucket|object_key_hash|version_id || "|" || u64BE(idx)`) defeats both cross-object replay and within-object reorder. | `encryption/client_sdk/sdk.go:7-42`, `encryption/client_sdk/sdk.go:183-201` |
| C3 | `DeriveConvergentDEK(contentHash, tenantID)` is cross-tenant unreachable: the HKDF salt binds the derivation to `tenantID` so distinct tenants always derive distinct DEKs. | `encryption/client_sdk/keygen.go:36-66` |
| C4 | The convergent-nonce mode never reuses a `(key, nonce)` pair across distinct plaintexts inside the same tenant. | `encryption/client_sdk/sdk.go:202-213` |
| C5 | The CMK wrap envelope (`xchacha20-poly1305-wrap-v1`) binds the wrapped DEK to the specific CMK URI and rejects open under a different CMK. | `encryption/client_sdk/wrap.go:63-122` |
| C6 | The manifest BodyEncryptor binds ciphertext to `(tenant_id, bucket, object_key_hash)` so a row swapped between primary-key columns will fail to open. | `metadata/manifest_store/postgres/body_encryptor.go:66-86`, `metadata/manifest_store/postgres/body_encryptor.go:121-155` |
| C7 | SigV4 chunked-upload signatures form a hash chain rooted in the seed signature; a captured `(chunkData, signature)` cannot be spliced into a different upload. | `internal/auth/authenticator.go:484-505`, `internal/auth/authenticator.go:626-631` |
| C8 | All key material lives in process memory only as long as needed; multipart DEKs are scrubbed at session end (see PR #74 — outside this package's scope but relevant to the threat model). | `api/s3compat/multipart/...` |

## 2. Cryptographic primitives in use

| Primitive | Implementation | Used for |
|---|---|---|
| XChaCha20-Poly1305 (RFC 8439 + RFC draft `xchacha`) | `golang.org/x/crypto/chacha20poly1305` (Go upstream) | Per-chunk object AEAD, CMK wrap, manifest body seal |
| HKDF-SHA256 (RFC 5869) | `golang.org/x/crypto/hkdf` | Convergent DEK derivation, convergent nonce derivation |
| HMAC-SHA256 (RFC 2104) | stdlib `crypto/hmac` + `crypto/sha256` | SigV4 signing key derivation, per-chunk SigV4 signatures |
| SHA-256 | stdlib `crypto/sha256` | SigV4 canonical request hash, DEK key-ID derivation, CMK content-hash inputs |
| crypto/rand (system CSPRNG) | stdlib `crypto/rand` | Per-object DEK generation, per-chunk random nonces (non-convergent mode), CMK wrap nonces |
| AWS KMS (Phase 3) | `github.com/aws/aws-sdk-go-v2/service/kms` | DEK wrap when `cmk://kms/...` is configured |
| HashiCorp Vault Transit (Phase 3) | direct REST client in `encryption/client_sdk/vault_wrapper.go` | DEK wrap when `cmk://vault/...` is configured |

**No bespoke cryptography.** Every primitive is taken from the
upstream `golang.org/x/crypto` or stdlib packages. We do not hand-
roll AEAD, KDF, HMAC, or signature primitives. The audit should
verify that observation by grepping for `Sum256`, `cipher.NewCBC`,
`aes.NewCipher`, etc. outside of test files; any hit must be
justified.

## 3. Object body AEAD: per-chunk framing

The SDK seals plaintext into fixed-size chunks
(`DefaultChunkSize = 16 MiB`) so that a range GET can fetch and
decrypt only the relevant chunks. The on-disk frame is
documented in the package doc:

```
| 24-byte nonce | 4-byte BE ciphertext length | ciphertext || 16-byte Poly1305 tag |
```

- Nonce size: `chacha20poly1305.NonceSizeX = 24`.
- Tag size: `chacha20poly1305.Overhead = 16`.
- Frame overhead: 28 bytes / chunk plus the 16-byte AEAD tag baked
  into the ciphertext (so 44 bytes of overhead per non-empty
  chunk). The expected total is exposed via `EncryptedSize`
  (`encryption/client_sdk/sdk.go:291-323`) which the gateway uses
  to advertise a known `Content-Length` to the backend ahead of
  the streaming PUT.

### 3.1 What the auditor should verify

- The AEAD construction call in `EncryptObject`
  (`encryption/client_sdk/sdk.go:118-149`):
  ```go
  aead, err := chacha20poly1305.NewX(dek)
  ```
  is the standard `NewX` constructor; we do not invoke the lower-
  level `chacha20.NewUnauthenticatedCipher` anywhere on a body
  path. (Grep-confirmed.)
- The DEK length precondition is checked. `chacha20poly1305.NewX`
  itself returns an error for non-32-byte keys. `WrapDEK`
  enforces the same precondition (`wrap.go:64-66`).
- The frame nonce is **stored in the frame**, not derived at
  decrypt time. This means rolling forward the convergent-nonce
  flag (which changes the derivation function on the encrypt
  side) does NOT require a parallel branch on the decrypt side
  — `DecryptObject` (`encryption/client_sdk/sdk.go:151-168`)
  reads the nonce off the wire either way.
- `EncryptedSize` is the inverse of the frame layout and has
  explicit typed errors for overflow, negative input, and
  non-positive chunk size (`encryption/client_sdk/sdk.go:236-289`).
  An auditor should construct an adversarial `plaintextLen` close
  to `math.MaxInt64` and confirm `ErrEncryptedSizeOverflow` is
  returned, not a silent wrap.

### 3.2 Range reads and partial frames

The decryptor walks frames sequentially. Range GET in
`api/s3compat/` translates a `Range: bytes=A-B` header to a
chunk-aligned starting frame plus a discard window inside that
frame. The auditor should:

- Verify that a malicious `Range` header that overlaps a frame
  boundary by 1 byte cannot trick the decryptor into authenticating
  a partial frame. (The AEAD `Open` call is at the frame level;
  trimming the start of plaintext after Open is what produces the
  partial-byte response.)
- Verify that a tampered ciphertext byte inside a frame produces
  an AEAD `Open` failure on that frame, surfaced as an HTTP 500
  to the client and an alert on the gateway (the data-path
  handler in `api/s3compat/handler.go` does not silently fall
  back to a degraded mode on an Open failure).

## 4. Per-chunk AAD construction

The package doc declares the AAD as:

```
AAD = ChunkAAD || "|" || big-endian uint64(chunk_index)
```

The recommended `ChunkAAD` payload is the canonical, pipe-
separated tuple
`tenant_id|bucket|object_key_hash|version_id` where
`object_key_hash` is a SHA-256 over the object key.

Implementation: `chunkAADBytes`
(`encryption/client_sdk/sdk.go:183-201`):
```go
aad := make([]byte, 0, len(chunkAAD)+1+8)
aad = append(aad, chunkAAD...)
aad = append(aad, '|')
var idx [8]byte
binary.BigEndian.PutUint64(idx[:], chunkIndex)
aad = append(aad, idx[:]...)
return aad
```

### 4.1 What the auditor should verify

- The AAD is included in the AEAD call (not just stored on the
  frame). Both `encryptReader.nextFrame` and
  `decryptReader.nextFrame` call `aead.Seal(... , aad)` /
  `aead.Open(... , aad)` with the result of `chunkAADBytes`.
- When `Options.ChunkAAD` is empty (zero-length slice) the SDK
  falls back to `AAD = nil` for both Seal and Open. This is a
  **deliberate backward-compatibility path** for objects sealed
  before the AAD field existed. The auditor should confirm:
  - The fall-back applies symmetrically on both sides (Seal and
    Open get the same nil AAD).
  - The legacy path is gated only on `len(opts.ChunkAAD) == 0`,
    not on any tenant configuration — meaning new writes from
    the gateway always pass a non-empty AAD if the gateway is
    invoked with it.
- The chunk index is big-endian uint64. Within an object, every
  chunk has a distinct index — so two chunks at positions `i`
  and `j > i` cannot have their ciphertexts swapped without
  failing AEAD verification.
- The pipe separator before the index is unambiguous given the
  ChunkAAD format. Auditor should confirm that the
  ChunkAAD itself cannot legitimately contain a trailing pipe
  followed by 8 bytes that would shift the index parsing on a
  hypothetical re-parser. (We do not re-parse the AAD on the
  decrypt side; the index is independently computed from the
  decryptor's chunk counter. So the only requirement is that
  the *encoder* and *decoder* both produce the same byte string
  for a given `(chunkAAD, chunkIndex)` — and they do, byte-for-
  byte.)

### 4.2 What the auditor should attempt

- **Cross-object frame splice**: take a ciphertext frame from
  object A (sealed with `tenant|bucketA|hashA`) and splice it
  into the chunk-0 position of object B (sealed with
  `tenant|bucketB|hashB`). The AAD differs in the `bucket`
  field; the AEAD `Open` must fail.
- **Within-object reorder**: swap chunks 5 and 7 inside the same
  object. The AAD differs in the `u64BE(chunkIndex)` suffix;
  the AEAD `Open` must fail.
- **Index aliasing**: a hostile encoder claims chunk index 0 for
  every chunk (e.g. a corrupted on-disk format). The decryptor's
  index is its own counter, so the AAD would mismatch from
  chunk 1 onward and AEAD `Open` would fail.

## 5. Convergent encryption — HKDF DEK derivation and deterministic nonces

### 5.1 DEK derivation

`encryption/client_sdk/keygen.go:36-66`:
```go
const ConvergentDEKInfo = "zkof-convergent-dek-v1"

func DeriveConvergentDEK(contentHash []byte, tenantID string) (DataEncryptionKey, error) {
    if len(contentHash) == 0 { /* required */ }
    if tenantID == ""        { /* required */ }
    salt := []byte(tenantID)
    info := []byte(ConvergentDEKInfo)
    r := hkdf.New(sha256.New, contentHash, salt, info)
    dek := make([]byte, chacha20poly1305.KeySize)
    if _, err := io.ReadFull(r, dek); err != nil { /* ... */ }
    return DataEncryptionKey(dek), nil
}
```

### 5.2 Why this is cross-tenant safe

HKDF (RFC 5869) is a two-step `Extract` then `Expand`. The
`salt` argument is the input to `Extract`. Because we pass
`tenantID` as the salt, two tenants computing convergent DEKs for
the **same content hash** still derive distinct DEKs. This is
not a security claim that relies on `contentHash` being secret —
it relies on HKDF's standard guarantee that distinct salts
produce independent key material.

The auditor should verify:

- The salt is `[]byte(tenantID)` and is non-empty (`tenantID == ""`
  is rejected at the top of the function).
- The IKM is `contentHash`. We treat it as IKM because that is
  what RFC 5869 wants: the IKM does not need to be uniformly
  random as long as HKDF's salt is suitably distinct. Auditor
  should confirm this is a defensible reading of the RFC. (Our
  position: yes; `contentHash` is the BLAKE3 / SHA-256 of the
  plaintext, which is high-entropy for any real object, and the
  tenant-keyed salt is the cross-tenant separator regardless.)
- The `info` is versioned (`zkof-convergent-dek-v1`). A future
  rotation can derive a disjoint key space by bumping `v1` →
  `v2` without ever colliding with existing manifests.

### 5.3 Why intra-tenant dedup loses forward secrecy (explicit)

We document this trade-off in `keygen.go:42-46`:

> stored ciphertext loses forward secrecy for the deduped object —
> a future leak of the DEK reveals every historical and future
> copy under the same (tenant, content_hash) key.

The auditor should agree that this trade-off is correctly
constrained: the leak is **intra-tenant only**, because the salt
isolates tenants from one another. Cross-tenant leak via
convergent dedup is not possible.

### 5.4 Convergent nonce derivation

When `Options.ConvergentNonce` is set, per-chunk nonces are also
deterministic — derived from the DEK and the chunk index via
HKDF:

`encryption/client_sdk/sdk.go:202-213`:
```go
const ConvergentNonceInfo = "zkof-nonce-v1"

func deriveConvergentNonce(dek DataEncryptionKey, chunkIndex uint64, nonceSize int) ([]byte, error) {
    var idxBytes [8]byte
    binary.BigEndian.PutUint64(idxBytes[:], chunkIndex)
    info := append([]byte(ConvergentNonceInfo), idxBytes[:]...)
    r := hkdf.New(sha256.New, dek, nil, info)
    nonce := make([]byte, nonceSize)
    if _, err := io.ReadFull(r, nonce); err != nil { /* ... */ }
    return nonce, nil
}
```

### 5.5 Why this does not violate the AEAD nonce-uniqueness requirement

XChaCha20-Poly1305's 24-byte nonce makes random nonces safe
indefinitely against birthday collisions for any realistic key.
Convergent mode replaces random nonces with deterministic ones,
which **must** be unique per `(key, plaintext)`. The construction
ensures that:

- Within a single object, two chunks at indices `i ≠ j` produce
  distinct nonces because HKDF info carries the index.
- Across two identical objects (same content → same convergent
  DEK), the nonces ARE identical by design — but so is the
  plaintext, so the AEAD nonce reuse is "key-and-plaintext"
  identical, which is the only kind of reuse XChaCha20-Poly1305
  permits (because the ciphertext is also bitwise identical, so
  no information is leaked beyond the dedup-oracle the convergent
  scheme deliberately provides).
- Across two distinct objects with distinct content within the
  same tenant, the convergent DEKs differ (different `contentHash`
  → different HKDF Extract output), so the nonces are computed
  under different keys and the (key, nonce) reuse cannot occur.

The auditor should verify the above three properties hold in the
code and write up any case where they do not.

## 6. SigV4 HMAC and chunked-upload chain

### 6.1 SigV4 signing key derivation

`internal/auth/authenticator.go:626-631`:
```go
func deriveSigningKey(secretKey, date, region, service string) []byte {
    kDate    := hmacSHA256([]byte("AWS4"+secretKey), date)
    kRegion  := hmacSHA256(kDate, region)
    kService := hmacSHA256(kRegion, service)
    return hmacSHA256(kService, "aws4_request")
}
```

This is the standard AWS SigV4 derivation. The auditor should
verify it matches the AWS reference vector. The `hmacSHA256`
helper at line 744-747 is the textbook two-arg
`hmac.New(sha256.New, key); hmac.Write(data); hmac.Sum(nil)`.

### 6.2 Per-chunk signature chain (aws-chunked PUT)

`internal/auth/authenticator.go:484-505`:
```go
func VerifyChunkSignature(prevSig string, chunkData []byte, signingKey []byte, timestamp, scope string) (string, error) {
    stringToSign := strings.Join([]string{
        "AWS4-HMAC-SHA256-PAYLOAD",
        timestamp,
        scope,
        prevSig,
        hex.EncodeToString(sha256.Sum256(nil)[:]),       // empty body sha (per AWS spec)
        hex.EncodeToString(sha256.Sum256(chunkData)[:]),
    }, "\n")
    sig := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
    return sig, nil
}
```

The handler in `api/s3compat/` calls this for every chunk, passing
the previous signature as `prevSig`. The seed signature is the
header signature computed at request entry by
`HeaderV4Strategy.Authenticate`.

### 6.3 What the auditor should verify

- Every chunk's verification uses the **caller-supplied
  signingKey**, which is derived once at request entry from
  `(secretKey, date, region, service)` and held in memory for
  the duration of the upload. The auditor should confirm there
  is no path that re-derives the signing key from a per-chunk
  header (which would let an attacker substitute a different
  date / region).
- The chain anchor is the seed signature from the Authorization
  header. A spliced chunk from a different upload would have a
  different anchor → different chain → mismatch.
- The `chunkData` hash is `SHA-256` over the chunk bytes, not
  over the chunk header. (Auditor should confirm by walking the
  handler.)
- The `"AWS4-HMAC-SHA256-PAYLOAD"` literal matches AWS's
  documented algorithm tag. Drift would silently produce
  incompatible signatures with `aws-cli --use-aws-chunked`.

## 7. Manifest body AEAD with key-bound AAD

`metadata/manifest_store/postgres/body_encryptor.go`:

- Algorithm: XChaCha20-Poly1305 (same as the body AEAD).
- Key: 32 bytes loaded from the gateway's CMK file or KMS at boot.
- Frame: `[24-byte nonce][ciphertext || 16-byte tag]`. No length
  prefix because the ciphertext is stored in a single Postgres
  `BYTEA` column whose length is implicit.
- AAD: `bodyContextAAD(BodyContext)` =
  `tenant_id || "|" || bucket || "|" || object_key_hash` when
  `BodyContext` is non-zero; nil otherwise (legacy compat).

### 7.1 What the auditor should verify

- The store's `Put` path always passes a non-zero `BodyContext`
  (`metadata/manifest_store/postgres/store.go:88-104`). Auditor
  should grep for any other writer that constructs a row directly
  with `INSERT INTO manifests` — there should be none outside the
  `Store` type.
- The store's `Get` and `List` paths construct the same
  `BodyContext` from the manifest key and pass it on Decrypt
  (`metadata/manifest_store/postgres/store.go:146,235`). A row
  whose primary-key columns have been swapped between tenants
  produces a different `BodyContext` and the AEAD `Open` fails.
- The encryptor accepts ciphertext sealed with an empty
  `BodyContext` (legacy rows). The auditor should verify this is
  intentional and not a downgrade path: a row whose ciphertext
  was sealed with a non-empty AAD CANNOT be replayed as a legacy
  row, because the legacy decryption attempt would fail the AEAD
  check and surface as an error to the caller.

## 8. DEK wrap envelope (local file / KMS / Vault)

### 8.1 Local-file wrap

`encryption/client_sdk/wrap.go:63-93`:
```go
aead.Seal(nil, nonce, dek, []byte(cmk.URI))
```

- The CMK URI is used as the AEAD AAD. A wrapped DEK from
  `cmk://local/path/A` will not open under `cmk://local/path/B`
  even if both files contain the same 32 bytes.
- The nonce is freshly sampled from `crypto/rand` (via
  `randReader`, which is the system CSPRNG outside tests).
- The serialized `WrappedDEK.WrappedKey` is `nonce || ciphertext_with_tag`.
- A separate `dekKeyID` derives a 16-byte SHA-256-based ID that
  the manifest stores so a reader can locate the right wrapped
  material even after CMK rotation (`encryption/client_sdk/wrap.go:124-156`).

### 8.2 AWS KMS wrap

`encryption/client_sdk/kms_wrapper.go:66-130`:

- Uses `kms.Encrypt` / `kms.Decrypt` with `EncryptionContext`
  populated from the CMK URI (the auditor should confirm the
  EncryptionContext map is non-empty and identical on both
  sides — KMS treats EncryptionContext as additional AAD).
- Does NOT roll its own AEAD; the wrapping is entirely inside
  AWS KMS using whatever AEAD the KMS key is configured for
  (typically AES-GCM under the hood, HSM-resident for HSM-class
  keys).

### 8.3 Vault Transit wrap

`encryption/client_sdk/vault_wrapper.go:93-160`:

- Uses Vault's `transit/encrypt/<key>` and `transit/decrypt/<key>`
  endpoints. The CMK URI maps to the Vault key name via
  `normalizeVaultKeyName`.
- Same property as KMS: we do not re-implement an AEAD; we hand
  the DEK to Vault and store the returned ciphertext blob.

### 8.4 What the auditor should verify

- All three wrappers share the same `Wrapper` interface and the
  same `WrappedDEK` shape, so swapping between them is a
  configuration-only change and does NOT affect the on-disk
  manifest format.
- The local-file wrap's AAD is the **CMK URI**, not a CMK
  fingerprint. This means rotating the file contents without
  changing the URI would invalidate every wrapped DEK that was
  sealed against the old contents. (Documented behaviour;
  auditor should verify rotation procedure in
  `docs/runbooks/` accounts for this — the current rotation
  story is "rewrap every DEK with the new key", which the
  package does not automate.)
- There is no path that constructs a `WrappedDEK` without an
  explicit `WrapAlgorithm` field, and `UnwrapDEK` rejects an
  unknown algorithm string up-front (`wrap.go:97-99`). This is
  the version-bump escape hatch for future algorithm rotation.

## 9. Randomness and nonce sources

| Site | Source | Length | Notes |
|---|---|---|---|
| Per-object DEK | `randReader` (= `crypto/rand`) | 32 B | `keygen.go:24-31` |
| Per-chunk nonce (random mode) | `crypto/rand` inside `encryptReader.nextFrame` | 24 B | One per frame |
| Per-chunk nonce (convergent mode) | HKDF-SHA256 over `(DEK, chunkIndex)` | 24 B | See §5.4 |
| CMK-wrap nonce | `randReader` | 24 B | `wrap.go:74-78` |
| BodyEncryptor nonce | `crypto/rand` | 24 B | `body_encryptor.go:121-138` |
| Convergent DEK | HKDF-SHA256 over `(contentHash, tenantID)` | 32 B | `keygen.go:36-66` |
| `randReader` in tests | overridable | — | Production code path is unaffected (`keygen.go:14-15`) |

### 9.1 What the auditor should verify

- `randReader` is `io.Reader = rand.Reader` at package init and
  is only ever reassigned in test files. Grep-confirmed:
  ```
  var randReader io.Reader = rand.Reader
  ```
  is the only assignment outside `_test.go`.
- No path uses `math/rand` for cryptographic purposes. (Grep-
  confirmed across the repo.)
- The HKDF derivations use SHA-256, not SHA-1. Grep-confirmed:
  `hkdf.New(sha256.New, ...)`.

## 10. Known limitations, PQC plan, and explicit out-of-scope items

### 10.1 Known limitations

- **No formal proof.** We rely on the standard security claims
  of XChaCha20-Poly1305 and HMAC-SHA256 as composed by the Go
  standard library. We do not provide a machine-checked proof.
- **No quantum resistance.** XChaCha20-Poly1305 is broken by a
  practical quantum computer (Grover gives a ~2^128 attack
  against the 256-bit key, which is still considered safe for
  the next decade per NIST, but the migration plan is below).
  HKDF-SHA256 is also Grover-affected for the same reason.
- **CMK material is not split.** A KMS / Vault compromise gives
  the attacker every tenant's wrapped DEK universe. We do not
  attempt threshold or split-key construction. This is the
  documented out-of-scope item from `threat-model.md` §4.

### 10.2 Post-quantum migration plan (high-level, not in-scope for this audit)

When the project migrates to PQC primitives:

- AEAD: AES-256-GCM-SIV is the most likely successor (still
  symmetric, still safe under Grover for 256-bit keys, less
  risky than XChaCha for nonce reuse).
- Key transport / wrap: ML-KEM (Kyber, NIST FIPS 203) for the
  KMS path; Vault Transit will adopt PQC primitives on the
  Vault side when HashiCorp ships them. The `WrapAlgorithm`
  field on `WrappedDEK` is already versioned for this transition.
- Signatures: ML-DSA (Dilithium, NIST FIPS 204) for any future
  signing surface. SigV4 itself stays as-is — it is HMAC, not
  signing.

The auditor is welcome to comment on this plan but should not
treat it as in-scope for the current package.

### 10.3 Explicit out-of-scope

- **Side-channel resistance** of the underlying `golang.org/x/crypto`
  primitives. We trust the upstream Go cryptography team's
  hardening; we do not have an independent timing-analysis story.
- **Compromise of the gateway process memory** (live DEKs, the
  per-multipart-session DEK before scrub). The threat model
  treats this as operationally mitigated — the audit should
  flag any path that **persists** a plaintext DEK outside the
  process address space, but in-process memory disclosure is
  not in scope.

## 11. Suggested attack ideas

Non-exhaustive list to seed the audit's threat-modelling session:

1. **AAD downgrade**: a tenant tries to write a new object with
   `Options.ChunkAAD == nil` to land on the legacy compat path.
   Confirm the gateway always passes a non-empty AAD on every
   new write (grep callsites in `api/s3compat/encryption_pipeline.go`).
2. **Cross-CMK envelope lift**: take a `WrappedDEK` sealed for
   `cmk://kms/key-A` and present it on a manifest whose CMK
   reference is `cmk://kms/key-B`. The wrap's AAD (CMK URI for
   local-file; EncryptionContext for KMS / Vault) must reject.
3. **HKDF salt collision**: confirm that no path computes a
   convergent DEK with an empty `tenantID` (the function rejects
   it; verify no caller swallows the error and falls back to a
   "global" derivation).
4. **Convergent dedup oracle abuse**: within a tenant, a
   confirmed-cleartext attack (the attacker can write
   plaintext and observe whether dedup happened) DOES reveal
   that the plaintext exists in the tenant's data. This is
   the documented price of intra-tenant convergent dedup
   (§5.3); confirm the attack stops at the tenant boundary.
5. **Chunked-upload chain break**: capture chunks 0..3 of a
   legitimate upload, splice chunk 2 from a different upload
   with a different `signingKey`. The chain must break at
   chunk 2's verification.
6. **Body-AAD downgrade**: a Postgres admin rewrites a sealed
   row to look like a legacy unsealed row. The `Decrypt` path
   first attempts the modern AAD-bound open; only on failure
   does it fall back to legacy. The auditor should confirm the
   fallback path is gated on the row's stored format marker,
   not on a content check that an attacker can spoof. (Current
   code: the wrapper always tries the modern path first; legacy
   compat is opt-in via the empty `BodyContext` test path. The
   auditor should verify this is not user-reachable in
   production.)
7. **EncryptedSize overflow**: send a chunked PUT with a hostile
   `Content-Length: 9223372036854775000`. Confirm
   `ErrEncryptedSizeOverflow` is returned and the request is
   rejected before any bytes hit the backend.
8. **Nonce-reuse under convergent mode**: write two objects with
   identical `contentHash` and DIFFERENT `Options.ChunkAAD`
   under the same tenant. They share a DEK (same convergent
   derivation) and share per-chunk nonces (deterministic from
   DEK + chunkIndex). The AAD differs. AEAD ciphertext will
   differ; both can decrypt under their respective AADs. There
   is no nonce-reuse vulnerability because the AAD is part of
   the AEAD authenticator, not the keystream. Confirm this
   reasoning matches `chacha20poly1305.NewX`'s documented
   behaviour.
