# DR — Postgres Metadata DB Restore

This runbook covers restoring the Postgres manifest store
(`metadata/manifest_store/postgres`) when the primary instance is
lost or corrupted. The recovery flow is point-in-time recovery (PITR)
from base backups + shipped WAL.

The manifest body is opaque JSONB (or BYTEA when the body encryptor
is configured); the gateway has no other state in Postgres. As long
as the manifest rows come back, the gateway can resume serving every
GET/HEAD/DELETE that addresses a manifest still on disk in the
underlying provider.

## Pre-conditions

Operators MUST have these in place BEFORE a failure happens. If any
is missing the recovery is not possible — this is the first thing
the on-call must check.

1. **Base backups** taken with `pg_basebackup` daily, retained for
   at least 14 days. The base backup directory layout is whatever
   the chosen backup tool produces; `pgbackrest` is the recommended
   tool for the fabric because its incremental + differential
   layout maps cleanly onto a 5-minute RPO target.
2. **WAL archiving** continuous, shipped to the same artifact store
   as base backups. `archive_command` must `cp` (or `pgbackrest
   archive-push`) into the archive on every WAL segment switch.
3. **Restore target host** ready: same Postgres major version, same
   OS, same TLS material, same `postgresql.conf` baseline. The
   gateway's connection-string secret in
   `internal/auth`-injected secrets must be re-pointable to the new
   host's hostname.
4. **Monitoring & alerting** wired so the operator knows the
   primary is down BEFORE customers do. The threshold is "any 5xx
   from the gateway's manifest path for >60s" (see
   [`docs/runbooks/cmk-rotation.md`](./cmk-rotation.md) for the
   companion paging procedure).

## Recovery steps

The restore is sequential — do not skip steps even if a step looks
unnecessary. The validation step at the end is required; without it
the gateway may serve stale rows for hours.

### 1. Stop the primary (if it is partially up)

If the primary is reachable but in a degraded state (replication
slot full, disk full, partial corruption), stop it cleanly so the
WAL archive is fully shipped. If the host is unreachable, skip to
step 2.

```bash
sudo systemctl stop postgresql@17-main
```

### 2. Restore base backup onto the recovery host

Restore the most recent base backup older than the corruption
timestamp. With pgbackrest the command is:

```bash
sudo -u postgres pgbackrest --stanza=fabric \
    --type=time --target="2025-03-15 14:00:00 UTC" \
    --target-action=promote restore
```

The `--target=...` value is the point-in-time the operator wants to
recover to — usually "5 minutes before the corruption was detected"
to maximise the chance the corrupting transaction has not been
replayed.

### 3. Configure recovery and start Postgres

pgbackrest writes the recovery configuration into
`postgresql.auto.conf` automatically. The operator only needs to
start the service and watch the recovery progress:

```bash
sudo systemctl start postgresql@17-main
sudo -u postgres tail -F /var/log/postgresql/postgresql-17-main.log
```

Wait until the log emits `recovery has paused` (when promoting at a
PITR target) or `database system is ready to accept connections`
(when promoting at end-of-WAL). The recovery may take tens of
minutes for terabyte-scale stores.

### 4. Validate the manifest table integrity

Run the validation queries below. Each is designed to surface a
common partial-restore failure mode. Run all three before
re-pointing the gateway.

```sql
-- 4a. Row count plausibility vs. yesterday's baseline. A drop of
-- more than 1% suggests a pre-corruption restore that lost rows.
SELECT count(*) AS row_count, max(updated_at) AS last_updated
FROM manifests;

-- 4b. JSONB body parseability (catches BYTEA→JSONB schema drift
-- after a body-encryptor rotation). Returns zero rows on a healthy
-- store; any returned row indicates a corrupt body column.
SELECT tenant_id, bucket, object_key_hash, version_id
FROM manifests
WHERE jsonb_typeof(body) IS DISTINCT FROM 'object'
LIMIT 100;

-- 4c. Primary-key uniqueness sanity (a partial replay can re-insert
-- previously-deleted rows under the same key). Returns zero rows
-- on a healthy store.
SELECT tenant_id, bucket, object_key_hash, version_id, count(*) c
FROM manifests
GROUP BY 1, 2, 3, 4
HAVING count(*) > 1
LIMIT 100;
```

If 4a is off by more than 1%, do NOT promote the recovery host. The
restore target was wrong — re-run step 2 with an earlier
`--target=...` time.

If 4b returns rows AND the deployment runs with a `BodyEncryptor`,
the body column is BYTEA on this host. The validation in 4b only
applies to deployments where `BodyEncryptor` is nil (JSONB column).

### 5. Re-point the gateway

Update the gateway's Postgres connection string secret to the new
host. The gateway re-reads connection-string secrets on signal —
send SIGHUP to every gateway instance:

```bash
ansible -i deploy/inventory gateways -m shell -a 'kill -HUP $(pgrep gateway)'
```

The gateway will reconnect within ~5s. The first successful manifest
fetch through the gateway marks the end of the RTO clock.

### 6. Validate end-to-end via the conformance runner

Run the s3-conformance runner against the recovered cell:

```bash
go test -count=1 -run TestRunConformance_LocalFSDev ./tests/s3_conformance/...
```

The matrix's PUT, GET, HEAD, LIST, DELETE rows must all be
`Passed`. A `Failed` here is recovery-incomplete; see the matrix
Detail field for the failing op and consult
[`docs/runbooks/s3-conformance.md`](./s3-conformance.md).

### 7. Re-enable WAL archiving on the new primary

A common post-restore mistake is leaving WAL archiving disabled. The
new primary MUST archive WAL or the next failure has no recovery
path. Confirm:

```sql
SHOW archive_mode;       -- should be 'on'
SHOW archive_command;    -- should call pgbackrest archive-push
```

If either is wrong, fix it in `postgresql.conf`, reload (`pg_ctl
reload`), and re-validate by waiting for the next WAL segment
switch (`SELECT pg_walfile_name(pg_current_wal_lsn())`) and
confirming it appears in the archive.

## RPO / RTO measurement

This runbook's RPO target is **5 minutes** — the WAL shipping
cadence. RPO = (time of last archived WAL segment) - (time of
failure).

The RTO target is **30 minutes** — wall-clock from the operator
declaring the primary down to the moment the gateway serves a
successful manifest fetch after the re-point. Most of the time is
consumed by step 3 (recovery replay) on terabyte-scale stores.

After every recovery drill the operator records the measured RPO /
RTO in `docs/dr-drill-log.md` (if absent, create it). The log is
the input to the published numbers in
[`dr.md`](./dr.md#published-rpo--rto-targets).

## DR drill

A drill SHOULD happen at least once per quarter. The drill script
lives at `scripts/dr/postgres-drill.sh` and:

1. Spins up a disposable Postgres instance from a recent base backup.
2. Runs validation queries 4a-4c above.
3. Times the recovery from base-backup-extract to ready-to-serve.
4. Tears down the disposable instance.

If the disposable instance cannot be brought up, the runbook itself
has regressed and the on-call must update it before the drill is
considered passed.
