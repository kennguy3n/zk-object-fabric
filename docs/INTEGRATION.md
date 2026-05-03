# Integration Guide — Deduplication for External Applications

This is the primary document external apps (KChat, kmail, zk-drive,
Kapp Business Suite, or any third-party S3 client) read to integrate
with ZK Object Fabric's deduplication.

ZK Object Fabric supports **intra-tenant deduplication only**. Two
objects are eligible to share a stored piece if and only if they
belong to the same tenant. **Cross-tenant deduplication is
permanently excluded** — it is a privacy side channel (one tenant
could probe to learn whether another tenant already stores a given
file) and is incompatible with the fabric's zero-knowledge posture.
See [PROPOSAL.md](PROPOSAL.md) §3.8 and the "Deduplication vs ZK
conflict" entry in §A of PROPOSAL.md for the underlying constraint.

## 1. Prerequisites

Before integrating, you need:

- A ZK Object Fabric **tenant** with HMAC credentials (access key +
  secret key). For B2C self-service, signup hands these out; for
  B2B / BYOC / dedicated cells, your operator provisions them via
  the console API. Onboarding mechanics are in
  [docs/runbooks/tenant-setup.md](runbooks/tenant-setup.md).
- An **S3-compatible client** — AWS SDK, `boto3`, MinIO client,
  `@aws-sdk/client-s3`, or any tool that speaks SigV4. The fabric's
  S3 data plane is on `:8080`; see [README.md](../README.md#quick-start-docker)
  for the demo setup.
- A **bucket dedup policy** enabled via the console API at `:8081`.
  Without an enabled policy the gateway behaves exactly like a
  standard S3 backend — no content addressing, no refcount, no
  shared pieces. See section 10 below for the API.
- For Pattern C (client-side convergent encryption) you also need
  the Go client SDK at `encryption/client_sdk/` or an equivalent
  reimplementation in your application's language.

## 2. Decision tree

Answer these three questions in order to pick the right integration
pattern:

1. **Will the same file be read by many users in the same group, but
   uploaded only once?** (Examples: a KChat 1000-member group share,
   an announcement attachment, a shared document.)
   → **Pattern A: Single Upload, N Readers**. No dedup needed; one
   PUT plus N GETs against the same object key. Works with every
   encryption mode.

2. **Will the same file be uploaded *independently* by multiple
   users (so the gateway sees N separate PUTs of the same bytes)?**
   - If your application uses **Strict ZK** (`client_side`
     encryption) → **Pattern C: Client-Side Convergent Encryption**.
   - If your application uses **`managed`** or
     **`public_distribution`** encryption → **Pattern B: Gateway
     Convergent Encryption**.

3. **No duplication expected** (every PUT is genuinely unique
   plaintext)? → Use the standard S3 API with no dedup policy.
   Random per-object DEKs preserve full forward secrecy.

```
                            ┌──────────────────────────────┐
                            │  Same file read by many       │
                            │  users in same group?         │
                            └─────────────┬─────────────────┘
                                          │ yes
                                          ▼
                                   Pattern A (no dedup)
                                          ▲
                                          │ no
                            ┌─────────────┴─────────────────┐
                            │  Same file uploaded           │
                            │  independently by many users? │
                            └─────────────┬─────────────────┘
                              yes │       │ no
                                  ▼       ▼
                  ┌───────────────┴──┐   Standard S3, no dedup
                  │  Strict ZK       │
                  │  (client_side)?  │
                  └────┬─────────┬───┘
                  yes  │         │ no (managed / public_distribution)
                       ▼         ▼
                 Pattern C    Pattern B
```

## 3. Pattern A: Single Upload, N Readers

**Use case.** A KChat 1000-member group shares one image: 1 PUT, 1000
GETs. No deduplication is needed because there is only ever one
stored object — every reader fetches the same key.

**Encryption modes.** Works identically with `client_side`,
`managed`, and `public_distribution`. The fabric does nothing
special.

**MLS-aware applications** (KChat, kmail Confidential Send /
Zero-Access Vault, any RFC 9420 group):

1. The sender generates a **content encryption key** (CEK) that is
   **independent of MLS epoch secrets** — typically a fresh random
   key, or a convergent key derived from the plaintext (see
   Pattern C). The CEK MUST NOT be derived from the
   `exporter_secret`, the application key schedule, or any other
   epoch-bound MLS secret.
2. The sender encrypts the file with that CEK and PUTs the
   ciphertext at some object key (e.g. `groups/{group_id}/files/{uuid}`).
3. The sender distributes the **CEK + object reference** as an MLS
   application message. The MLS channel provides forward secrecy
   and post-compromise security for the *delivery* of the CEK; the
   stored file's lifetime is independent of the MLS epoch.
4. Recipients receive the MLS application message, GET the same
   object, and decrypt with the CEK from the message.

**Why CEK independence matters.** MLS epoch secrets rotate on every
membership change (member add, member remove, key update). If the
file CEK were derived from an epoch secret, the file would become
unreadable to *current* members the moment the group rolls to a new
epoch. Always derive the file CEK from content (convergent) or
random bytes — never from `exporter_secret`, `welcome_secret`, or
any other epoch-bound output of the MLS key schedule (RFC 9420 §8).

**Python / boto3 example** (sender uploads once, recipients GET the
same key):

```python
import boto3
import os
import secrets

s3 = boto3.client(
    "s3",
    endpoint_url="https://fabric.example.com:8080",
    aws_access_key_id=os.environ["FABRIC_ACCESS_KEY"],
    aws_secret_access_key=os.environ["FABRIC_SECRET_KEY"],
    region_name="auto",
)

# --- Sender ---
group_id = "1f2e3d4c-..."
object_key = f"groups/{group_id}/files/{secrets.token_hex(16)}"

# CEK is independent of MLS epoch secrets. It can be random or
# convergent; here we use random.
cek = secrets.token_bytes(32)
ciphertext = encrypt_with_cek(plaintext, cek)  # XChaCha20-Poly1305 etc.

s3.put_object(Bucket="kchat-files", Key=object_key, Body=ciphertext)

# Distribute (cek, object_key) over MLS as an application message.
mls_send_application_message(group, {"cek": cek, "key": object_key})

# --- Recipients (1000 of them) ---
msg = mls_receive_application_message(group)
resp = s3.get_object(Bucket="kchat-files", Key=msg["key"])
plaintext = decrypt_with_cek(resp["Body"].read(), msg["cek"])
```

No dedup policy is required for Pattern A — the gateway only stores
the object once because the application only PUTs it once.

## 4. Pattern B: Gateway Convergent Encryption

**Use case.** A B2C community with viral media (the same meme
re-uploaded by hundreds of users), or a B2B organization where many
employees independently upload the same company document. The
gateway sees N PUTs of identical plaintext and collapses them into a
single stored piece.

**Encryption modes.** `managed` or `public_distribution` only.
Pattern B is incompatible with `client_side` because the gateway
must see plaintext to compute the content hash and derive the
convergent DEK. If your tenant requires Strict ZK, use Pattern C
instead.

**Gateway flow** (transparent to the client):

1. Client PUTs plaintext normally (standard S3 `PutObject`).
2. Gateway computes `content_hash = BLAKE3(plaintext)`.
3. Gateway derives the convergent DEK:
   `DEK = HKDF-SHA256(content_hash, salt = tenant_id, info = "zkof-convergent-dek-v1")`.
   The salt is the **tenant ID** so identical plaintexts uploaded by
   different tenants produce *different* DEKs and hash to different
   ciphertext — this is the cryptographic enforcement of the
   intra-tenant boundary.
4. Gateway encrypts with **deterministic per-chunk nonces** derived
   via HKDF: `nonce = HKDF-SHA256(secret = DEK, salt = nil,
   info = "zkof-nonce-v1" || u64_BE(chunk_index))[:24]`, matching
   the SDK's `ConvergentNonce` path, so the ciphertext is itself a
   deterministic function of the plaintext + tenant.
5. Gateway computes `ciphertext_hash = BLAKE3(ciphertext)`.
6. Gateway looks up `ContentIndex(tenant_id, ciphertext_hash)`.
   - **Hit**: the new manifest's `Piece` points at the existing
     piece's locator, the existing piece's refcount is incremented,
     and **no bytes are written to the backend**. The PUT returns
     the same `ETag` and `Content-Length` it would have returned
     without dedup.
   - **Miss**: the gateway writes the piece to the backend, registers
     `(tenant_id, ciphertext_hash) → piece_id` in `ContentIndex`,
     and the new manifest points at the freshly-written piece.

**Client-side changes required: zero.** Clients use the standard S3
API. Dedup is fully transparent.

**Python / boto3 example** (two users independently upload the same
file; the second is deduped):

```python
import boto3, os

s3 = boto3.client(
    "s3",
    endpoint_url="https://fabric.example.com:8080",
    aws_access_key_id=os.environ["FABRIC_ACCESS_KEY"],
    aws_secret_access_key=os.environ["FABRIC_SECRET_KEY"],
    region_name="auto",
)

# Both calls upload the same plaintext under different keys.
with open("quarterly-report.pdf", "rb") as f:
    body = f.read()

# Alice uploads first — gateway writes the piece, registers ContentIndex.
s3.put_object(Bucket="company-docs", Key="alice/q1.pdf", Body=body)

# Bob uploads later — gateway sees the same content_hash, finds an
# existing ContentIndex entry, increments refcount, writes ZERO new
# bytes to the backend.
s3.put_object(Bucket="company-docs", Key="bob/q1.pdf", Body=body)

# Both Alice and Bob can GET their key normally — the manifests
# point at the same physical piece, but the API surface is unchanged.
```

The dedup policy must be enabled on the bucket (`enabled: true`,
`scope: "intra_tenant"`) — see section 10. Without it, both PUTs
write independent ciphertext with random DEKs and consume the full
storage twice.

## 5. Pattern C: Client-Side Convergent Encryption (Strict ZK)

**Use case.** End-to-end encrypted applications (KChat with MLS,
kmail Zero-Access Vault, any tenant in `client_side` mode) where the
same file may be uploaded independently to multiple groups, but the
gateway must *never* see plaintext.

**Encryption mode.** `client_side` with the convergent option
enabled. The gateway sees only ciphertext; deduplication is keyed
off `BLAKE3(ciphertext)`.

**Client SDK changes required:**

1. Use `DeriveConvergentDEK(plaintext, tenantID)` instead of
   `GenerateDEK()`.
   Derivation: `DEK = HKDF-SHA256(BLAKE3(plaintext), salt = tenant_id, info = "zkof-convergent-dek-v1")`.
   This is the same construction the gateway uses in Pattern B, so
   the *same plaintext* in the same tenant produces the *same DEK*
   regardless of who derives it.
2. Set `Options{ConvergentNonce: true}` so the SDK derives per-chunk
   nonces deterministically:
   `nonce = HKDF-SHA256(secret = DEK, salt = nil,
   info = "zkof-nonce-v1" || u64_BE(chunk_index))[:24]` instead of
   sampling from `crypto/rand`.

With both options the SDK produces a **bit-identical ciphertext
stream** for any (plaintext, tenant_id) pair. Two clients in the
same tenant uploading the same plaintext will write the same
ciphertext bytes; the gateway sees this collision and deduplicates.

**Gateway flow.** The gateway never sees plaintext:

1. Receives ciphertext (the SDK has already encrypted with the
   convergent DEK and deterministic nonces).
2. Computes `ciphertext_hash = BLAKE3(ciphertext_bytes)`.
3. Looks up `ContentIndex(tenant_id, ciphertext_hash)`.
4. Hit → refcount++, no backend write. Miss → write the piece,
   register the index entry. Same flow as Pattern B from step 5
   onward.

**Go example** using the client SDK with convergent options:

```go
package main

import (
    "bytes"
    "context"
    "io"
    "os"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/service/s3"

    "github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
)

func uploadConvergent(ctx context.Context, s3c *s3.Client,
    bucket, key, tenantID string, plaintext []byte,
) error {
    // 1. Derive the convergent DEK from plaintext + tenant ID.
    dek, err := client_sdk.DeriveConvergentDEK(plaintext, tenantID)
    if err != nil {
        return err
    }

    // 2. Encrypt with deterministic per-chunk nonces.
    ct, err := client_sdk.EncryptObject(
        bytes.NewReader(plaintext),
        dek,
        client_sdk.Options{ConvergentNonce: true},
    )
    if err != nil {
        return err
    }

    body, err := io.ReadAll(ct)
    if err != nil {
        return err
    }

    // 3. PUT the ciphertext. The gateway sees only ciphertext;
    //    if another client in the same tenant already uploaded
    //    the same plaintext, the gateway dedups via ContentIndex
    //    and refcount++ — no extra bytes hit the backend.
    _, err = s3c.PutObject(ctx, &s3.PutObjectInput{
        Bucket: aws.String(bucket),
        Key:    aws.String(key),
        Body:   bytes.NewReader(body),
    })
    return err
}

func main() {
    // Wrap the convergent DEK with the tenant CMK and stash it on
    // the manifest as you would for any client_side object — the
    // wrapped DEK still goes on the manifest so the *uploader* can
    // re-derive it on GET. Every uploader of this plaintext derives
    // the identical DEK from content + tenant_id, so any of them
    // can decrypt their own manifest.
    _ = os.Stdout
}
```

**Trade-off.** Convergent encryption removes per-object randomness
from the DEK, so the **stored file loses forward secrecy**: any
party that learns the plaintext can re-derive the CEK and decrypt
the stored ciphertext, even retroactively. This is intrinsic to
content-addressed encryption.

This trade-off is local to the stored file. It does **not** affect
MLS — the MLS channel that delivers the (CEK, object_ref) pair
retains its full forward secrecy and post-compromise security
guarantees (see section 7 below).

## 6. MLS Compatibility

MLS (RFC 9420) provides forward secrecy and post-compromise security
for the **message channel**. It does not — and is not designed to —
provide forward secrecy for stored files. Mixing the two layers
naively is the most common integration mistake.

Rules:

- **MLS FS / PCS apply to the message channel, not the file at
  rest.** Whether you use Pattern A, B, or C, MLS protections on
  KChat / kmail messages are unchanged.
- **The file CEK must be independent of MLS epoch secrets.** Derive
  it from content (convergent — Pattern C) or from `crypto/rand`
  (random — Pattern A); then distribute it over MLS as an
  application message. Never derive a file CEK from
  `exporter_secret`, `welcome_secret`, the application secret, or
  any other epoch-bound output of the MLS key schedule.
- **MLS epoch transitions do not invalidate stored files.** Member
  add / remove / key update rotates the MLS group state but does
  not rotate the file CEK; the file remains decryptable by anyone
  who already received the CEK over MLS.
- **The `exporter_secret` (RFC 9420 §8) is epoch-bound.** It is
  available specifically for application-layer key derivation, but
  using it for file encryption keys creates exactly the failure
  mode above (members lose access to old files on every epoch
  transition). Do not use it for file CEKs.

| Pattern   | MLS forward secrecy / PCS for messages | File CEK derivation         | Stored-file forward secrecy           |
| --------- | -------------------------------------- | --------------------------- | ------------------------------------- |
| Pattern A | Fully preserved                        | Random, distributed via MLS | Preserved (random per-object DEK)     |
| Pattern B | N/A — application does not encrypt     | Gateway-side convergent     | Lost (convergent DEK)                 |
| Pattern C | Fully preserved                        | Client-side convergent      | Lost (convergent DEK)                 |

## 7. Non-MLS Applications

For applications that don't use MLS — kmail Standard Private Mail,
zk-drive, Kapp Business Suite, any third-party S3 client — the
patterns map straightforwardly:

- **Pattern A**: distribute object keys over the application's own
  messaging or notification system (kmail message body, zk-drive
  share-link table, Kapp record reference, etc.). No MLS message is
  required; the app just needs a way to tell readers which key to
  GET.
- **Pattern B**: no application changes. Enable the dedup policy on
  the bucket and continue using the standard S3 client. The fabric
  handles `BLAKE3 + HKDF-SHA256` derivation, deterministic nonces,
  and `ContentIndex` lookups internally. This is the recommended
  default for `managed`-mode tenants where dedup pays off.
- **Pattern C**: use the Go client SDK at
  `encryption/client_sdk/` with
  `Options{ConvergentNonce: true}` and `DeriveConvergentDEK(...)`.
  For non-Go applications, reimplement the same construction in the
  application's language:
  - `BLAKE3` for the content hash and per-chunk nonce derivation.
  - `HKDF-SHA256` with `salt = tenant_id` and
    `info = "zkof-convergent-dek-v1"` for the DEK.
  - `XChaCha20-Poly1305` (24-byte nonce, 16-byte tag) for chunk
    sealing, with chunks of 16 MiB plaintext (matching
    `client_sdk.DefaultChunkSize`).
  Verify interoperability against a Go client before shipping.

## 8. Security trade-offs

| Property                                         | Random DEK (no dedup)            | Convergent DEK (dedup enabled)                 |
| ------------------------------------------------ | -------------------------------- | ---------------------------------------------- |
| Deduplication possible                           | No                               | Yes (intra-tenant only)                        |
| Forward secrecy of stored file                   | Yes — DEK is fresh per object    | No — anyone with the plaintext re-derives the DEK |
| Confirmation-of-file attack                      | Not feasible                     | Feasible within the same tenant                |
| MLS forward secrecy for CEK delivery             | Preserved (Pattern A)            | Preserved (Patterns A and C)                   |
| MLS post-compromise security                     | Preserved                        | Preserved                                      |
| Gateway sees plaintext (`managed` mode)          | Yes (in memory only)             | Yes (in memory only) — Pattern B               |
| Gateway sees plaintext (`client_side` mode)      | No                               | No — Pattern C, content hash is over ciphertext |

"Confirmation-of-file" means an attacker with a candidate plaintext
*and* tenant credentials can verify whether that plaintext is stored
in the tenant by re-deriving the DEK and checking whether the
resulting ciphertext hash already exists. This is intrinsic to any
content-addressed encryption scheme and is the reason cross-tenant
dedup is permanently excluded — within a tenant the attacker model
is constrained, but across tenants it becomes a privacy side
channel.

## 8.5 Complete Dedup Scenario Matrix

This matrix enumerates every (upload method × encryption mode)
combination the gateway recognises and whether the dedup pipeline
fires for it. It is the canonical answer to "will my upload dedup?"
and is referenced from
[STORAGE_INFRA.md](STORAGE_INFRA.md), [PROPOSAL.md](PROPOSAL.md)
§3.14, and [PROGRESS.md](PROGRESS.md) Phase 3.5.

| Upload Method | Encryption Mode | Dedup? | Reason |
|---|---|---|---|
| Single `PutObject` | `managed` | Yes (Pattern B) | Gateway derives convergent DEK from `BLAKE3(plaintext)` |
| Single `PutObject` | `public_distribution` | Yes (Pattern B) | Same as `managed` |
| Single `PutObject` | `client_side` + convergent | Yes (Pattern C) | Client SDK sends deterministic ciphertext |
| Single `PutObject` | `client_side` + random DEK | No | Random DEK per object — ciphertext is unique |
| Single `PutObject` | unencrypted (`""`) | No | Empty encryption mode excluded from dedup path |
| Single `PutObject` + EC profile | Any | No | Erasure-coded objects dispatched before dedup check |
| Multipart (1 part) | `client_side` or `""` | Yes | Single piece + convergent ciphertext |
| Multipart (1 part) | `managed` / `public_distribution` | Yes (Pattern B) | Deferred convergent consolidation (see below) |
| Multipart (N parts, N>1) | `client_side` or `""` | Yes | Per-part BLAKE3 + `piece_ids` JSONB column on `content_index` |
| Multipart (N parts, N>1) | `managed` / `public_distribution` | Yes (Pattern B) | Deferred convergent consolidation (see below) |
| `CopyObject` | Source has `ContentHash` | Yes | Refcount++ on existing piece, no data movement |
| `CopyObject` | Source has no `ContentHash` | No | Fresh piece via server-side copy or GET+PUT |
| `CopyObject` | Source is EC or multipart | Rejected (501) | Multi-piece sources cannot be safely refcounted |
| `DeleteObject` | Manifest has `ContentHash` | Refcount-- | Piece removed only when refcount reaches 0 |
| `DeleteObject` | No `ContentHash` | Direct delete | Legacy path, pieces deleted immediately |

### Multipart `managed` / `public_distribution` — deferred convergent consolidation

`CreateMultipartUpload` still mints a **fresh random DEK per
session** (`multipart.Upload.DEKMaterial`), so the parts streamed
to the backend during `UploadPart` cannot themselves be convergent
ciphertext. The gateway sidesteps that by consolidating the
upload into a single convergent-encrypted piece at
`CompleteMultipartUpload` time:

1. **At UploadPart**: when dedup is enabled on the upload's
   policy, the handler hashes each part's plaintext with BLAKE3
   *before* sealing it with the session's random DEK and stashes
   the digest on the in-memory upload session
   (`Upload.PlaintextPartHashes`). The plaintext is already in
   memory — the hash is essentially free.
2. **At CompleteMultipartUpload**: the per-part digests are
   concatenated in ascending `PartNumber` order and BLAKE3'd to
   produce the canonical plaintext hash:
   `plaintext_hash = BLAKE3(BLAKE3(p1) || BLAKE3(p2) || ... || BLAKE3(pN))`.
3. **Lookup**: the gateway calls
   `LookupByPlaintextHash(tenant_id, plaintext_hash)` on the
   `content_index`. If a row exists, the new upload's parts (all
   sealed with the random DEK) are deleted from the backend and
   the manifest is redirected at the canonical piece. The
   convergent DEK is re-derived on-the-fly so the new manifest
   carries an independently wrapped copy that decrypts the
   canonical piece.
