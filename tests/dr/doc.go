// Package dr is the end-to-end disaster-recovery verification
// harness for the fabric.
//
// The harness drives a full DR cycle against the real code paths
// the operator-facing runbooks describe (see
// docs/runbooks/dr-postgres-restore.md,
// docs/runbooks/dr-cross-cell-failover.md,
// docs/runbooks/dr-manifest-resume.md) and produces concrete RPO
// (recovery point objective — data loss in objects) and RTO
// (recovery time objective — wall-clock from failure detection
// to first successful read) measurements.
//
// The verifier exercises three core invariants:
//
//  1. Every object that finished replicating to the destination
//     cell before the failure window opens must be byte-identical
//     after recovery. A mismatch is a silent data-corruption
//     regression in the replicator or manifest store and the
//     test fails loudly.
//
//  2. The RPO measurement must equal the number of objects that
//     were PUT after the snapshot and before the simulated
//     failure but did not reach the destination. This pins the
//     replicator's "drain everything older than the snapshot
//     timestamp" guarantee.
//
//  3. The RTO measurement must be bounded by an operator-supplied
//     target. RTO is wall-clock from the moment the source cell
//     stops accepting reads to the moment a recovery gateway
//     serves a successful GET against the destination manifest
//     store + provider.
//
// The harness is intentionally in-process: it operates on
// metadata/manifest_store/memory + providers/local_fs_dev cells
// driven by the real cross_cell.Replicator, and it is the
// primary regression gate for the cross-cell DR path. A
// secondary, infrastructure-level harness (Postgres pg_basebackup
// + WAL replay, real cross-region replication) is described in
// the runbooks above and is out of scope for this package.
//
// The exported Verifier and Report types are stable; the test
// fixtures inside the package are internal.
package dr
