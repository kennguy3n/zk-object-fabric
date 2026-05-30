# External S3 Conformance Harness Runbook

This runbook is the operator-facing companion to the
S3 conformance runbook ([`s3-conformance.md`](s3-conformance.md))
introduced alongside the in-process matrix in
`tests/s3_conformance/`. It walks through running the two
third-party harnesses (Ceph `s3-tests` and MinIO `mint`) against
a deployed gateway and aggregating their output into a single
audit-grade matrix JSON.

## Scope

This runbook is for the **audit-side** conformance run — the
gate that an external auditor (or our own monthly audit run)
exercises against a long-lived gateway endpoint (typically the
Tier-3 staging deployment from WS2.1, or production after a
release). It is NOT the in-process matrix from `s3-conformance.md`,
which runs on every CI commit against `local_fs_dev` and is the
fast hermetic gate.

The two are complementary:

| Gate                | When            | Where         | Drives                                  |
| ------------------- | --------------- | ------------- | --------------------------------------- |
| In-process matrix   | Every commit    | CI runner     | `go test ./tests/s3_conformance/...`    |
| External harnesses  | Pre-release     | Operator host | `deploy/conformance/scripts/run_*.sh`   |

A diff between the two matrices for the same gateway build is
itself a useful audit artifact: a row that the in-process matrix
records as `passed` but the external harness records as `failed`
is a real defect — the in-process runner is too lenient (or the
external harness exposes an SDK quirk the AWS SDK v2 for Go
doesn't trigger). A row that the in-process matrix records as
`unsupported` but the external harness records as `passed` is
the opposite — the in-process runner is overly conservative and
the operation actually works.

## Prerequisites

- A reachable gateway endpoint with:
  - HTTPS (the harnesses can run against `http://` but every
    audit-grade run uses TLS).
  - IAM credentials with full read/write on a dedicated
    conformance test bucket.
- The operator host needs:
  - Python 3.10+ and `pip` (for s3-tests).
  - Docker (for mint).
  - `git`, `bash`, `find` (GNU coreutils).
  - The `conformance-aggregate` binary on PATH (or at
    `/opt/zkof-conf/bin/conformance-aggregate`). Build from
    repo root with `make build` or `go install
    ./cmd/conformance-aggregate`.

## Running the harnesses

The driver scripts live in `deploy/conformance/scripts/`. They
are idempotent — each run produces a timestamped artifact set
under `$REPORTS_DIR/external/` without overwriting previous
runs, so an operator can re-run the same harness multiple times
during incident triage without losing earlier evidence.

```bash
export GATEWAY_ENDPOINT="https://gw.staging.example.com"
export GATEWAY_BUCKET="zkof-conf-$(date +%s)"
export GATEWAY_ACCESS_KEY="..."
export GATEWAY_SECRET_KEY="..."
export GATEWAY_SHA="$(git rev-parse HEAD)"
export REPORTS_DIR="/var/lib/zkof-conf/reports"

cd deploy/conformance

# Step 1: Ceph s3-tests (~10-20 min)
./scripts/run_s3tests.sh

# Step 2: MinIO mint (~5-15 min in MINT_MODE=core)
./scripts/run_mint.sh

# Step 3: Aggregate
./scripts/aggregate.sh
```

Output structure under `$REPORTS_DIR/external/`:

```
external/
├── s3tests/
│   ├── 20260530T143000Z.xml      # XUnit XML from nosetests
│   └── 20260530T143000Z.log      # Full stdout/stderr of the run
├── mint/
│   └── 20260530T144500Z/         # Per-mint-run dir
│       ├── aws-sdk-go/log.json
│       ├── aws-sdk-java/log.json
│       └── ...                    # One subdir per SDK mint exercised
└── matrix-20260530T150000Z.json   # Aggregated normalised matrix
```

## Reading the matrix JSON

The matrix conforms to the schema produced by
`tests/conformance/external.Matrix`:

```jsonc
{
  "gateway_endpoint": "https://gw.staging.example.com",
  "gateway_sha":      "abc123...",
  "generated_at":     "2026-05-30T15:00:00Z",
  "entries": [
    {
      "op":          "test_object_put_get_range",
      "category":    "test_s3",
      "source":      "ceph-s3-tests",
      "status":      "passed",
      "duration_ms": 823
    },
    {
      "op":          "PutObjectTagging",
      "category":    "aws-sdk-go",
      "source":      "minio-mint",
      "sdk":         "aws-sdk-go",
      "status":      "failed",
      "duration_ms": 3,
      "detail":      "PutObjectTagging failed; tagging surface not implemented"
    }
  ]
}
```

Statuses map to actions:

| Status        | Action                                                                                                              |
| ------------- | ------------------------------------------------------------------------------------------------------------------- |
| `passed`      | None.                                                                                                               |
| `unsupported` | Confirm the corresponding tag is in `deploy/conformance/s3tests/skip-tags.txt` (s3-tests) or expected for MINT_MODE. |
| `failed`      | **Defect.** Triage: the harness expected one behaviour, the gateway returned another. Compare against in-process matrix to localise. |
| `errored`     | **Infrastructure issue.** Typically network failure, missing bucket, or harness panic. Re-run the affected harness alone first.      |

## Updating the skip-tag list

When a feature is implemented (e.g. object tagging lands):

1. Remove the tag (e.g. `tagging`) from
   `deploy/conformance/s3tests/skip-tags.txt`.
2. Remove the corresponding entry from
   `unsupportedSubresources` in `api/s3compat/handler.go`.
3. Re-run the external harness — the previously-`unsupported`
   rows should flip to `passed`. If any flip to `failed`, the
   feature is not yet fully conformant.
4. Move the operation in `tests/s3_conformance/runner_test.go`
   from `mustBeUnsupported` to `mustPass` so the in-process
   gate matches.

## Pinning the harness versions

For audit reproducibility (the auditor must be able to re-run
the exact same harness mix months later), pin both upstream
references before recording a published matrix:

```bash
# Pin s3-tests to a specific commit.
export S3TESTS_REV="$(git -C /tmp/s3-tests rev-parse HEAD)"

# Pin mint to a specific image digest.
export MINT_IMAGE="minio/mint@sha256:$(docker inspect minio/mint:edge --format '{{ index .RepoDigests 0 }}' | cut -d@ -f2)"
```

Both values, plus `$GATEWAY_SHA`, are the three-tuple the audit
dossier (WS2.4) bundles so an auditor can reproduce the exact
verification run.

## Aggregator CLI reference

```
conformance-aggregate \
  [-s3tests-xunit  PATH] \
  [-mint-logs-dir  PATH] \
  [-gateway-endpoint URL] \
  [-gateway-sha SHA] \
  [-out OUT_PATH]

Exit codes:
  0  audit-pass (all entries pass or unsupported)
  1  one or more failed or errored entries
  64 invalid CLI usage
  65 input file could not be parsed
```

At least one of `-s3tests-xunit` and `-mint-logs-dir` is
required. Use both for a full audit run; use one when
re-aggregating a single harness output during incident triage.
