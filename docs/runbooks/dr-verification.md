# DR — Automated Verification

This document describes the in-process disaster-recovery verifier
([`tests/dr/verifier.go`](../../tests/dr/verifier.go)) and the CI job
that runs it on every push.

## What the verifier checks

The verifier drives a full four-phase DR cycle against the real
`cross_cell.Replicator`, an in-memory manifest store
(`metadata/manifest_store/memory`), and the local-FS provider
(`providers/local_fs_dev`). On every run it asserts three invariants
and produces a JSON report:

1. **Steady-state objects are byte-recoverable**. Every object PUT
   into the source cell before the snapshot point must be readable
   from the destination cell with byte-identical content after the
   simulated failure. Any mismatch is a hard test failure — the
   replicator must not corrupt or truncate manifests/pieces in
   transit.

2. **RPO equals the in-flight count**. The verifier intentionally
   stops the replicator after the steady drain, then seeds N
   in-flight objects in the source cell. Those N objects MUST be
   missing from the destination after the failure (the production
   analogue is "PUTs after the operator-declared cutoff, before LB
   flip, are lost"). `MeasuredRPO == InFlightObjects` is asserted
   exactly.

3. **RTO is bounded**. Wall-clock from `FailureDetectedAt` to the
   first successful GET against the destination cell must be less
   than or equal to `RTOTarget`. The verifier returns an error
   (and CI fails) when this exceeds the published target in
   [`dr.md`](./dr.md#published-rpo--rto-targets).

The verifier also pins replicator behaviour:

- Every destination-side piece's `Backend` field is rewritten to
  the destination cell's ID. A piece still pointing at the source
  ID after recovery is a replicator regression.
- The cross-cell replicator copies at least one piece per steady
  object before cancellation. A zero progress count fails the run
  with "replicator made zero progress; fixture is broken" so a
  silently-broken test setup cannot mask a real regression.

## Reading the report

The CI job writes the report to `artifacts/dr-report.json`. The
schema (from [`tests/dr/verifier.go`](../../tests/dr/verifier.go) `Report`
struct) is:

| Field                       | Meaning                                                  |
|-----------------------------|----------------------------------------------------------|
| `started_at` / `finished_at`| Wall-clock bounds of the cycle.                          |
| `steady_objects`            | Configured count of pre-snapshot PUTs.                   |
| `in_flight_objects`         | Configured count of post-snapshot PUTs.                  |
| `recovered_objects`         | Distinct steady objects readable from dest after failure. |
| `lost_objects`              | In-flight objects not present on dest.                   |
| `measured_rpo_objects`      | == `lost_objects` for the in-process harness.            |
| `failure_detected_at`       | Wall-clock when the verifier cancelled the replicator.   |
| `recovery_ready_at`         | Wall-clock of the first successful dest GET.             |
| `measured_rto`              | `recovery_ready_at` minus `failure_detected_at`.         |
| `replicator_lag_at_snapshot`| `cross_cell.Replicator.LagNanos()` at the snapshot.      |
| `replicator_pieces_copied`  | Lifetime CopiedPieces count at the end of the cycle.     |

A regression in any field above is a release blocker. The CI job
artifact retains 30 runs so the operator can trend the measured
numbers over time (a slow upward drift in `measured_rto` indicates
the replicator is gaining cost on the hot path; investigate before
the published target is breached).

## Running locally

```bash
go test -race -count=1 -v ./tests/dr/...
```

The full battery completes in under 3 seconds on a stock dev
laptop. Use `-run TestVerifier_HappyPath` to isolate the
happy-path flow during local debugging.

To dump the JSON report from a local run:

```bash
DR_REPORT_FILE=$PWD/dr-report.json \
  go test -count=1 -run TestVerifier_HappyPath ./tests/dr/...
cat dr-report.json
```

The env var is the same seam the CI workflow uses; setting it
is the only way to obtain the report struct on disk (the
verifier itself returns a `*Report` to its caller — the test
is responsible for persisting it).

The test binary itself drives the verifier; there is no separate
CLI today. If that ever becomes necessary the next step is to add
`cmd/dr-verifier/main.go` that wires a `Verifier` against
real-deployment cells from a config file. The harness is already
shaped for this — `Verifier.Source` / `Verifier.Dest` accept any
`cross_cell.Cell` implementation.

## CI integration

The verifier runs as a dedicated CI job that:

1. Builds the repo with `go build ./...`.
2. Runs the verifier with `-race -count=1` to flush any
   data-race regressions.
3. Uploads `artifacts/dr-report.json` as a build artifact for
   trend analysis.

See [`.github/workflows/dr-verify.yml`](../../.github/workflows/dr-verify.yml)
for the exact job definition. The job runs on every push to PR
branches plus daily on a cron schedule so a slow regression that
only shows up under churn is caught without waiting for the next
feature PR.

## What the verifier does NOT cover

The verifier is the regression gate for the cross-cell async
failover path only. The other two DR runbooks
([`dr-postgres-restore.md`](./dr-postgres-restore.md),
[`dr-manifest-resume.md`](./dr-manifest-resume.md)) describe
out-of-band recovery flows that require external infrastructure
(Postgres + pg_basebackup, a real provider) and are not exercised
in CI. They are exercised by quarterly operator drills; see each
runbook's "DR drill" section.

A future infrastructure-level harness could lift those out into
container-based drill scripts (e.g. spin up a Postgres + pgbackrest
+ gateway under docker compose and assert the restore flow). That
is out of scope for this runbook.