4. **Miss**: the gateway reads every part back, decrypts with the
   session's random DEK, re-encrypts the assembled plaintext under
   `DEK = DeriveConvergentDEK(plaintext_hash, tenantID)` with
   `Options{ConvergentNonce: true}`, writes a single consolidated
   piece, deletes the per-part pieces, and registers a new
   `content_index` row carrying both `content_hash`
   (`BLAKE3(consolidated_ciphertext)`) and `plaintext_hash`. Any
   subsequent multipart upload of the same content with the same
   chunking matches on `plaintext_hash` and follows the hit
   branch.

**Cost.** The miss branch reads, decrypts, and re-encrypts every
part. This is one-time per fresh content and bounded by the part
size; subsequent uploads of the same content cost only a
`LookupByPlaintextHash` and the deletion of the just-uploaded
parts. The hit branch never reads parts back from the backend at
all.

**Cross-method dedup limits.** `plaintext_hash` is dual-format:
single-PUT entries store `BLAKE3(plaintext)`, multipart entries
store `BLAKE3(per-part-digests-concatenated)`. The two values
differ even for the same logical content, so a single-PUT
followed by a multipart upload (or vice versa) does not currently
dedup across methods — only within the same method. Single-PUT
after multipart still dedups when the consolidated ciphertext's
`content_hash` matches a previously written single-PUT, but that
is a coincidental match and not the primary path. A future Phase
4+ task may add an eager backfill that recomputes
`BLAKE3(plaintext)` for legacy single-PUT rows so cross-method
dedup falls out automatically.

