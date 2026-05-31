// Package chaos is the WS1.2 chaos / failure-injection test suite for
// zk-object-fabric.
//
// The suite has two layers:
//
//  1. Fault-injecting decorators that wrap the real
//     [providers.StorageProvider] and [manifest_store.ManifestStore]
//     interfaces. The decorators are deterministic (no goroutines,
//     no real network), so chaos tests are reproducible under -race
//     and can run in CI without external infrastructure.
//
//  2. Scenarios that drive real downstream code — the EC repair
//     queue, the StorageProvider conformance contract, the manifest
//     store interface — under those fault injectors, and assert that
//     the system degrades gracefully (no panics, no data corruption,
//     no silently-lost writes, no leaked goroutines).
//
// The scenarios in this package cover the in-process slice of the
// failure modes called out in docs/PROGRESS.md "Chaos /
// failure-injection testing":
//
//   - Provider 503 storms (Wasabi-style transient origin failures).
//   - Provider partition mid-write (the dark path that exposes
//     orphaned sidecar / partial-write bugs).
//   - Cache partition (forces every read through the origin path).
//   - ManifestStore unavailability (the gateway must never silently
//     drop or duplicate manifests when Postgres is unreachable).
//   - Piece loss during EC repair (NVMe-node-loss while the repair
//     queue is mid-flight).
//   - Concurrent multi-component failure (provider AND manifest
//     store down at the same time).
//
// The infrastructure-level failover scenarios (single gateway node
// kill, Postgres primary failover, cross-cell replication failover)
// are out of scope for the in-process suite. They are documented in
// docs/runbooks/chaos-testing.md as production-only exercises with
// the same acceptance criteria.
//
// Run the suite with:
//
//	go test -race -count=1 ./tests/chaos/...
package chaos
