# Disaster Recovery — Index

This is the operator-facing index of the fabric's disaster-recovery
(DR) runbooks. Each linked runbook covers a single failure surface
end-to-end; together they describe the recovery path for every
component the fabric depends on.

## Failure surfaces and their runbooks

| Failure surface                                  | Runbook                                                     |
|--------------------------------------------------|-------------------------------------------------------------|
| Postgres metadata DB lost or corrupted           | [`dr-postgres-restore.md`](./dr-postgres-restore.md)        |
| Source cell unreachable (cross-cell failover)    | [`dr-cross-cell-failover.md`](./dr-cross-cell-failover.md)  |
| Manifest store partially corrupted, gateway up   | [`dr-manifest-resume.md`](./dr-manifest-resume.md)          |
| Continuous in-process DR verification (regression gate) | [`dr-verification.md`](./dr-verification.md)        |

Read the runbook before the failure happens. Each one starts with a
"Pre-conditions" section that lists what the operator must have in
place before recovery is even possible (backups, replication
configuration, monitoring access).

## Published RPO / RTO targets

These are the targets the fabric promises and the verifier exercises
on every CI run. A regression that drives either above the target is
a release blocker.

| Tier                          | RPO target           | RTO target           |
|-------------------------------|----------------------|----------------------|
| Manifest DB (Postgres, PITR)  | 5 minutes (WAL ship) | 30 minutes (restore) |
| Cross-cell async replication  | 60 seconds (lag)     | 5 minutes (failover) |
| Cross-cell sync replication   | 0 (synchronous)      | 5 minutes (failover) |
| Manifest-resume after corruption | 0 (re-reads from provider) | 15 minutes |

The cross-cell async RPO target matches the default
`cross_cell.Replicator.Interval` (60s) plus a tick's worth of slack.
The sync mode is RPO=0 by definition — the PUT path itself does the
copy on the critical path.

## How the verifier maps to the runbooks

The in-process DR verifier
([`tests/dr/verifier.go`](../../tests/dr/verifier.go)) drives the
cross-cell async failover path end-to-end on every CI run. It is the
regression gate for the cross-cell runbook; the other two runbooks
(Postgres restore, manifest resume) are exercised by operator drill
scripts referenced from their respective runbooks (see each runbook's
"DR drill" section).

The verifier writes a JSON report to `artifacts/dr-report.json` that
captures the measured RPO / RTO for the run; CI fails the job if the
measurement exceeds the published target above.