**Re-chunking does not dedup.** Two multipart uploads of the same
plaintext bytes split into different part sizes produce different
per-part digest sequences and therefore different `plaintext_hash`
values. This is intentional — eager whole-object hashing during
multipart would require either holding the entire plaintext in
memory or reading every part back at Complete time even for
non-dedup uploads.

### Multi-part multipart uploads (N>1 parts) — supported for convergent modes

Multi-part multipart uploads now dedup when the encryption mode is
`client_side` or unencrypted (`""`). The `content_index` table
carries a nullable `piece_ids` JSONB column that holds the
ordered, per-part canonical piece list for a multi-piece entry;
the schema-PK (`tenant_id`, `content_hash`) row continues to anchor
the entry on the first piece so single-piece reverse lookups
(orphan GC, the `piece_id` index) still work for both shapes.

The gateway computes the canonical hash incrementally so it
doesn't have to re-read every part from the backend at
`CompleteMultipartUpload` time:

1. **At UploadPart**: when the upload is dedup-eligible
   (`multipartDedupEligible`), the gateway tees the part body
   through a BLAKE3 hasher and stores the per-part digest on the
   in-memory `multipart.Upload`. No backend GETs.
2. **At CompleteMultipartUpload**: the gateway combines the
   per-part digests in ascending PartNumber order:
   `content_hash = BLAKE3(hash1 || hash2 || ... || hashN)`. If any
   per-part hash is missing (e.g. the upload session was restarted)
   the gateway falls back to `hashAssembledPieces` and reads every
   piece from the backend.
