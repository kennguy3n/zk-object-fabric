# Integration Guide — Deduplication for External Applications

This guide is the primary document external applications — KChat,
KMail, ZK Drive, the Kapp Business Suite, and any third-party S3
client — read to integrate with ZK Object Fabric's **intra-tenant**
object deduplication. It documents the three integration patterns the
fabric supports and how each interacts with end-to-end encryption,
including MLS (RFC 9420) group messaging.

**Cross-tenant dedup is permanently excluded.** It is incompatible
with the fabric's zero-knowledge guarantees and acts as a privacy
side channel (one tenant could probe to learn whether another tenant
holds a specific file). All dedup happens within a single
`tenant_id` boundary; pieces are never shared across tenants. See
[PROPOSAL.md](PROPOSAL.md) §10 ("What NOT to Do") for the full
rationale.

## Prerequisites

- A ZK Object Fabric tenant with HMAC credentials (access key + secret).
- An S3-compatible client. Any SigV4 client works — the examples in
  this guide use Python `boto3` and the Go AWS SDK.
- The target bucket must have the dedup policy enabled via the
  console API at `:8081`. See
  [Enabling Dedup on a Bucket](#enabling-dedup-on-a-bucket) below.

## Decision Tree

Walk this tree top-down to pick the right pattern for your use case:

```
Is the same file READ by many users in the same group?
├── YES → Pattern A (Single Upload, N Readers — no dedup needed)
└── NO  → continue

Is the same file UPLOADED independently by multiple users?
├── YES → Which encryption mode does the bucket use?
│         ├── client_side (Strict ZK)    → Pattern C (client-side convergent)
│         └── managed | public_distribution → Pattern B (gateway convergent)
└── NO  → No duplication expected. Use standard S3 PUT/GET. Dedup is irrelevant.
```

Rule of thumb:

- One upload, many readers → **Pattern A** (object-key sharing).
- Many uploads, gateway can see plaintext → **Pattern B** (gateway dedup).
- Many uploads, gateway cannot see plaintext → **Pattern C** (client-side dedup).

---

## Pattern A: Single Upload, N Readers

**Use case.** A KChat 1000-member group: the sender uploads a file
once and every group member fetches it by the same object key. There
is exactly one ciphertext on the backend regardless of group size;
the fan-out happens at read time.

**Properties.**

- Works with **all** encryption modes (`managed`, `client_side`,
  `public_distribution`).
- Gateway stores a single piece. Read fan-out is absorbed by the
  L0 / L1 hot-object cache (see PROPOSAL.md §3.7 and §3.11).
- No dedup logic, no `ContentIndex` writes, no client SDK changes.

**MLS impact: none.** MLS protects the *message channel* that
distributes the object reference and the file CEK. The stored
ciphertext is independent of MLS epoch state — see
[MLS Compatibility](#mls-compatibility) for the full trade-off.

**Flow.**

1. Sender encrypts the file with a Content Encryption Key (CEK)
   that is *independent of MLS epoch secrets* — either random or
   content-derived. See the warning in
   [MLS Compatibility](#mls-compatibility) about epoch-bound keys.
2. Sender PUTs ciphertext to the gateway under a single object key.
3. Sender wraps the CEK and the object reference in an MLS
   application message and sends it to the group.
4. Each receiving member unwraps the application message, learns
   the CEK and object key, GETs the same key, and decrypts locally.

### Code example (Python / boto3)

```python
import boto3, os, secrets
from cryptography.hazmat.primitives.ciphers.aead import ChaCha20Poly1305

s3 = boto3.client(
    "s3",
    endpoint_url="https://gateway.zkof.example",
    aws_access_key_id=os.environ["ZKOF_AK"],
    aws_secret_access_key=os.environ["ZKOF_SK"],
    region_name="us-east-1",
)

# --- Sender ---
plaintext = open("agenda.pdf", "rb").read()
cek       = ChaCha20Poly1305.generate_key()        # 32 bytes
nonce     = secrets.token_bytes(12)
ciphertext = nonce + ChaCha20Poly1305(cek).encrypt(nonce, plaintext, None)

s3.put_object(Bucket="group-files", Key="msg/42/agenda.pdf", Body=ciphertext)

# Distribute (cek, "msg/42/agenda.pdf") to all members
# via the MLS application-message channel — out of band of S3.

# --- Each recipient ---
obj   = s3.get_object(Bucket="group-files", Key="msg/42/agenda.pdf")["Body"].read()
nonce, ct = obj[:12], obj[12:]
plain = ChaCha20Poly1305(cek).decrypt(nonce, ct, None)
```

The gateway sees one PUT and N GETs against a single key. Cache
behavior, not dedup, is what makes this efficient.

---

## Pattern B: Gateway Convergent Encryption

**Use case.** B2C communities with viral media (the same meme
uploaded by hundreds of users) and B2B organizations where a
company-wide document is independently re-uploaded by many
employees. Many users *independently* PUT the same plaintext.

**Encryption mode.** `managed` or `public_distribution` only. These
are the modes where the gateway sees plaintext and is allowed to
derive keys from it.

**Client code changes.** **Zero.** Standard S3 PUT and GET. Dedup
is fully transparent — the second uploader sees a normal `200 OK`
and gets back a manifest pointing at the existing piece.

### Flow

1. Client PUTs plaintext (standard S3 PUT — no special headers, no
   precomputed hash).
2. Gateway computes `content_hash = BLAKE3(plaintext)`.
3. Gateway derives the convergent DEK:
   `convergent_dek = HKDF-SHA256(content_hash, salt = tenant_id)`.
4. Gateway encrypts with the convergent DEK and **deterministic**
   per-chunk nonces (see Pattern C step 3 for the nonce formula).
5. Gateway computes `ciphertext_hash = BLAKE3(ciphertext)`.
6. Gateway looks up `ContentIndex(tenant_id, ciphertext_hash)`.
7. **Match:** create a new manifest pointing at the existing
   `piece_id`, increment `ref_count`, and skip the backend write.
8. **No match:** write the piece to the backend, then insert a new
   row into `ContentIndex(tenant_id, ciphertext_hash)`.

**MLS impact: N/A.** Pattern B is gated on `managed` /
`public_distribution`, which are not Strict-ZK modes. Apps that
use MLS for E2EE messaging do not put plaintext through these
modes for confidential content; they use Pattern C.

### Code example (Python / boto3)

```python
import boto3, os
s3 = boto3.client("s3", endpoint_url="https://gateway.zkof.example",
                  aws_access_key_id=os.environ["ZKOF_AK"],
                  aws_secret_access_key=os.environ["ZKOF_SK"],
                  region_name="us-east-1")

body = open("brand-guidelines.pdf", "rb").read()

# User A — first uploader. Gateway writes a new piece.
s3.put_object(Bucket="company-docs", Key="users/alice/brand.pdf", Body=body)

# User B — uploads the EXACT same bytes under a different key.
# Gateway derives the same convergent DEK, computes the same ciphertext_hash,
# finds an existing piece in ContentIndex, increments ref_count, and skips
# the backend write. From User B's perspective: a normal 200 OK.
s3.put_object(Bucket="company-docs", Key="users/bob/brand.pdf",   Body=body)
```

Both manifests resolve to the same physical piece. Operators see
one `pieces` row with `ref_count = 2` and one backend object.

---

## Pattern C: Client-Side Convergent Encryption (Strict ZK)

**Use case.** A Strict-ZK app — for example KChat with end-to-end
encryption — where the same file is uploaded independently to
multiple groups or users, and the gateway must never see plaintext
or the keys.

**Encryption mode.** `client_side` with the convergent option
enabled. The bucket's dedup policy must be set (see
[Enabling Dedup on a Bucket](#enabling-dedup-on-a-bucket)) and the
client SDK must be invoked in convergent mode.

**Property preserved.** Strict ZK. The gateway sees only ciphertext
and a `ciphertext_hash`. It never sees plaintext or any DEK.

### Flow

1. Client SDK computes `content_hash = BLAKE3(plaintext)`.
2. Client SDK derives the convergent DEK:
   `convergent_dek = HKDF-SHA256(content_hash, salt = tenant_id)`.
3. Client SDK encrypts with **deterministic per-chunk nonces**:
   `nonce = BLAKE3(chunk_plaintext || chunk_index)[:24]`. Two
   uploads of the same plaintext under the same tenant therefore
   produce **byte-identical** ciphertext.
4. Client PUTs ciphertext to the gateway (standard S3 PUT).
5. Gateway computes `ciphertext_hash = BLAKE3(ciphertext_bytes)`.
   It never sees plaintext.
6. Gateway looks up `ContentIndex(tenant_id, ciphertext_hash)`.
7. **Match:** create a new manifest pointing at the existing
   `piece_id`, increment `ref_count`, skip the backend write.
8. **No match:** write the piece, then insert a new row into
   `ContentIndex(tenant_id, ciphertext_hash)`.

### Client SDK changes (Go)

The default Strict-ZK flow generates a random DEK and uses random
per-chunk nonces — that path is non-convergent on purpose (see
[`encryption/client_sdk/keygen.go`](../encryption/client_sdk/keygen.go)
and [`encryption/client_sdk/sdk.go`](../encryption/client_sdk/sdk.go)).
For convergent mode, swap two calls:

```go
// --- Default Strict ZK (non-convergent, no dedup) ---
dek, err := client_sdk.GenerateDEK()
if err != nil { return err }
ct, err := client_sdk.EncryptObject(plaintext, dek, client_sdk.Options{})

// --- Convergent Strict ZK (dedup-enabled) ---
dek, err := client_sdk.DeriveConvergentDEK(plaintext, tenantID)
if err != nil { return err }
ct, err := client_sdk.EncryptObject(plaintext, dek, client_sdk.Options{
    ConvergentNonce: true,
})
```

`DeriveConvergentDEK` and the `Options.ConvergentNonce` field are
the convergent extensions to the existing SDK — they reuse the same
XChaCha20-Poly1305 framing so existing decrypt code is unchanged.

### Python equivalent (for non-Go clients)

Apps that cannot use the Go SDK directly must implement the same
protocol. The exact recipe is:

```python
import os
from blake3 import blake3
from cryptography.hazmat.primitives.kdf.hkdf import HKDF
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.ciphers.aead import ChaCha20Poly1305  # XChaCha via libsodium in real code

CHUNK_SIZE = 16 * 1024 * 1024  # match client_sdk.DefaultChunkSize

def derive_convergent_dek(plaintext: bytes, tenant_id: str) -> bytes:
    content_hash = blake3(plaintext).digest()
    return HKDF(
        algorithm=hashes.SHA256(),
        length=32,
        salt=tenant_id.encode("utf-8"),
        info=b"zkof/convergent-dek/v1",
    ).derive(content_hash)

def convergent_nonce(chunk: bytes, idx: int) -> bytes:
    return blake3(chunk + idx.to_bytes(8, "big")).digest()[:24]

def encrypt_convergent(plaintext: bytes, tenant_id: str) -> bytes:
    dek = derive_convergent_dek(plaintext, tenant_id)
    out = bytearray()
    for i in range(0, len(plaintext), CHUNK_SIZE):
        chunk = plaintext[i:i + CHUNK_SIZE]
        nonce = convergent_nonce(chunk, i // CHUNK_SIZE)
        # In production, use an XChaCha20-Poly1305 implementation
        # (e.g. libsodium / PyNaCl). ChaCha20Poly1305 here is a
        # placeholder — its 12-byte nonce is NOT what the gateway
        # expects.
        out += nonce + ChaCha20Poly1305(dek).encrypt(nonce[:12], chunk, None)
    return bytes(out)
```

Implementing this protocol exactly — BLAKE3 + HKDF-SHA256 +
XChaCha20-Poly1305 with the deterministic nonce formula — is what
makes a non-Go client's ciphertext byte-identical to the Go SDK's
output, which is what enables the `ContentIndex` lookup in step 6
to hit.

---

## MLS Compatibility

This section is for any application using MLS (RFC 9420) for group
messaging — KChat is the canonical example.

1. **MLS forward secrecy (FS) and post-compromise security (PCS)
   are properties of the *message channel*, not file storage.**
   They are fully preserved regardless of which dedup pattern the
   bucket uses. Dedup never weakens the MLS handshake or the
   protection of in-transit application messages.
2. **MLS protects the message that carries the file CEK reference.**
   The file CEK itself is derived independently of MLS epoch
   secrets — either random (Pattern A with non-convergent CEK) or
   content-derived (Patterns B / C and convergent variants of A).
3. **CRITICAL WARNING — do NOT derive file CEKs from MLS epoch
   secrets.** MLS keys rotate on every membership change (every
   Add / Remove / Update epoch transition). Files encrypted with
   epoch-bound keys become **permanently unreadable** after the
   next epoch transition, because the keys needed to decrypt them
   are deleted by the MLS forward-secrecy machinery. Use random or
   content-derived (convergent) CEKs instead.
4. **Do not use the MLS `exporter_secret` (RFC 9420 §8) for file
   encryption.** It is epoch-bound and subject to the same FS-driven
   deletion as every other epoch secret.
5. **Pattern × MLS interaction matrix:**

   | Pattern | MLS FS / PCS | File-level forward secrecy |
   | --- | --- | --- |
   | A — Single upload, N readers   | Preserved | Depends on CEK scheme: random CEK delete-and-forget gives FS; convergent CEK does not. |
   | B — Gateway convergent         | N/A — bucket is `managed` / `public_distribution`, not Strict ZK; MLS is not in the picture. | None — gateway holds plaintext access. |
   | C — Client-side convergent     | Preserved | None — anyone who later obtains the same plaintext can re-derive the convergent DEK. |

6. **Trade-off, stated explicitly.** Dedup saves storage; forward
   secrecy protects already-stored data against future key
   compromise. **Pick one per bucket / dedup-policy.** For most
   B2C and B2B use cases — viral media, company-wide documents,
   software distributions — dedup savings dominate the loss of
   file-level FS. For high-sensitivity buckets (legal hold,
   confidential personal records) prefer a non-convergent CEK so
   deleting the CEK cryptographically erases the file even if the
   ciphertext lingers.

---

## Security Trade-offs

| Property                                   | Random CEK (no dedup)                         | Convergent CEK (dedup enabled)                                                        |
| ------------------------------------------ | ---------------------------------------------- | ------------------------------------------------------------------------------------- |
| Dedup possible                             | No                                             | Yes                                                                                   |
| Forward secrecy of stored file             | Yes — delete the CEK and the file is unreadable| No — anyone with the plaintext can re-derive the CEK                                  |
| Confirmation-of-file attack                | Not possible                                   | Possible — an attacker who guesses the plaintext can probe whether the file is stored |
| MLS FS for CEK delivery                    | Yes                                            | Yes — the MLS application message carrying the reference is still protected           |
| MLS PCS                                    | Unaffected                                     | Unaffected                                                                            |
| Cross-group dedup (different MLS keys)     | N/A                                            | Not possible — different tenant / app keys yield different ciphertext, by design     |

The "confirmation-of-file" row deserves emphasis: with a convergent
CEK, an attacker who *already knows* a candidate plaintext can
re-derive the DEK, encrypt, hash, and ask the gateway whether that
ciphertext exists for the tenant. This is an inherent property of
all convergent-encryption schemes, not a bug. Buckets where this
attack is in scope should use non-convergent (random) CEKs and
accept the loss of dedup.

---

## Non-MLS Applications

For applications that do not use MLS — KMail (S/MIME and managed
modes), ZK Drive (managed and Strict-ZK file storage), the Kapp
Business Suite (record attachments), and any standard S3 client:

- **Pattern A** works identically: upload once, share the object
  key with authorized users. The fabric's permission model (HMAC
  credentials + bucket policies) decides who is authorized.
- **Pattern B** is the recommended default for `managed`-mode
  apps. There are zero client-side changes — drop in the bucket's
  dedup policy and the gateway dedups transparently.
- **Pattern C** requires the app to use the ZK Object Fabric Go
  client SDK, or to implement the convergent-encryption protocol
  end-to-end:
  - Hash: BLAKE3 over plaintext.
  - KDF: HKDF-SHA256 with `salt = tenant_id` and a fixed `info`
    string (`zkof/convergent-dek/v1`).
  - Cipher: XChaCha20-Poly1305 with the deterministic per-chunk
    nonce formula `nonce = BLAKE3(chunk || chunk_index)[:24]`.
  - Chunk size: must match the gateway's expected size
    (`client_sdk.DefaultChunkSize`, currently 16 MiB).

  Any divergence in any of those four parameters yields different
  ciphertext bytes, which means the `ContentIndex` lookup misses
  and dedup silently fails.

---

## Enabling Dedup on a Bucket

> **Status:** the console API below is the *target* shape. It is
> scheduled for Phase 3.5 and is not yet implemented in the gateway.
> Track progress in [PROGRESS.md](PROGRESS.md) under the Phase 3.5
> milestone.

Issue a single console-API call against the gateway's control plane
on `:8081`:

```http
POST /api/v1/tenants/{tenant_id}/buckets/{bucket}/dedup-policy
Content-Type: application/json
Authorization: <console HMAC>

{
  "enabled": true,
  "scope":   "intra_tenant",
  "level":   "object"
}
```

- `scope` is always `intra_tenant`. Cross-tenant scopes are
  rejected by the API and will never be supported (see PROPOSAL.md
  §10).
- `level` is `object` for object-level dedup (Patterns A / B / C).
  Block-level dedup is configured operator-side, not via this API
  (see below).

### B2B cells with Ceph RGW: block-level dedup

For B2B tenants on dedicated Ceph RGW cells, block-level dedup is
configured operator-side on the Ceph cluster (the RGW dedup pool
tier and Ceph's deduplication CLI). The gateway does not need any
configuration change — Ceph handles block-level dedup transparently
beneath the S3 contract. Object-level dedup via the console API
above can be layered on top for additional savings on duplicate
whole objects.

---

## ContentIndex Schema

The `content_index` table lives in the gateway's metadata
PostgreSQL database (the same database that holds the object
manifests). It is the source of truth for "have we already stored
this ciphertext for this tenant?" lookups.

```sql
CREATE TABLE content_index (
    tenant_id    TEXT    NOT NULL,
    content_hash TEXT    NOT NULL,
    piece_id     TEXT    NOT NULL,
    backend      TEXT    NOT NULL,
    ref_count    INT     NOT NULL DEFAULT 1,
    size_bytes   BIGINT  NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, content_hash)
);
CREATE INDEX idx_content_index_piece ON content_index (piece_id);
```

Notes:

- The composite primary key `(tenant_id, content_hash)` is what
  makes dedup strictly intra-tenant. Two tenants storing the same
  bytes get two distinct rows and two distinct pieces.
- `content_hash` is the **ciphertext** BLAKE3 hash for Patterns B
  and C — never the plaintext hash. The gateway only ever sees the
  ciphertext in Pattern C.
- `ref_count` is incremented on every new manifest that points at
  the piece and decremented on manifest deletion. The piece is
  eligible for backend deletion only when `ref_count` reaches zero.
- `idx_content_index_piece` exists so reverse lookups during piece
  garbage collection (find every manifest referencing a given
  `piece_id`) stay cheap.

---

## See Also

- [PROPOSAL.md](PROPOSAL.md) §3.14 — Deduplication design (planned;
  current dedup notes live in §3.8 "Encryption model" and §10 "What
  NOT to Do").
- [STORAGE_INFRA.md](STORAGE_INFRA.md) — Dedup section (planned
  Phase 3.5 addition covering operator-side Ceph dedup pool tier
  configuration).
- [PROGRESS.md](PROGRESS.md) — Phase 3.5 milestone (intra-tenant
  dedup: console API, `ContentIndex` migrations, client SDK
  convergent extensions).
- [`encryption/client_sdk/sdk.go`](../encryption/client_sdk/sdk.go) —
  Strict-ZK client SDK; will host the `Options.ConvergentNonce`
  flag in Phase 3.5.
- [`encryption/client_sdk/keygen.go`](../encryption/client_sdk/keygen.go) —
  Current `GenerateDEK`; will host `DeriveConvergentDEK` in Phase
  3.5.
