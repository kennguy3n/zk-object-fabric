# Load Testing Runbook

This runbook documents the **published procedure** for measuring
zk-object-fabric latency, throughput, and error rate against the SLA
targets the benchmark suite enforces.

The targets are enforced by code, not aspiration — every benchmark
scenario in [`tests/benchmark/suite.go`](../../tests/benchmark/suite.go)
declares a `Target` per metric, and the runner fails the scenario if
the measured value violates the target. CI gates on a smoke-sized run
against `local_fs_dev` (see `.github/workflows/ci.yml`).

| SLA gate                                | Constant in code           | Target          |
|-----------------------------------------|----------------------------|-----------------|
| PUT p99 latency (cache hit)             | `TargetPutP99CacheHitMs`   | ≤ 50 ms         |
| PUT p99 latency (Wasabi origin)         | `TargetPutP99OriginMs`     | ≤ 200 ms        |
| GET p99 latency (L0 / memory cache)     | `TargetGetP99L0Ms`         | ≤ 20 ms         |
| GET p99 latency (L1 / NVMe cache)       | `TargetGetP99L1Ms`         | ≤ 100 ms        |
| GET p99 latency (Wasabi origin miss)    | `TargetGetP99OriginMs`     | ≤ 300 ms        |
| Sustained throughput per gateway node   | `TargetSustainedRPS`       | ≥ 10 000 req/s  |
| Per-request error rate (sustained)      | `TargetErrorRateMax`       | ≤ 1e-3          |
| Offered-load efficiency                 | `TargetRPSEfficiencyMin`   | ≥ 0.95          |

---

## 1. Components

- **Driver**: [`tests/benchmark`](../../tests/benchmark/) — `SustainedRunner`
  drives concurrent PUT/GET/HEAD/DELETE/LIST against any
  `providers.StorageProvider`, with token-bucket rate limiting and
  per-worker HDR histograms (see `histogram.go`).
- **CLI**: [`cmd/benchmark-runner`](../../cmd/benchmark-runner/) — builds a
  provider from CLI flags + env vars, runs `benchmark.DefaultSuite()`,
  writes a JSON report, and exits non-zero on any target failure.
- **Suite**: `DefaultSuite()` declares ten scenarios that gate each of
  the published SLA targets independently (`put-cache-hit`, `put-origin`,
  `get-l0-cache-hit`, `get-l1-cache-hit`, `get-origin`,
  `sustained-throughput-10k-rps`, `cache-hit-ratio-hot`,
  `wasabi-origin-egress-ratio`, `list-performance-*`).
- **CI smoke**: GitHub Actions job `load-test-smoke` runs the harness
  against `local_fs_dev` with short overrides on every PR.

---

## 2. Tier 1: CI smoke run (every PR)

The smoke run exists to keep the harness honest — it would catch a
regression that, for example, blows up the rate limiter or accidentally
sets a target to 0. It does **not** validate the production SLA itself
(it runs against a local filesystem and at 1% of target RPS).

```bash
go build ./cmd/benchmark-runner
./benchmark-runner \
  -provider=local_fs_dev \
  -duration=2s -rps=200 -seed-objects=64 \
  -max-object-bytes=4096 \
  -out=load-smoke.json
```

The `-scenario` flag is a **single substring filter**, not a
comma-separated list (see `cmd/benchmark-runner/main.go:selectSuite`).
To exercise more than one scenario in the smoke phase, the CI workflow
invokes the harness once per scenario (e.g.
`-scenario=put-cache-hit`, then `-scenario=get-l0-cache-hit`, …).
Each invocation produces an independent JSON report that downstream
job steps can aggregate.

If `load-smoke.json.AllPassed` is `false`, CI fails. The failing
scenarios appear in the JSON under `scenarios[].failures`.

---

## 3. Tier 2: local Ceph RGW demo run (pre-deploy)

Before pushing a new gateway build to the Linode staging environment,
run the harness against the local Ceph RGW demo cluster
(`docker compose up ceph-rgw` from `deploy/demo/`):

```bash
./benchmark-runner \
  -provider=ceph_rgw \
  -rgw-endpoint=http://localhost:8888 \
  -rgw-bucket=bench \
  -rgw-region=default \
  -rgw-access-key=$RGW_KEY \
  -rgw-secret-key=$RGW_SECRET \
  -duration=10m \
  -rps=2000 \
  -out=docs/reports/load/ceph-rgw-$(date -u +%Y%m%dT%H%M%SZ).json
```

This run validates that the harness can saturate an S3-compatible
backend at multi-thousand-RPS without leaking goroutines, file
descriptors, or memory.