3. **On hit**: the manifest's `Pieces[i]` are redirected to the
   canonical entry's `PieceIDs[i]` (matched by `PartNumber`). The
   just-uploaded duplicate parts are deleted from the backend in a
   single `deleteUploadedParts` sweep.
4. **On miss**: the gateway registers a new `content_index` row
   with `PieceIDs` populated from the upload's parts. The PK row's
   `piece_id` field is anchored on the first part so single-piece
   queries continue to work.

`managed` / `public_distribution` multipart now also dedups via
the deferred convergent consolidation flow described above. The
two flows are mutually exclusive: `client_side` / unencrypted
uploads run through the per-part ciphertext-hash + `piece_ids`
path documented here, while gateway-encrypted uploads run through
the plaintext-hash lookup + consolidation path. The encryption
mode picks the branch.

### Why erasure-coded objects don't dedup

EC objects are sharded into k+m pieces by `putErasureCoded` before
the dedup check runs in `handler.go#Put`. The dedup pipeline
operates on **single pieces** (one piece per `content_index` row);
EC objects are inherently multi-piece and would require a separate
shard-set refcount model.

For B2B tenants on Ceph RGW, **block-level dedup at the RADOS
layer** (Ceph's dedup pool tier with content-defined chunking)
catches sub-object duplicates transparently underneath the gateway
— see PROPOSAL.md §3.14.4. EC + Ceph block dedup therefore
recovers most of what object-level dedup would have provided, with
none of the schema complexity.

### CopyObject dedup behavior

`CopyObject` uses a three-tier strategy, in order:

1. **Dedup-aware fast path**: when the source manifest carries a
   `ContentHash` AND the destination tenant has a `content_index`
   row for it, the gateway calls `ContentIndex.IncrementRef` and
   writes a new manifest pointing at the **same `piece_id`** as the
   source. No backend data movement occurs.
2. **Server-side copy**: when the source has no `ContentHash` (or
   the index row has been GC'd), the gateway delegates to the
   provider's `CopyPiece` so the bytes copy provider-internally
   without crossing the gateway.
3. **GET+PUT fallback**: when the provider has no `CopyPiece`
   support, the gateway streams the source through itself and
   writes a fresh piece on the destination.

EC and multipart sources are rejected with HTTP 501
(`NotImplemented`): a multi-piece source has no single canonical
`piece_id` to refcount, and the `CopyPiece` server-side path is
not defined for sharded objects. Clients receive the same 501 for
both source types so the failure surface is uniform.

### Versioning and dedup

Each version of an object has its own manifest. Overwriting a key
creates a new manifest version (a fresh `VersionID`); the previous
manifest survives until it is deleted explicitly or expires under a
lifecycle rule. If the new version's content hash matches the old,
both manifests point at the **same physical piece** and the
`content_index` refcount is 2.

Deleting a single version decrements the refcount by exactly 1.
The piece survives as long as **any** version of any object in the
tenant references it — including older versions of the very same
key. The piece is removed from the backend only when the refcount
reaches zero, which by construction is the moment the last version
referencing it is deleted.

### Cross-cell replication and dedup

When the cross-cell replicator (`migration/cross_cell/replicator.go`)
mirrors a manifest from a source cell to a destination cell, the
**replicated manifest carries the `ContentHash` field forward** —
but the destination cell's `content_index` does not have a
corresponding entry. The replicated piece is therefore a **full,
non-deduped copy on the destination cell**.

Dedup on the destination cell only kicks in if a second upload of
the same content arrives there independently and the destination
gateway runs Pattern B / C against its own `content_index`. The
replicator does not pre-warm the destination index because doing so
across cell boundaries would create a privacy side channel: two
tenants whose replicas happen to share content would otherwise
reveal that fact through cross-cell refcount probes.

### DR copies and dedup

Per [PROPOSAL.md §3.14.9](PROPOSAL.md#3149-fallback--dr-copies),
**DR copies are always full, non-deduped objects**. DR is a
durability primitive: the goal is independent survivability of the
plaintext content even if every dedup table on the primary cell
were lost. Sharing pieces between primary and DR backends would
make the DR backend a single point of failure for the dedup
graph and is therefore explicitly forbidden by the placement
DSL's `dr` clause.

## 9. Bucket dedup policy API

The console API at `:8081` manages per-bucket dedup policy. All
endpoints require operator-level authentication; see
[docs/runbooks/tenant-setup.md](runbooks/tenant-setup.md) for the
console auth model.

### Enable dedup on a bucket

```
POST /api/v1/tenants/{tenant_id}/buckets/{bucket}/dedup-policy
Content-Type: application/json

{
  "enabled": true,
  "scope": "intra_tenant",
  "level": "object"
}
```

Fields:

| Field     | Type    | Values                                  | Description                                                                 |
| --------- | ------- | --------------------------------------- | --------------------------------------------------------------------------- |
| `enabled` | bool    | `true` / `false`                        | Master switch. `false` is equivalent to no policy (random DEKs, no dedup).  |
| `scope`   | string  | `"intra_tenant"`                        | The only supported scope. Cross-tenant dedup is permanently excluded.       |
| `level`   | string  | `"object"` / `"object+block"`           | Granularity. `object` is supported on every backend; `object+block` is Ceph RGW dedicated cells only and additionally enables sub-object block-level dedup. |

`level: "object+block"` requires that the bucket's placement policy
resolve to a Ceph RGW backend (see
[STORAGE_INFRA.md](STORAGE_INFRA.md) — Ceph RGW dedicated cell). On
non-Ceph backends the gateway rejects the request with HTTP 400.

### Read current policy

```
GET /api/v1/tenants/{tenant_id}/buckets/{bucket}/dedup-policy
```

Returns the same JSON document, or HTTP 404 if no policy is set.

### Disable dedup

```
DELETE /api/v1/tenants/{tenant_id}/buckets/{bucket}/dedup-policy
```

Removes the policy. **Existing deduplicated pieces are not
re-expanded** — historical objects retain their convergent DEKs and
shared pieces. Only *future* PUTs use random DEKs and write
independent ciphertext. Refcount-based garbage collection continues
to apply to the legacy pieces until their last referencing manifest
is deleted.

## 10. See also

- [PROPOSAL.md](PROPOSAL.md) §3.14 — Deduplication design
  (intra-tenant scope, ContentIndex, convergent encryption).
- [STORAGE_INFRA.md](STORAGE_INFRA.md) — Deduplication section
  (per-backend support matrix, Ceph RGW block-level dedup).
- [PROGRESS.md](PROGRESS.md) — Phase 3.5 (Deduplication rollout
  status).
- [PROPOSAL.md](PROPOSAL.md) §3.8 — Encryption model
  (`client_side` / `managed` / `public_distribution`).
- `metadata/manifest.go` — `EncryptionConfig` and `Piece` types.
- `encryption/client_sdk/` — Go client SDK reference implementation.
