# ZK Object Fabric — Capacity Dossier

- **Audience**: external auditors, prospective customers, on-call
  operators planning regional rollout.
- **Status**: dossier; numeric targets in §2 are machine-enforced
  by `cmd/benchmark-runner` against every sustained-load run.
- **Source of truth**: <a href="../tests/capacity/targets.go">`tests/capacity/targets.go`</a>.
  Drift between this document and the constants is caught by
  <a href="../tests/capacity/targets_test.go">`tests/capacity/targets_test.go`</a> and
  <a href="../tests/capacity/dossier_test.go">`tests/capacity/dossier_test.go`</a>.

This document collects in one place every numeric target the
gateway commits to: performance, S3 protocol limits, per-cell sizing,
availability, and the operational metrics that are still open. It is
the canonical capacity reference — the per-region capacity envelope,
the S3 protocol limits, and an explicit list of open operational
targets (so a reviewer never has to guess whether a missing number
is an oversight or an open question).

If a number in this document does not match the corresponding
constant in `tests/capacity/targets.go`, the document is stale —
re-grep `tests/capacity/targets_test.go` to confirm the canonical
value and update this file in the same commit.

---

## §1. Scope and non-goals

**In scope.** Every number that is either (a) machine-enforced by
existing gating code (the benchmark suite, the Tier 3 verifier, the
gateway's quota and rate-limit middleware) or (b) inherited
non-negotiably from the S3 protocol surface.

**Out of scope.** Theoretical durability nines. Per
[`docs/PROPOSAL.md`](PROPOSAL.md) §11.4 ("Anti-patterns to avoid"):

> **Publish theoretical "eleven nines" durability** — Cannot be validated by analysis. Only publish measured durability from chaos tests.

The only durability number that ships with an audit bundle is the
measured durability the chaos suite produces under the specific
gateway build under audit; that number is recorded in the chaos
report, not in this dossier.

---

## §2. Performance targets

These are the values enforced by `cmd/benchmark-runner` against
every sustained-load run (see
[`docs/runbooks/load-testing.md`](runbooks/load-testing.md)). A run
that violates any one of them fails the load-test gate.

| Metric                                              | Target                | Constant                                              | Enforced by                                                       |
| --------------------------------------------------- | --------------------- | ----------------------------------------------------- | ----------------------------------------------------------------- |
| PUT p99 latency (hot-tier cache hit on the way out) | ≤ 50 ms               | `capacity.PutP99CacheHitMs`                           | `cmd/benchmark-runner`, `tests/benchmark/suite.go`                |
| PUT p99 latency (Wasabi origin)                     | ≤ 200 ms              | `capacity.PutP99OriginMs`                             | `cmd/benchmark-runner`, `tests/benchmark/suite.go`                |
| GET p99 latency (L0 / memory cache hit)             | ≤ 20 ms               | `capacity.GetP99L0Ms`                                 | `cmd/benchmark-runner`, `tests/benchmark/suite.go`                |
| GET p99 latency (L1 / NVMe disk cache hit)          | ≤ 100 ms              | `capacity.GetP99L1Ms`                                 | `cmd/benchmark-runner`, `tests/benchmark/suite.go`                |
| GET p99 latency (Wasabi origin miss)                | ≤ 300 ms              | `capacity.GetP99OriginMs`                             | `cmd/benchmark-runner`, `tests/benchmark/suite.go`                |
| Sustained throughput per gateway node               | ≥ 10 000 req/s        | `capacity.SustainedRPS` / `capacity.PerGatewayNodeSustainedRPS` | `cmd/benchmark-runner`, `tests/benchmark/suite.go`                |
| Per-request error rate (sustained load)             | ≤ 1 × 10⁻³            | `capacity.ErrorRateMax`                               | `cmd/benchmark-runner`, `tests/benchmark/suite.go`                |
| Offered-load efficiency (attained / target)         | ≥ 0.95                | `capacity.RPSEfficiencyMin`                           | `cmd/benchmark-runner`, `tests/benchmark/suite.go`                |
| Linode L0/L1 cache hit ratio (Hot tier)             | > 0.90                | `capacity.CacheHitRatioHotMin`                        | `cmd/benchmark-runner`, `tests/benchmark/suite.go`                |
| Wasabi origin egress ratio (egress ÷ stored)        | ≤ 1.0 per tenant      | `capacity.WasabiOriginEgressRatioMax`                 | `cmd/benchmark-runner`, `tests/benchmark/suite.go`                |

The Tier 3 staging verifier
([`cmd/tier3-verify`](../cmd/tier3-verify/main.go)) consumes the JSON
output of `cmd/benchmark-runner` and
asserts every one of these targets before declaring a deployment
audit-ready. A staging run that produces no benchmark report at all
fails the verifier with a non-zero exit code (see
[`docs/runbooks/load-testing.md`](runbooks/load-testing.md) §"Tier
3 verifier").

---

## §3. S3 protocol capacity envelope

These limits come from the AWS S3 reference documentation
([`qfacts.html`](https://docs.aws.amazon.com/AmazonS3/latest/userguide/qfacts.html))
and are non-negotiable: any client built against the S3 SDK assumes
them. The gateway enforces them in
`api/s3compat/multipart_handler.go` and surfaces violations as the
standard S3 error codes (`EntityTooLarge`, `InvalidPart`,
`InvalidPartOrder`).

| Limit                            | Value     | Constant                              |
| -------------------------------- | --------- | ------------------------------------- |
| Maximum single-object size       | 5 TiB     | `capacity.MaxObjectSizeBytes`         |
| Maximum part-number              | 10 000    | `capacity.MaxMultipartParts`          |
| Minimum part size (non-final)    | 5 MiB     | `capacity.MinMultipartPartSizeBytes`  |
| Maximum part size                | 5 GiB     | `capacity.MaxMultipartPartSizeBytes`  |

---

## §4. Per-gateway-node capacity envelope

The gateway node is the unit of horizontal scaling. Per the
benchmark gate, a single gateway node must sustain ≥ 10 000 req/s.
A region's aggregate request capacity is therefore approximately:

```
RegionRPS ≈ PerGatewayNodeSustainedRPS × NumGatewayNodes
        ≈ 10 000 × N
```

This is the planning number that operators use when sizing a new
cell. It is a lower bound: a gateway node that does not sustain
10 000 req/s under the standard load profile fails the
benchmark gate and is not eligible for production rollout.

---

## §5. Per-cell capacity envelope

A "cell" is the regional unit defined in
[`docs/PROPOSAL.md`](PROPOSAL.md) §6 — an independent metadata
shard plus its own gateway fleet, repair queues, and storage
backend. The cell sizing bounds:

| Limit                              | Value                       | Constant                              |
| ---------------------------------- | --------------------------- | ------------------------------------- |
| Minimum usable storage per cell    | 2 PiB                       | `capacity.MinCellUsableCapacityBytes` |
| Maximum usable storage per cell    | 20 PiB                      | `capacity.MaxCellUsableCapacityBytes` |

Why the bounds (from
[`docs/PROPOSAL.md`](PROPOSAL.md) §6.2): "Below 2 PB the per-cell
overhead dominates; above 20 PB repair and failure domains get
unwieldy." A region that needs more than 20 PiB usable is sized as
multiple cells, not as one oversized cell. The cross-cell topology
is described in [`docs/PROPOSAL.md`](PROPOSAL.md) §6.5.

Per-cell gateway-node count is operationally bounded by the L7
load-balancer fan-out and the metadata shard's connection ceiling,
not by an architectural number; cell sizing in practice is driven
by `RegionRPS` from §4 plus a 30% headroom for cache warm-up.

---

## §6. Per-tenant capacity envelope

Per-tenant capacity is configured per-tenant in the tenant record
(see [`metadata/tenant`](../metadata/tenant/)) and is not committed
as a single global number in this dossier. The two operationally
significant quotas:

- **Egress budget**: a TB-per-month ceiling on Wasabi-origin egress
  per tenant. Enforced by `internal/auth/abuse.go`
  (`tenantEgressBudgetBytes`) and surfaced in tenant records as
  `Budgets.EgressTBMonth`. The dossier-level ceiling is
  `capacity.WasabiOriginEgressRatioMax` (egress ÷ stored ≤ 1.0
  per tenant per month).
- **Request-rate limit**: a per-tenant token-bucket RPS limit
  enforced by `internal/auth/rate_limit.go`. Default values are
  documented in [`docs/runbooks/tenant-setup.md`](runbooks/tenant-setup.md);
  the dossier does not pin them because the quota envelope is
  business-policy-driven, not architectural.

---

## §7. Reliability targets

Only one nines-style target is committed in this dossier:
**availability**, derived directly from the error-rate ceiling.

| Metric                                | Target           | Constant                          |
| ------------------------------------- | ---------------- | --------------------------------- |
| Availability (1 − error rate)         | ≥ 99.9%          | `capacity.AvailabilityFractionMin` (= 1 − `ErrorRateMax`) |

Availability is measurable from the gateway's own
request-success counters during a sustained-load run; the
`load-test-smoke` CI job exercises the gate (≥ 99.9% across
~1 000 requests) on every commit.

**Durability** is intentionally NOT listed as a target. Per
[`docs/PROPOSAL.md`](PROPOSAL.md) §11.4, only chaos-measured
durability is published, and only against the specific build under
audit. The chaos suite ([`tests/chaos`](../tests/chaos))
records observed durability under single-node loss, zone loss,
metadata-DB failover, provider-side outage, and cache partition.
That report ships in the audit bundle ([§9](#9-cross-reference--enforcement-map)) and is the only durability
number that should appear in any external communication.

---

## §8. Open operational targets

These metrics are not yet machine-enforced. They are explicitly
enumerated here so an auditor sees the gap as a known, tracked gap
and does not mistake the absence for an oversight.

| Metric                                       | Unit        | Gated on          | Source-of-truth gate (once closed)                            |
| -------------------------------------------- | ----------- | ----------------- | ------------------------------------------------------------- |
| Repair time (single node loss)               | hours       | Hybrid / owned-DC | To be added to `tests/chaos` measurement reports              |
| Storage COGS / TB-month (local DC)           | USD         | Owned-DC          | To be added to operator cost dossier (separate from this one) |
| Migration throughput (Wasabi → local cell)   | bytes / sec | Hybrid            | To be added to `migration/` runbook + chaos report            |

When a target lands here, the canonical constant moves into
`tests/capacity/targets.go` and `tests/capacity/dossier_test.go`
enforces that the entry leaves the "Open" subsection above. The
`OpenOperationalTargets()` helper in `tests/capacity/targets.go`
enumerates the same list so other parts of the codebase (audit
bundle, runbooks) can reference the gaps from one source.

---

## §9. Cross-reference / enforcement map

Per-target trace from the dossier through the enforcement gate to
the audit-bundle artifact:

| Dossier section            | Enforcement gate                                       | Audit-bundle artifact                                                |
| -------------------------- | ------------------------------------------------------ | -------------------------------------------------------------------- |
| §2 Performance             | `cmd/benchmark-runner` + `cmd/tier3-verify`            | `reports/load/{timestamp}.json` (raw) + `tier3-verify` exit summary  |
| §3 S3 protocol             | `api/s3compat/multipart_handler.go`                    | `tests/s3_compat/` results, plus external conformance matrix         |
| §4 Per-gateway-node        | `cmd/benchmark-runner` `SustainedRPS` gate             | Same as §2                                                           |
| §5 Per-cell                | Operator runbook (cell-sizing dossier)                 | `deploy/staging/evidence/{timestamp}/cell-sizing.md`                 |
| §6 Per-tenant              | `internal/auth/rate_limit.go`, `internal/auth/abuse.go` | Tenant record dump + abuse-trip log                                  |
| §7 Reliability             | `cmd/benchmark-runner` + `cmd/tier3-verify`            | Same as §2; chaos report for durability                              |
| §8 Open                    | n/a (gap)                                              | Listed in audit-bundle "Known limitations" section                   |

The audit bundle is assembled by `make audit-bundle`. Each row of
the table above corresponds to a directory in the bundle's MANIFEST.

---

## §10. Change procedure

1. Edit the constant in `tests/capacity/targets.go`.
2. Re-run `go test -race -count=1 ./tests/capacity/...`. The pinned
   tests in `targets_test.go` will fail until you update the
   matching expected value, AND the doc-grep test in
   `dossier_test.go` will fail until you update the corresponding
   row in this document.
3. Update the row in §2-§8 above with the new value AND the
   commit-level rationale (link to the measurement report, the
   business decision, or the upstream protocol spec).
4. If the change closes an open target from §8, also remove the
   entry from `OpenOperationalTargets()` in
   `tests/capacity/targets.go` AND update the §8 table.

Every dossier change is therefore reviewable as a single PR diff
where the constant, the test, the documented value, and the
rationale all move together.