Commit the JSON report under `docs/reports/load/` and link it from
this runbook's [§6 Report archive](#6-report-archive).

---

## 4. Tier 3: Linode + Wasabi staging gateway

This is the **published SLA validation** run. It uses the production
gateway binary, the production cache layout, and a Wasabi origin in
the target region.

```bash
# From a dedicated load-driver VM in the same region as the gateway.
./benchmark-runner \
  -provider=wasabi \
  -wasabi-endpoint=https://s3.us-east-1.wasabisys.com \
  -wasabi-bucket=$STAGING_BUCKET \
  -wasabi-region=us-east-1 \
  -wasabi-access-key=$WASABI_KEY \
  -wasabi-secret-key=$WASABI_SECRET \
  -duration=1h \
  -rps=12000 \
  -concurrency=128 \
  -seed-objects=10000 \
  -out=docs/reports/load/linode-wasabi-$(date -u +%Y%m%dT%H%M%SZ).json
```

Acceptance criteria (each enforced by the harness — no manual eyeballing):

1. `AllPassed == true` in the report.
2. `scenarios[].results[].value` for every `*_p99` metric is within
   the corresponding `Target*` constant.
3. `sustained_rps >= 10 000` for `sustained-throughput-10k-rps`.
4. `error_rate <= 1e-3` for `sustained-throughput-10k-rps`.
5. `rps_efficiency >= 0.95` for `sustained-throughput-10k-rps`.

If any criterion fails, **do not promote** the gateway build. File an
incident, attach the failed report, and triage from the per-scenario
histograms in the JSON.

### 4.1 Deployment artifacts

The staging infrastructure is codified in
[`deploy/staging/`](../../deploy/staging/README.md):

- **Load driver Terraform** — `deploy/staging/load-driver/terraform/`
  provisions a Linode VM in the same region as the gateway fleet.
- **Run script** — `deploy/staging/load-driver/scripts/run_tier3.sh`
  executes the canonical invocation above and feeds the report to the
  Tier 3 verifier (`cmd/tier3-verify`).
- **Evidence collector** — `deploy/staging/scripts/collect_evidence.sh`
  pulls the report, verdict, gateway journals, Wasabi access logs, and
  health snapshots into a SHA-anchored evidence dossier for the audit
  package.
- **Verifier** — `cmd/tier3-verify` re-applies the acceptance criteria
  independently of the runner. It reads a benchmark-runner JSON report
  and produces a structured `Verdict` JSON (package `tests/tier3verify`)
  where `all_passed: true` is the gate.

The full runbook — prerequisites, step-by-step deployment, and
teardown — is in
[`deploy/staging/README.md`](../../deploy/staging/README.md).

---

## 5. Report schema

The harness emits a single JSON document per run. Top-level shape:

```jsonc
{
  "suite": "zk-object-fabric-benchmark",
  "started_at": "...",
  "finished_at": "...",
  "all_passed": true,
  "scenarios": [
    {
      "name": "put-cache-hit",
      "pass": true,
      "failures": [],
      "results": [
        {
          "metric": "put_latency_p99_cache_hit",
          "value": 12.4,
          "duration_ns": 60000000000,
          "labels": { "scenario": "...", "unit": "ms", ... },
          "histogram": {
            "count": 60000,
            "min_ns": 800000,
            "mean_ns": 4100000,
            "max_ns": 47100000,
            "p50_ns": 3200000,
            "p95_ns": 9800000,
            "p99_ns": 12400000,
            "p999_ns": 22100000,
            "buckets": [ { "upper_bound_ns": ..., "count": ... }, ... ]
          }
        }
      ]
    }
  ]
}
```

Notes on interpretation:

- `value` is always in the unit declared by the scenario's `Target.Unit`
  (e.g. `ms` for latencies, `req/s` for throughput, `ratio` for error
  rates and hit ratios).
- `histogram` is present only for latency-shaped metrics. The
  `buckets` array is HDR-style log2 bucketing with 256 linear
  sub-buckets per magnitude — worst-case relative error on the
  reported percentile is ~0.4 %.
- `histogram.count` is the per-metric op count; sum across scenarios
  to get the total request count for the run.

---

## 6. Report archive

Once a real-deployment run lands, commit the JSON report to
`docs/reports/load/` and append a row here:

| Date (UTC)        | Environment              | Build SHA | Report |
|-------------------|--------------------------|-----------|--------|
| _none yet_        |                          |           |        |

The Linode + Wasabi staging report is the canonical artifact that
demonstrates the gateway meets its load-testing acceptance criteria
on a real cloud deployment.
