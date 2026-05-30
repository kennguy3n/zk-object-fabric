# External S3 Conformance Harnesses

Wiring for the two third-party S3 conformance suites listed in
[`docs/runbooks/s3-conformance.md`](../../docs/runbooks/s3-conformance.md):

- **Ceph s3-tests** — Python/nose suite, the canonical S3
  conformance gate used by Ceph RGW, MinIO, Backblaze B2, and
  most other S3-compatible vendors. Produces JUnit XUnit XML.
- **MinIO mint** — Docker-based multi-SDK harness exercising
  AWS SDKs across Go, Java, Python, JS, .NET, and Ruby.
  Produces per-SDK newline-delimited JSON logs.

Plus a Go aggregator (`cmd/conformance-aggregate`) that parses
both harness outputs into a single normalised matrix JSON
compatible with the in-process matrix from
`tests/s3_conformance/` for side-by-side diffing in the audit
dossier.

## Why two harnesses

The in-process matrix at `tests/s3_conformance/` exercises the
gateway with **one** S3 client implementation (AWS SDK v2 for Go).
A defect that affects only a specific SDK's quirks (e.g. a
Java SDK that double-URL-encodes object keys before signing) would
sail through that gate.

s3-tests and mint together cover:

- 6 SDK implementations × the canonical S3 surface (mint).
- Edge cases AWS itself documents but most clients don't trigger,
  e.g. `If-Match` with non-ASCII ETags, multipart-with-zero-parts,
  presigned URL clock-skew tolerance (s3-tests).

If both external harnesses agree with the in-process matrix on
every supported operation, the gateway is conformant at the
SDK-and-edge-case level the AWS S3 reference implementation is
audited at.

## What this directory contains

| Path                                            | Purpose                                                                                 |
| ----------------------------------------------- | --------------------------------------------------------------------------------------- |
| `scripts/run_s3tests.sh`                        | Boots venv, clones ceph/s3-tests, writes config, runs nosetests, archives XUnit XML.    |
| `scripts/run_mint.sh`                           | Pulls `minio/mint:edge`, runs container against the gateway, archives per-SDK logs.     |
| `scripts/aggregate.sh`                          | Walks `$REPORTS_DIR/external/{s3tests,mint}`, picks latest outputs, runs aggregator.    |
| `s3tests/skip-tags.txt`                         | Authoritative skip list mapping to the gateway's `unsupported` set.                     |
| `mint/` (intentionally empty)                   | Placeholder for any future operator-supplied mint-config overrides (currently env-only).|

The corresponding code:

| Path                                                              | Purpose                                                            |
| ----------------------------------------------------------------- | ------------------------------------------------------------------ |
| `tests/conformance/external/external.go`                          | XUnit + mint log parsers, `Matrix` aggregator, sort + serialise.   |
| `cmd/conformance-aggregate/main.go`                               | CLI wrapper around the aggregator package.                         |

## Operator quick start

Assumes the gateway is reachable at `$GATEWAY_ENDPOINT` with a
test bucket and IAM credentials. The scripts run on any Linux
host with Python 3.10+, docker, git, and Go (only at build time).

```bash
export GATEWAY_ENDPOINT="https://gw.staging.example.com"
export GATEWAY_BUCKET="zkof-conf-$(date +%s)"
export GATEWAY_ACCESS_KEY="..."
export GATEWAY_SECRET_KEY="..."
export GATEWAY_SHA="$(git -C /path/to/zk-object-fabric rev-parse HEAD)"
export REPORTS_DIR="/var/lib/zkof-conf/reports"

# 1. Run Ceph s3-tests (10-20 minutes; exits 0 = pass-or-skip,
#    1 = failure; XUnit is always archived).
./scripts/run_s3tests.sh

# 2. Run MinIO mint (5-15 minutes depending on MINT_MODE;
#    defaults to "core" which is the minimal AWS-correct subset).
./scripts/run_mint.sh

# 3. Aggregate both into a single matrix JSON. Picks the
#    most-recent outputs from $REPORTS_DIR/external/.
./scripts/aggregate.sh
```

The aggregator emits
`$REPORTS_DIR/external/matrix-{timestamp}.json` containing every
entry from both harnesses with normalised statuses (`passed`,
`failed`, `unsupported`, `errored`) plus the gateway endpoint
and SHA stamped at the top level. Drop this into the audit
dossier alongside the in-process matrix.

## Exit codes and CI gating

Each script preserves the underlying harness's exit code:

| Script              | Exit 0                              | Exit 1                              | Exit ≥64                            |
| ------------------- | ----------------------------------- | ----------------------------------- | ----------------------------------- |
| `run_s3tests.sh`    | All tests passed or skipped.        | One or more failures or errors.     | Setup error (missing env, repo).    |
| `run_mint.sh`       | All SDKs reported no defects.       | At least one SDK reported a defect. | docker unavailable / image pull.    |
| `aggregate.sh`      | All entries pass or unsupported.    | One or more failed/errored entries. | No input found / aggregator missing.|

The intended CI/dossier policy: a `failed` or `errored` entry in
either harness blocks publication of the matrix. An
`unsupported` entry that matches a tag in `skip-tags.txt` is
expected. An `unsupported` entry whose tag is NOT in
`skip-tags.txt` is a regression (the gateway started returning
501 for an operation it previously supported) and should be
investigated.

## Pinning the harness versions

For audit reproducibility, pin `S3TESTS_REV` and `MINT_IMAGE` to
specific revisions/tags before recording a published matrix:

```bash
export S3TESTS_REV="$(git -C /tmp/s3-tests rev-parse HEAD)"
export MINT_IMAGE="minio/mint@sha256:<digest>"
```

These two values, together with the gateway SHA stamped in the
matrix, are the three-tuple an auditor needs to re-run the
exact same harness mix against the exact same gateway build.
