# Load-test report archive

This directory is where the JSON reports emitted by the load-test
harness ([`tests/benchmark`](../../../tests/benchmark/), CLI at
[`cmd/benchmark-runner`](../../../cmd/benchmark-runner/)) are committed
after a real run, per
[`docs/runbooks/load-testing.md`](../../runbooks/load-testing.md).

It is intentionally empty except for this README until the first real
staging run lands. CI smoke runs (Tier 1) upload their reports as
GitHub Actions artifacts rather than committing them here — only
durable, citable runs (Tier 2 Ceph RGW and Tier 3 Linode + Wasabi)
belong in version control.

## What gets deposited here

Each committed file is a single self-contained JSON document produced
by `benchmark-runner -out=…`. The schema is documented in
[load-testing.md §5](../../runbooks/load-testing.md#5-report-schema);
the top-level shape is:

```jsonc
{
  "suite": "zk-object-fabric-phase2",
  "started_at": "...",
  "finished_at": "...",
  "all_passed": true,
  "scenarios": [
    {
      "name": "put-cache-hit",
      "pass": true,
      "failures": [],
      "results": [ { "metric": "...", "value": 12.4, "histogram": { ... } } ]
    }
  ]
}
```

`all_passed` is the headline gate; per-scenario `failures` and the
per-metric `histogram` carry the detail an operator triages from.

## Naming convention

Reports are named by the harness after the provider and a UTC
timestamp, matching the `-out=` paths in the runbook:

| Tier | Provider | Filename pattern |
|------|----------|------------------|
| Tier 2 (pre-deploy) | Ceph RGW demo | `ceph-rgw-<YYYYMMDDThhmmssZ>.json` |
| Tier 3 (published SLA) | Linode + Wasabi | `linode-wasabi-<YYYYMMDDThhmmssZ>.json` |

The Tier 3 run script
([`deploy/staging/load-driver/scripts/run_tier3.sh`](../../../deploy/staging/load-driver/scripts/run_tier3.sh))
writes the report (plus a `*.verdict.json` from `cmd/tier3-verify`, a
`*.run.log`, and a `*.env.json`) to the load-driver VM under
`/var/lib/zkof-loaddrv/reports`; the operator copies the report JSON
here and records the run in the runbook's
[§6 Report archive](../../runbooks/load-testing.md#6-report-archive)
table.

## What gates on these reports

The **Load testing** item in
[`docs/PROGRESS.md`](../../PROGRESS.md#production-readiness) cannot be
checked off until a passing Tier 3 `linode-wasabi-*.json` report lands
here. CI automates the reduced-scale smoke confidence (see the
`staging-validation` workflow job); the production SLA itself is only
validated by a real Tier 3 run, which requires paid infrastructure and
a human operator.
