# BYOC Backend Setup Runbook

How a Bring-Your-Own-Cloud (BYOC) customer plugs their existing
cloud storage into ZK Object Fabric. The fabric ships scaffold
adapters for Backblaze B2, Cloudflare R2, and AWS S3; this
runbook is the customer-side onboarding for each.

## When to use BYOC

- The customer already has a multi-PB footprint on B2 / R2 / S3
  and does not want to migrate.
- The customer's procurement / budgeting requires the storage
  spend to live on their cloud bill, not the fabric's.
- The customer wants direct-to-origin egress under their cloud
  provider's egress agreement (this is the Cloudflare R2
  motivation in particular — zero egress fees for most clients).

BYOC is not a substitute for the fabric's encryption / placement /
abuse-throttling layers; the gateway still wraps every PUT in
the customer's CMK and routes traffic through the rate limiter
and abuse guard. The customer's S3 bucket simply replaces Wasabi
as the durable origin.

## Adapter status

| Adapter         | Tag                | Notes                                                                  |
| --------------- | ------------------ | ---------------------------------------------------------------------- |
| `aws_s3`        | `TestSuite_AWSS3`  | Native AWS SDK v2 client; supports KMS-encrypted buckets and SigV4.    |
| `backblaze_b2`  | `TestSuite_BackblazeB2` | S3-compatible mode; uses the unsigned-payload middleware fix.     |
| `cloudflare_r2` | `TestSuite_CloudflareR2` | S3-compatible mode; Cloudflare account ID required.              |

All three embed `providers/s3_generic.Provider` and inherit the
PutPiece middleware fix from PR #25 so the AWS SDK v2 signer
accepts non-seekable `io.Reader` bodies against non-AWS
endpoints.

## Step-by-step: AWS S3

### 1. Customer-side provisioning

Customer (or their cloud team) creates a bucket with the
following IAM policy attached to the gateway's machine
identity (or to the static credential issued to the gateway):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "GatewayObjectIO",
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:AbortMultipartUpload",
        "s3:ListBucketMultipartUploads"
      ],
      "Resource": "arn:aws:s3:::customer-zkof-byoc/*"
    },
    {
      "Sid": "GatewayBucketList",
      "Effect": "Allow",
      "Action": ["s3:ListBucket", "s3:GetBucketLocation"],
      "Resource": "arn:aws:s3:::customer-zkof-byoc"
    }
  ]
}
```

If the bucket uses customer-managed KMS encryption, also grant
`kms:Decrypt` and `kms:GenerateDataKey` on the bucket's CMK.

### 2. Gateway config

```json
{
  "providers": {
    "aws_s3": {
      "region":     "us-east-1",
      "bucket":     "customer-zkof-byoc",
      "endpoint":   "",
      "access_key": "${AWS_S3_ACCESS_KEY}",
      "secret_key": "${AWS_S3_SECRET_KEY}"
    }
  }
}
```

Leave `endpoint` empty for the standard AWS S3 endpoint; set it
explicitly only when the customer is using a privately routed
S3 (e.g., S3 on Outposts or PrivateLink).

### 3. Run compliance suite

```bash
AWS_S3_ENDPOINT="" \
AWS_S3_BUCKET="customer-zkof-byoc" \
AWS_S3_ACCESS_KEY="$AWS_S3_ACCESS_KEY" \
AWS_S3_SECRET_KEY="$AWS_S3_SECRET_KEY" \
  go test -v -run TestSuite_AWSS3 ./tests/s3_compat/
```

13/13 subtests should PASS. Capture the log under
`docs/audits/aws-s3-${customer}-${date}.log` for the customer's
compliance team.

### 4. Apply tenant placement

```bash
curl -fsS -X PUT \
  -H "Authorization: Bearer $ZKOF_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{
    "provider":        "aws_s3",
    "region":          "us-east-1",
    "country_allow_list": ["US"],
    "erasure_profile": "6+2",
    "encryption_mode": "managed"
  }' \
  "$CONSOLE_URL/api/tenants/${TENANT_ID}/placement"
```

## Step-by-step: Backblaze B2

### 1. Customer-side provisioning

Customer creates a B2 bucket and an Application Key scoped to
that bucket with `readFiles`, `writeFiles`, `deleteFiles`,
`listFiles`, and `listBuckets` capabilities.

### 2. Gateway config

```json
{
  "providers": {
    "backblaze_b2": {
      "endpoint":   "https://s3.us-west-002.backblazeb2.com",
      "region":     "us-west-002",
      "bucket":     "customer-zkof-byoc",
      "access_key": "${B2_ACCESS_KEY}",
      "secret_key": "${B2_SECRET_KEY}"
    }
  }
}
```

The endpoint format is `https://s3.${REGION}.backblazeb2.com`
with the region matching the bucket's home region.

### 3. Run compliance suite

