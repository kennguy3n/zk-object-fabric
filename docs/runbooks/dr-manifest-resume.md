# DR — Manifest-DB Restore and Resume

This runbook covers the case where the gateway is still up but the
manifest store has been partially corrupted: some manifests are
missing, dangling, or point at pieces that no longer exist in the
provider. The recovery path is reconcile-and-resume — the underlying
pieces are still on the provider (S3, Wasabi, or local NVMe), so the
gateway can resume serving as soon as the manifest store is
reconciled with the provider's actual content.

This is a more frequent failure mode than full Postgres loss: an
errant migration, a botched manual UPDATE, a partial WAL replay, or
a malformed manifest body encryptor rotation can leave the manifest
store in an inconsistent state while the gateway and provider are
otherwise healthy.

## Pre-conditions

1. **The provider is intact**. This runbook assumes the underlying
   storage backend (S3, Wasabi, local NVMe) still has every piece
   it had before the manifest store was damaged. If pieces are also
   missing, this is a "both halves lost" scenario and the recovery
   is full-restore-from-backup (see
   [`dr-postgres-restore.md`](./dr-postgres-restore.md)).
2. **A recent manifest backup is available**. Even a 24-hour-old
   `pg_dump` is enormously useful because it bounds the
   reconciliation work to "manifests written since the dump".
3. **The gateway can be put in read-only mode** to prevent further
   PUTs from corrupting the now-inconsistent store mid-recovery.

## Identifying the failure mode

Three categories of inconsistency are recoverable. Each has its own
sub-flow below.

| Inconsistency                       | Symptom                                                                 |
|-------------------------------------|-------------------------------------------------------------------------|
| Dangling manifest                   | Manifest row exists; `Pieces[*].PieceID` 404s in the provider.          |
| Orphan piece                        | Piece exists in the provider; no manifest references it.                |
| Stale latest-version index          | `manifest_store.List` returns rows for keys whose latest version is gone. |

The conformance runner is the fastest signal for category 1; a
provider-side audit script (see step 3 below) is the signal for
category 2.

## Recovery steps

### 1. Put the gateway in read-only mode

The gateway honours a `--read-only` flag that 405s every mutating
S3 operation. Set it and bounce every gateway instance:

```bash
ansible -i deploy/inventory gateways -m shell -a \
    'sed -i "s/^READ_ONLY=.*/READ_ONLY=true/" /etc/gateway.env && systemctl restart gateway'
```

Confirm by issuing a PUT — the gateway must return 405
MethodNotAllowed.

### 2. Take a manifest store dump for forensic comparison

Before mutating the store, capture the current state so the
reconciliation can be audited. Even a partial dump is valuable.

```bash
sudo -u postgres pg_dump \
    --table manifests --data-only --column-inserts \
    fabric > /var/backups/manifests-pre-resume-$(date +%s).sql
```

If the table is huge (>10 GB), use `COPY ... TO STDOUT` with a
compressed pipe instead.

### 3. Run the audit script to identify dangling manifests

The audit script walks the manifest table, fetches every
referenced piece's HEAD from the provider, and emits a CSV of
`(tenant_id, bucket, object_key_hash, version_id, piece_id, status)`
where `status` is `ok`, `404`, or `error`. The script lives at
`scripts/dr/manifest-audit.sh` (TODO: ship in a follow-up PR if not
present; without it this step can be performed manually via psql
+ `aws s3api head-object` in a loop, but that is operationally
painful).

```bash
sudo -u postgres ./scripts/dr/manifest-audit.sh \
    --conn 'host=localhost dbname=fabric' \
    --provider-endpoint https://wasabi.example.com \
    > /var/log/manifest-audit-$(date +%s).csv
```

Rows with `status=404` are dangling manifests; rows with
`status=error` are inconclusive (retry the audit before declaring
recovery).

### 4. Delete dangling manifests

For each dangling manifest, issue a per-row DELETE so the manifest
store stops returning them on GET/LIST. The gateway's
`ListObjectVersions` handler will then no longer surface them; new
PUTs under the same key will not be blocked by the orphan row.

```sql
BEGIN;
-- Replace the placeholder with the CSV from step 3.
DELETE FROM manifests
WHERE (tenant_id, bucket, object_key_hash, version_id) IN (
    -- Paste rows here.
);
-- Validate the affected row count BEFORE committing.
COMMIT;
```

### 5. Reconcile orphan pieces

Orphan pieces are not customer-visible but they cost storage. The
reconciliation depends on the provider:

- **S3-compatible providers** (Wasabi, R2, MinIO): the gateway
  writes pieces under `pieces/<piece_id>`. Run an inventory job
  against that prefix and diff against the manifest store's
  `Pieces[*].PieceID` column. Orphans can be safely deleted; back
  them up to an "orphan-quarantine" prefix first in case the audit
  was wrong.
- **Local NVMe** (`providers/local_fs_dev`): orphans live as plain
  files under `<root>/pieces/`. Same diff procedure; quarantine
  before delete.

A misclassified "orphan" (i.e. the manifest exists but the audit
missed it) is silent data loss, so always quarantine first.

### 6. Take the gateway out of read-only mode

After the manifest store is reconciled, lift read-only:

```bash
ansible -i deploy/inventory gateways -m shell -a \
    'sed -i "s/^READ_ONLY=.*/READ_ONLY=false/" /etc/gateway.env && systemctl restart gateway'
```

The RTO clock stops at the moment the first PUT succeeds after
this command.

### 7. Verify with the conformance runner

Run the conformance runner against the recovered store:

```bash
go test -count=1 -run TestRunConformance_LocalFSDev ./tests/s3_conformance/...
```

Any `Failed` row is recovery-incomplete. See the matrix Detail
field and consult
[`docs/runbooks/s3-conformance.md`](./s3-conformance.md).

## RPO / RTO measurement

This runbook's RPO target is **0**: the underlying provider still
has every piece, and the reconciliation re-establishes the manifest
rows. No customer data is lost; the failure mode is
availability-only.

The RTO target is **15 minutes**, dominated by the audit script's
walk over the manifest table. A multi-TB manifest store can push
RTO to 30+ minutes; if that becomes a real customer impact, the
mitigation is to shard the audit by (tenant_id, bucket) and run the
shards in parallel.

## DR drill

A drill SHOULD happen at least once per quarter. The script lives at
`scripts/dr/manifest-resume-drill.sh` (ship before drill day) and:

1. Spins up a disposable fabric instance.
2. PUTs 1k objects.
3. Manually deletes 10% of the manifests via SQL to simulate
   corruption.
4. Runs steps 3-7 of this runbook.
5. Asserts the final conformance run is all-passed.
6. Tears down the disposable instance.

A drill that takes longer than 15 minutes is a runbook bug — file
a P2 against the on-call team's backlog.
