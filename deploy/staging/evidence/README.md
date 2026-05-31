# Tier 3 staging evidence dossier — layout contract

Each Tier 3 staging run produces one directory under
`deploy/staging/evidence/<UTC-timestamp>-<gateway-sha-prefix>/`,
assembled by [`../scripts/collect_evidence.sh`](../scripts/collect_evidence.sh)
or `make tier3-evidence`.

The directory layout is part of the audit contract — when the
WS2.4 audit hand-off package bundles this dossier, the layout is
what auditors expect.

## Layout

```
deploy/staging/evidence/20260530T130000Z-deadbeef1234/
├── 00-environment.json               # run-level provenance
├── 01-load-driver/
│   ├── env.json                      # captured by run_tier3.sh
│   ├── report.json                   # benchmark-runner JSON report
│   ├── verdict.json                  # tier3-verify JSON verdict
│   └── run.log                       # stdout/stderr of run_tier3.sh
├── 02-gateway-journals/
│   ├── node-01-<host>.log            # journalctl -u zk-gateway over run window
│   ├── node-01-<host>.health.json    # /internal/health snapshot
│   ├── node-01-<host>.metrics.prom   # Prometheus /metrics scrape
│   └── ...                           # one set per gateway node
├── 03-wasabi-access-logs/            # aws s3 sync from the logs bucket
├── 04-nodebalancer/
│   └── ...                           # Linode API outputs (operator-attached)
└── MANIFEST.txt                      # SHA-256 of every file in the dossier
```

Plus the tarball, which is the actual shipping artifact:

```
deploy/staging/evidence/
├── 20260530T130000Z-deadbeef1234.tar.gz
└── 20260530T130000Z-deadbeef1234.tar.gz.sha256
```

## File-by-file contract

### `00-environment.json`

```json
{
  "dossier_timestamp": "20260530T130000Z",
  "gateway_sha": "deadbeef1234...",
  "gateway_nodes": "gw1.example,gw2.example,gw3.example",
  "load_driver_host": "loaddrv.example",
  "staging_bucket": "zkof-us-east-1-staging",
  "wasabi_endpoint": "https://s3.us-east-1.wasabisys.com",
  "wasabi_region": "us-east-1",
  "wasabi_log_bucket": "zkof-us-east-1-staging-logs",
  "run_window_start": "2026-05-30T12:00:00Z",
  "run_window_end": "2026-05-30T13:00:00Z",
  "collected_by": "alice@workstation"
}
```

### `01-load-driver/`

- `env.json` — written by `run_tier3.sh` at the start of the run.
  Captures gateway endpoint, gateway SHA, harness parameters,
  binary SHA-256 of both `benchmark-runner` and `tier3-verify`.
  This is the most-cited file for "what was run" provenance.
- `report.json` — the `benchmark.Report` JSON the runner emits.
  Auditors verify this against `report.MANIFEST.txt`'s hash to
  confirm no post-hoc tampering. The structured fields are
  documented in `tests/benchmark/runner.go` (`Report`,
  `ReportScenario`, `Result`).
- `verdict.json` — the `tier3verify.Verdict` JSON the verifier
  emits. This is the **gate**: a top-level
  `"all_passed": true` plus `"report_claim_all_passed": true`
  with every per-scenario `pass: true` is the only configuration
  that promotes the gateway build. Any deviation requires
  triage.
- `run.log` — stdout+stderr of `run_tier3.sh`. Includes any
  warnings the harness emitted (e.g.
  `skipped_op_fraction > threshold` warnings, retry storms, etc.)
  that don't show up in the structured report.

### `02-gateway-journals/`

One triplet per gateway node:

- `node-NN-<host>.log` — `journalctl -u zk-gateway --since
  $RUN_WINDOW_START --until $RUN_WINDOW_END --no-pager`. The
  audit checks for: error/panic lines, repair-queue activity,
  cache eviction storms, slowloris-shield activations.
- `node-NN-<host>.health.json` — snapshot of
  `/internal/health`. Reflects steady-state at collection time.
- `node-NN-<host>.metrics.prom` — Prometheus `/metrics` scrape.
  Reflects steady-state at collection time. The histogram
  metrics here cross-reference the benchmark report's per-
  scenario histograms.

### `03-wasabi-access-logs/`

Synced from the per-region staging logs bucket
(`zkof-<region>-staging-logs`). Wasabi access logging must have
been enabled on the bucket **before** the run started; otherwise
this directory is empty and the dossier is incomplete (see
`collect_evidence.sh` Stage 3 fallback README).

### `04-nodebalancer/`

Operator-attached. The `collect_evidence.sh` script lays out a
README pointing at the `linode-cli` invocations needed to
populate this directory; full automation is intentionally
deferred because it requires the Linode API token to live on the
operator workstation (the load driver does not have it).

### `MANIFEST.txt`

```
<sha256>  ./00-environment.json
<sha256>  ./01-load-driver/env.json
<sha256>  ./01-load-driver/report.json
<sha256>  ./01-load-driver/verdict.json
<sha256>  ./01-load-driver/run.log
<sha256>  ./02-gateway-journals/node-01-<host>.log
... etc ...
```

Auditors verify with:

```bash
( cd <dossier_dir> && sha256sum -c MANIFEST.txt )
```

Every line must report `OK`. A single `FAILED` line means the
dossier was tampered with after collection.

## Lifecycle

Dossiers are immutable once produced. The `MANIFEST.txt` + the
tarball SHA-256 anchor the contents. Re-running a stage of the
pipeline produces a NEW dossier directory with a new timestamp;
the previous dossier must be retained for the audit retention
window (typically the audit firm specifies — usually >= 12 months
post-engagement).

For the WS2.4 audit hand-off package, the most recent passing
dossier tarball is included verbatim. Failing dossiers are
included separately under a `incidents/` subdirectory with an
attached incident report.
