# ZK Object Fabric — S3 Compatibility Matrix

This matrix describes ZK Object Fabric (ZKOF) coverage of the AWS S3
API surface. It is the single view that answers "does my S3 tool work
against ZKOF?"

- **Supported** — implemented in the gateway and covered by
  `tests/s3_compat/`.
- **Out of scope** — intentionally not offered (replaced by a ZKOF
  primitive, or excluded to avoid scope creep). The gateway rejects
  these sub-resources with `501 NotImplemented`; the authoritative
  reject list is `unsupportedSubresources` in
  `api/s3compat/handler.go`.

## Object operations

| S3 operation                       | ZKOF status  | Notes                                                              |
| ---------------------------------- | ------------ | ------------------------------------------------------------------ |
| `PutObject`                        | Supported    | Gateway encrypts/chunks, writes to active backend                  |
| `PutObject` (`x-amz-tagging`)      | Supported    | Tags applied at write time; same limits as `PutObjectTagging`      |
| `PutObject` (metadata)             | Supported    | `Content-Type`, system headers, and `x-amz-meta-*` persisted       |
| `GetObject`                        | Supported    | Cache or origin read; decrypts in managed mode                     |
| `GetObject` (`Range`)              | Supported    | Single byte range, suffix range (`bytes=-N`), and open-ended ranges |
| `GetObject` (multi-range)          | Supported    | Multiple ranges returned as `206 multipart/byteranges`             |
| `GetObject` (conditional)          | Supported    | `If-Match`/`If-None-Match` (strong ETag), `If-Modified-Since`/`If-Unmodified-Since`, `If-Range` |
| `GetObject` (`response-*`)         | Supported    | `response-content-type`/`-disposition`/`-encoding`/`-language`/`-cache-control`/`-expires` overrides |
| `GetObject?versionId=`             | Supported    | Object versioning via manifest versions                            |
| `HeadObject`                       | Supported    | Metadata from manifest; same conditional + `response-*` handling   |
| `DeleteObject`                     | Supported    | Manifest tombstone + async backend delete                          |
| `DeleteObjects` (bulk)             | Supported    | POST bulk-delete endpoint                                          |
| `CopyObject`                       | Supported    | Server-side copy within same tenant, including erasure-coded and multipart sources |
| `CopyObject` (conditional)         | Supported    | `x-amz-copy-source-if-match`/`-none-match`/`-modified-since`/`-unmodified-since` |
| `CopyObject` (`x-amz-metadata-directive`) | Supported | `COPY` (default) preserves source metadata; `REPLACE` applies request metadata |
| `CopyObject` (`x-amz-tagging-directive`)  | Supported | `COPY` (default) preserves source tags; `REPLACE` applies `x-amz-tagging` |
| Emitted `ETag` / `Last-Modified`   | Supported    | Returned on PUT/GET/HEAD/COPY; drive conditional and `If-Range` handling |
| Presigned GET/PUT                  | Supported    | Deployment-independent URL format                                  |
| `Put/Get/DeleteObjectTagging`      | Supported    | Tags as JSONB on manifest; 10 tags/object, 128/256-char limits     |
| `Put/GetObjectRetention`           | Supported    | Governance/compliance retain-until-date; per object version        |
| `Put/GetObjectLegalHold`           | Supported    | On/off legal hold per object version                               |
| `GetObjectAttributes`              | Out of scope | Covered by `HeadObject` + tagging                                  |
| S3 Select                          | Out of scope | SQL-on-objects excluded                                            |

## Bucket operations

| S3 operation                                   | ZKOF status  | Notes                                                       |
| ---------------------------------------------- | ------------ | ----------------------------------------------------------- |
| `CreateBucket` / `DeleteBucket` / `HeadBucket` | Supported    | Namespace in metadata + backend bucket/prefix               |
| `ListBuckets`                                  | Supported    | From tenant metadata                                        |
| `ListObjectsV2`                                | Supported    | Served from metadata store                                  |
| `ListObjectVersions`                           | Supported    | Returns manifest versions                                   |
| `Put/GetBucketVersioning`                      | Supported    | Bucket-level Enabled/Suspended config; delete markers       |
| `Put/Get/DeleteBucketLifecycleConfiguration`   | Supported    | Per-bucket rules + daily evaluator wired into the gateway   |
| `Put/GetObjectLockConfiguration`               | Supported    | Default retention rule; requires bucket versioning          |
| `Put/Get/DeleteBucketCors`                     | Supported    | Per-bucket rules + preflight (OPTIONS) and response headers |
| `Put/GetBucketNotificationConfiguration`       | Supported    | Webhook transport; `ObjectCreated`/`ObjectRemoved` events, async dispatcher |
| `Put/Get/DeleteBucketEncryption`               | Supported    | Bucket default SSE (`AES256`/`aws:kms`) → gateway ManagedEncrypted; layered onto object writes |
| `Get/PutBucketAcl`, `Get/PutObjectAcl`         | Out of scope | Replaced by tenant IAM + placement policy                   |
| `Get/PutBucketPolicy`                          | Out of scope | Replaced by ZK placement policies                           |
| `Get/PutBucketReplication`                     | Out of scope | Replaced by the ZK migration engine                         |
| `Get/PutBucketLogging`                         | Out of scope | Server-side access logging not offered                      |
| `Get/PutBucketWebsite`                         | Out of scope | Static website hosting not offered                          |
| `Get/PutBucketAccelerateConfiguration`         | Out of scope | Replaced by the ZK cache layer                              |
| `Get/PutBucketRequestPayment`                  | Out of scope | Single payer (tenant) model                                 |
| Inventory / Metrics / Analytics                | Out of scope | S3 Inventory / Storage Lens excluded                        |
| `Put/GetPublicAccessBlock`                     | Out of scope | ZK-by-default supersedes public-access blocking             |
| `Put/GetBucketOwnershipControls`               | Out of scope | Single-owner tenant model                                   |
| Intelligent-Tiering config                     | Out of scope | Tiering driven by lifecycle rules + migration engine        |

## Multipart operations

| S3 operation                | ZKOF status | Notes                                          |
| --------------------------- | ----------- | ---------------------------------------------- |
| `CreateMultipartUpload`     | Supported   | Gateway manages parts; accepts `x-amz-tagging` + metadata |
| `UploadPart`                | Supported   | Each part encrypted/chunked independently      |
| `CompleteMultipartUpload`   | Supported   | Assembles final manifest; applies upload-time tags + metadata |
| `AbortMultipartUpload`      | Supported   | Cleans up part manifests + backend pieces      |
| `ListParts`                 | Supported   | From part manifests                            |
| `ListMultipartUploads`      | Supported   | From metadata store                            |

## Client SDK

| Capability                    | ZKOF status | Notes                                                       |
| ----------------------------- | ----------- | ----------------------------------------------------------- |
| Go client-side encryption SDK | Supported   | `encryption/client_sdk/` (XChaCha20-Poly1305, convergent DEK) |

---

See [PROPOSAL.md](PROPOSAL.md) for the S3 API contract and encryption
model, and [ARCHITECTURE.md](ARCHITECTURE.md) for where each handler
lives in the codebase.
