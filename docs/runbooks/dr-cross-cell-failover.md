# DR — Cross-Cell Replication Failover

This runbook covers failing over from a source cell to a destination
cell when the source becomes unreachable. The fabric's cross-cell
replicator (`migration/cross_cell`) mirrors manifests + provider
bytes asynchronously; this runbook describes the operator-driven
flip that promotes the destination to primary.

The runbook applies to BOTH async-replicated objects (mode `async`)
and sync-replicated objects (mode `sync`). Sync objects have RPO=0
by definition (the gateway does the copy on the PUT critical path);
async objects have RPO bounded by the replicator's `Interval` plus
its `LagNanos` measurement at the moment of failure.

## Pre-conditions

1. **Replication policy in place**. The relevant manifests must
   carry a non-nil `PlacementPolicy.ReplicationPolicy` with
   `SourceCell`, `DestCell`, and `Mode` set. The verifier asserts
   this on a per-object basis; see
   [`tests/dr/verifier.go`](../../tests/dr/verifier.go).
2. **Destination cell warm**: `cross_cell.Replicator` is running
   continuously between source and dest BEFORE the failure. A cold
   destination requires a separate "initial seeding" path that is
   out of scope for this runbook.
3. **Lag monitoring**: alerts trip when
   `cross_cell.Replicator.LagNanos()` exceeds the published RPO
   for >5 consecutive minutes. The alert rule is in
   `deploy/monitoring/cross-cell-lag.yaml` (TODO if not present —
   add before declaring this runbook production-ready).
4. **DNS / load-balancer flip plan**. The failover changes which
   gateway pool receives customer reads. The operator must know
   the exact LB record / DNS record to mutate. Document it in the
   cell's Terraform module before any drill.

## Decision: failover vs. recovery

A failover is irreversible from the perspective of in-flight PUTs —
any PUT that landed in the source after the snapshot but before the
failover detection is LOST (RPO bound). Before doing the failover,
the operator MUST confirm:

- The source is genuinely down (network split? alert flap?).
- The destination is hot enough to serve traffic (cache warm-up
  done — see `docs/PROGRESS.md` Cache Warming Strategy).
- The most recent successful replicator tick happened within RPO
  bound. If lag was already growing before failure, RPO is worse
  than the published number.

If any of the above is "no", consider waiting 5-15 minutes for the
source to recover before declaring failover. The wrong call here
costs customer data.

## Failover steps

### 1. Confirm source is unreachable

```bash
# From a host outside the source cell's network:
curl -sS --max-time 5 https://source-cell.gateway.example.com/healthz
# If this returns 200 within 5s, the source is up and you should NOT
# fail over. Investigate the alert before proceeding.
```

If the source is partially up (some hosts answer, others don't),
treat it as down for failover purposes — partial split-brain is
worse than failover, because two cells writing under the same
manifest store creates conflict.

### 2. Stop the source-side replicator

If the source is reachable on the control plane (just not the data
plane), SIGTERM the replicator process so it does not race the
failover:

```bash
ssh source-cell-control.example.com 'pkill -TERM cross-cell-replicator'
```

If unreachable, skip this step; the replicator is dead by definition.

### 3. Capture the last-known replicator lag

The replicator exposes `LagNanos()` via the metrics scrape on
`:9090/metrics`. The metric name is `cross_cell_lag_nanos`. Capture
the most recent value BEFORE the failure:

```bash
curl -sS https://prom.example.com/api/v1/query \
    --data-urlencode 'query=cross_cell_lag_nanos{cell="source-cell"}' \
    | jq -r '.data.result[0].value[1]'
```

Convert to a duration: `(value in nanoseconds) / 1e9` seconds. This
is the RPO upper bound for the failover.

### 4. Promote the destination cell

Update the LB / DNS record so the customer-facing endpoint resolves
to the destination cell's gateway pool. The exact command depends
on the chosen LB; the canonical layout is Linode NodeBalancers (per
[`docs/runbooks/beta-onboarding.md`](./beta-onboarding.md)):

```bash
# Replace the source-cell backend pool with the dest-cell pool:
linode-cli nodebalancers config-update <NB_ID> <CONFIG_ID> \
    --backends '[{"ip":"<dest-cell-gateway-1>:443"},
                 {"ip":"<dest-cell-gateway-2>:443"}]'
```

The flip is atomic from the LB perspective. The cutover happens
within ~30 seconds (default health-check interval); customers see
errors during that window. The RTO clock starts at the moment of
this command.

### 5. Confirm reads succeed against the destination

Drive a synthetic GET against the customer-facing endpoint as soon
as the LB health check turns green:

```bash
aws --endpoint-url https://customer.example.com s3 cp \
    s3://known-good-bucket/known-good-key /tmp/dr-probe
```

The RTO clock stops at the moment this command returns 200 with a
non-empty body.

### 6. Disable cross-cell replication from the old source

Any cross_cell.Replicator instance still configured with the failed
source MUST be stopped: a future "the source came back" recovery
must not accidentally re-replicate from the stale source to the new
destination (which is now the source-of-truth).

```bash
# In every still-running replicator host:
sudo systemctl stop cross-cell-replicator
sudo systemctl disable cross-cell-replicator
```

The replicator can be re-enabled later when the operator is ready
to set up a new destination for the new source. That is a
separate workflow (see [`docs/runbooks/beta-onboarding.md`](./beta-onboarding.md)).

### 7. Document the incident

Record the following in the DR drill log
(`docs/dr-drill-log.md`):

- Wall-clock failover start (LB command time)
- Wall-clock first-successful-GET time
- Measured RTO = (b) - (a)
- Captured replicator lag at failure (from step 3)
- Measured RPO = lag at failure
- Number of objects affected (estimable from PUT rate * RPO)
- Any deviation from this runbook

## RPO / RTO measurement

This runbook's published targets are:

- **Async mode RPO**: 60 seconds (replicator default `Interval`).
- **Sync mode RPO**: 0 (synchronous PUT-path copy).
- **RTO**: 5 minutes (LB flip + cache warm + first successful GET).

The verifier ([`tests/dr/verifier.go`](../../tests/dr/verifier.go))
exercises both numbers on every CI run. A regression that drives
either above the target fails the verifier's RTOTarget gate.

## DR drill

The cross-cell drill runs automatically in CI via the verifier; see
[`dr-verification.md`](./dr-verification.md) for how it is wired.
The operator-facing drill is run quarterly:

1. Pick a non-customer-bucket.
2. PUT 10k objects under the bucket with replication policy.
3. Wait for `LagNanos < 60s`.
4. Simulate a source failure (firewall the source cell).
5. Run the steps above, timing each.
6. Compare measured RPO/RTO to the published targets.

If the measured RTO exceeds 5 minutes for a non-pathological run,
the runbook itself needs work — file a P1 against the on-call
team's backlog.
