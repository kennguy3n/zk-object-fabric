# Wasabi Multi-Region Bucket Provisioning

This directory ships the operator scripts that stand up the Phase 3
production Wasabi origin: one bucket per region, naming convention
`zkof-{region}-{env}`, with per-bucket IAM and presigned-URL CORS
support.

## Layout

| File                          | Purpose                                                          |
| ----------------------------- | ---------------------------------------------------------------- |
| `provision_buckets.sh`        | Idempotent bucket creator across all regions in `regions.env`.   |
| `regions.env.example`         | Template region list. Copy to `regions.env` and edit.            |
| `iam_policy.template.json`    | Per-bucket IAM policy (object read/write/list, no admin).        |
| `cors_config.json`            | CORS rules for presigned-URL GET/PUT.                            |
| `gateway_config.example.json` | Snippet for the gateway's `config.providers.wasabi.regions` list.|

## Quick start

```bash
cp regions.env.example regions.env
$EDITOR regions.env  # set WASABI_ROOT_ACCESS_KEY / SECRET / regions

./provision_buckets.sh
```

The script is idempotent: re-running it on a bucket that already
exists is a no-op for that bucket but still re-applies the bucket
policy and CORS configuration so drift is corrected on every run.

## Region naming

The gateway registers each region as a separate `StorageProvider`
under the registry key `wasabi-<region>`, so a placement policy
that wants to pin to EU traffic writes:

```yaml
provider: wasabi-eu-central-1
region:   eu-central-1
country:  DE
```

This stays compatible with the legacy single-region `provider:
wasabi` configuration; if both are configured, both register and
`pickDefaultBackend` prefers the legacy `wasabi` entry for boot.
A pure multi-region config (no legacy `endpoint` / `bucket` set)
also boots with a Wasabi default — `pickDefaultBackend` falls
through to the lexicographically-smallest `wasabi-<region>` key
when no `wasabi` entry exists. Production deploys should pin a
specific region as the default via tenant placement policies
rather than relying on this fallback.

## Output

After provisioning, the script prints an example
`config.providers.wasabi.regions` block ready to paste into the
gateway config. Production deploys store the per-region access /
secret keys in AWS Secrets Manager (or HashiCorp Vault) and let
the gateway init container pull them at boot.

## Cost & compliance notes

- Wasabi enforces a 90-day minimum storage duration. Configure the
  rebalancer's `drain` state to honor this so we don't pay for
  early-deletion fees when migrating into a local DC cell.
- Each bucket is region-locked; cross-region replication is not
  used. The gateway's dual-write topology handles cross-region
  redundancy at the placement level, not at the bucket level.
- The IAM policy template grants `s3:GetObject`, `s3:PutObject`,
  `s3:DeleteObject`, `s3:AbortMultipartUpload`,
  `s3:ListBucketMultipartUploads`, and `s3:ListBucket` — no
  bucket-level deletion or policy mutation. Operators with the
  root credential perform bucket-level changes via this script,
  not via the gateway's runtime credential.