```bash
B2_ENDPOINT="https://s3.us-west-002.backblazeb2.com" \
B2_BUCKET="customer-zkof-byoc" \
B2_ACCESS_KEY="$B2_ACCESS_KEY" \
B2_SECRET_KEY="$B2_SECRET_KEY" \
  go test -v -run TestSuite_BackblazeB2 ./tests/s3_compat/
```

### 4. Apply placement

```bash
curl -fsS -X PUT \
  -H "Authorization: Bearer $ZKOF_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{
    "provider":        "backblaze_b2",
    "region":          "us-west-002",
    "erasure_profile": "6+2",
    "encryption_mode": "managed"
  }' \
  "$CONSOLE_URL/api/tenants/${TENANT_ID}/placement"
```

## Step-by-step: Cloudflare R2

### 1. Customer-side provisioning

Customer creates an R2 bucket and an R2 API token scoped to that
bucket with **Object Read + Write** permissions.

### 2. Gateway config

```json
{
  "providers": {
    "cloudflare_r2": {
      "account_id": "${R2_ACCOUNT_ID}",
      "endpoint":   "https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com",
      "bucket":     "customer-zkof-byoc",
      "access_key": "${R2_ACCESS_KEY}",
      "secret_key": "${R2_SECRET_KEY}"
    }
  }
}
```

The endpoint format is
`https://${ACCOUNT_ID}.r2.cloudflarestorage.com` (or the
`-eu.r2.cloudflarestorage.com` variant for EU-resident buckets).

### 3. Run compliance suite

```bash
R2_ENDPOINT="https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com" \
R2_BUCKET="customer-zkof-byoc" \
R2_ACCESS_KEY="$R2_ACCESS_KEY" \
R2_SECRET_KEY="$R2_SECRET_KEY" \
  go test -v -run TestSuite_CloudflareR2 ./tests/s3_compat/
```

### 4. Apply placement

```bash
curl -fsS -X PUT \
  -H "Authorization: Bearer $ZKOF_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{
    "provider":        "cloudflare_r2",
    "region":          "auto",
    "erasure_profile": "6+2",
    "encryption_mode": "managed"
  }' \
  "$CONSOLE_URL/api/tenants/${TENANT_ID}/placement"
```

## Nightly compliance CI

The `.github/workflows/byoc-compliance.yml` workflow runs the
three suites nightly against the credentials stored in
GitHub Actions secrets. Failures page the platform on-call
through the standard PagerDuty hook configured in
`config.abuse.alert_webhook_url`.

| Secret name              | Purpose                                                  |
| ------------------------ | -------------------------------------------------------- |
| `B2_ACCESS_KEY`          | Application key for the B2 nightly bucket.               |
| `B2_SECRET_KEY`          | Corresponding application key secret.                    |
| `B2_BUCKET`              | Bucket name (e.g. `zkof-nightly-b2`).                    |
| `B2_ENDPOINT`            | Region-scoped endpoint URL.                              |
| `R2_ACCESS_KEY`          | R2 token access key.                                     |
| `R2_SECRET_KEY`          | R2 token secret access key.                              |
| `R2_BUCKET`              | Bucket name.                                             |
| `R2_ENDPOINT`            | Cloudflare R2 endpoint URL.                              |
| `AWS_S3_ACCESS_KEY`      | IAM user access key for the AWS S3 nightly bucket.       |
| `AWS_S3_SECRET_KEY`      | Corresponding secret access key.                         |
| `AWS_S3_BUCKET`          | Bucket name.                                             |
| `AWS_S3_REGION`          | AWS region (e.g. `us-east-1`).                           |

If any of those secrets are unset (e.g. on forks), the
workflow's helper step skips that adapter and the run stays
green. The corresponding `TestSuite_*` helpers self-skip when
their env vars are unset.

## Troubleshooting

### `SignatureDoesNotMatch` against B2 / R2 / non-AWS S3

The fix is already shipped: `providers/s3_generic/generic.go`
applies `v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware`
on every PutPiece. If you see the error against a bespoke
S3-compatible endpoint that is not B2 / R2 / AWS S3, confirm
the endpoint accepts unsigned payloads (`x-amz-content-sha256:
UNSIGNED-PAYLOAD`); if not, the customer needs to enable that
header acceptance on their endpoint or accept signed-only
PUTs (which the gateway does not currently emit because they
require a seekable body).

### `NoSuchUpload` after a CompleteMultipartUpload

R2 occasionally takes a second after CompleteMultipartUpload
before subsequent reads see the new object. The gateway's
multipart handler retries on `NoSuchKey` once with a 500ms
backoff before giving up.

### Egress charges on AWS S3

If the customer's BYOC bucket is in `us-east-1` but the
gateway fleet is in `eu-central-1`, every GET incurs
inter-region egress charges. The placement policy's
`country_allow_list` and `region` fields should match the
bucket's region; if they don't, the operator must explain the
trade-off to the customer or move the gateway fleet's
preferred region.
