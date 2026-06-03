# ZK Object Fabric — S3 Compatibility Matrix

This matrix tracks ZK Object Fabric (ZKOF) coverage against the AWS S3
API surface. It is the single view that answers "does my S3 tool work
against ZKOF, and if not, is it on the roadmap?"

- **Supported** — implemented and covered by `tests/s3_compat/`.
- **Planned (WSx)** — roadmapped under a Workstream slice in
  [PROPOSAL.md §15](PROPOSAL.md); not yet shipped. The gateway
  currently rejects the sub-resource with `501 NotImplemented`.
- **Out of scope** — intentionally not offered (replaced by a ZKOF
  primitive, or excluded to avoid scope creep). See
  [PROPOSAL.md §3.2.2](PROPOSAL.md).

Status reflects the design intent in PROPOSAL.md, not a live
deployment probe. The authoritative reject list is
`unsupportedSubresources` in `api/s3compat/handler.go`.

## Object operations

| S3 operation                       | ZKOF status     | Notes                                                              |
| ---------------------------------- | --------------- | ------------------------------------------------------------------ |
| `PutObject`                        | Supported       | Gateway encrypts/chunks, writes to active backend                  |
| `GetObject`                        | Supported       | Cache or origin read; decrypts in managed mode                     |
| `GetObject` (`Range`)              | Supported       | Range-aligned encrypted chunks                                     |
| `GetObject?versionId=`             | Supported       | Object versioning via manifest versions                            |
| `HeadObject`                       | Supported       | Metadata from manifest                                             |
| `DeleteObject`                     | Supported       | Manifest tombstone + async backend delete                          |
| `DeleteObjects` (bulk)             | Supported       | POST bulk-delete endpoint                                          |
| `CopyObject`                       | Supported       | Server-side copy within same tenant                                |
| Presigned GET/PUT                  | Supported       | Phase-independent URL format                                       |
| `Put/Get/DeleteObjectTagging`      | Supported       | Tags as JSONB on manifest; 10 tags/object, 128/256-char limits     |
| `Put/GetObjectRetention`           | Supported       | Governance/compliance retain-until-date; per object version        |
| `Put/GetObjectLegalHold`           | Supported       | On/off legal hold per object version                               |
| `GetObjectAttributes`              | Out of scope    | Covered by `HeadObject` + tagging                                  |
| S3 Select                          | Out of scope    | SQL-on-objects excluded                                            |

## Bucket operations

| S3 operation                                   | ZKOF status     | Notes                                                       |
| ---------------------------------------------- | --------------- | ----------------------------------------------------------- |
| `CreateBucket` / `DeleteBucket` / `HeadBucket` | Supported       | Namespace in metadata + backend bucket/prefix               |
| `ListBuckets`                                  | Supported       | From tenant metadata                                        |
| `ListObjectsV2`                                | Supported       | Served from metadata store                                  |
| `ListObjectVersions`                           | Supported       | Returns manifest versions                                   |
| `Put/GetBucketVersioning`                      | Supported       | Bucket-level Enabled/Suspended config; delete markers       |
| `Put/Get/DeleteBucketLifecycleConfiguration`   | Planned (WS8.2) | Per-bucket rules + daily evaluator                          |
| `Put/GetObjectLockConfiguration`               | Supported       | Default retention rule; requires versioning (WS8.4)         |
| `Put/Get/DeleteBucketCors`                     | Supported       | Per-bucket rules + preflight (OPTIONS) and response headers  |
| `Put/GetBucketNotificationConfiguration`       | Planned (WS8.6) | Webhook transport; `ObjectCreated`/`ObjectRemoved`          |
| `Put/Get/DeleteBucketEncryption`               | Planned (WS8.7) | Maps SSE header to ZKOF encryption modes                    |
| `Get/PutBucketAcl`, `Get/PutObjectAcl`         | Out of scope    | Replaced by tenant IAM + placement policy                   |
| `Get/PutBucketPolicy`                          | Out of scope    | Replaced by ZK placement policies (§3.10)                   |
| `Get/PutBucketReplication`                     | Out of scope    | Replaced by the ZK migration engine (§4)                    |
| `Get/PutBucketLogging`                         | Out of scope    | Server-side access logging not offered                      |
| `Get/PutBucketWebsite`                         | Out of scope    | Static website hosting not offered                          |
| `Get/PutBucketAccelerateConfiguration`         | Out of scope    | Replaced by the ZK cache layer (§3.7)                       |
| `Get/PutBucketRequestPayment`                  | Out of scope    | Single payer (tenant) model                                 |
| Inventory / Metrics / Analytics                | Out of scope    | S3 Inventory / Storage Lens excluded                        |
| `Put/GetPublicAccessBlock`                     | Out of scope    | ZK-by-default supersedes public-access blocking             |
| `Put/GetBucketOwnershipControls`               | Out of scope    | Single-owner tenant model                                   |
| Intelligent-Tiering config                     | Out of scope    | Tiering driven by lifecycle (WS8.2) + migration engine      |

## Multipart operations

| S3 operation                | ZKOF status | Notes                                          |
| --------------------------- | ----------- | ---------------------------------------------- |
| `CreateMultipartUpload`     | Supported   | Gateway manages parts                          |
| `UploadPart`                | Supported   | Each part encrypted/chunked independently      |
| `CompleteMultipartUpload`   | Supported   | Assembles final manifest from part manifests   |
| `AbortMultipartUpload`      | Supported   | Cleans up part manifests + backend pieces      |
| `ListParts`                 | Supported   | From part manifests                            |
| `ListMultipartUploads`      | Supported   | From metadata store                            |

## Client SDK

| Capability                         | ZKOF status   | Notes                                                       |
| ---------------------------------- | ------------- | ----------------------------------------------------------- |
| Go client-side encryption SDK      | Supported     | `encryption/client_sdk/` (XChaCha20-Poly1305, convergent DEK) |
| Rust client-side encryption SDK    | Planned (WS9) | Byte-compatible with the Go SDK (PROPOSAL.md §15.2)         |

---

See [PROPOSAL.md §15](PROPOSAL.md) for the Workstream 8 / 9
specifications and [PROGRESS.md](PROGRESS.md) for per-slice status.
