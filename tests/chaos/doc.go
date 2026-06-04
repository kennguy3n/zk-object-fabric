// Package chaos is the WS1.2 chaos / failure-injection test suite for
// zk-object-fabric.
//
// The suite has two layers:
//
//  1. Fault-injecting decorators that wrap the real
//     [providers.StorageProvider], [manifest_store.ManifestStore],
//     and [hot_object_cache.HotObjectCache] interfaces
//     ([FaultProvider], [FaultManifestStore], [FaultCache]). The
//     decorators share one fault vocabulary ([FaultConfig] /
//     [FaultMode]) and are deterministic (no goroutines, no real
//     network), so chaos tests are reproducible under -race and can
//     run in CI without external infrastructure.
//
//  2. Scenarios that drive real downstream code — the EC repair
//     queue, the StorageProvider conformance contract, the manifest
//     store interface, the s3compat gateway PUT/GET path, the
//     lazy_read_repair migration engine, and the in-memory rate
//     limiter — under those fault injectors, and assert that the
//     system degrades gracefully (no panics, no data corruption,
//     no silently-lost writes, no leaked goroutines).
//
// The scenarios in this package cover the in-process slice of the
// failure modes called out in docs/PROGRESS.md "Chaos /
// failure-injection testing":
//
//   - Provider 503 storms (Wasabi-style transient origin failures).
//   - Provider partition mid-write (the dark path that exposes
//     orphaned sidecar / partial-write bugs).
//   - Cache partition (TestChaos_CachePartition: every cache Get
//     errors via FaultCache; the gateway must fall through to the
//     origin and still serve byte-correct data).
//   - ManifestStore unavailability (TestChaos_MetadataDBFailover:
//     manifest read/write faults mid-PUT/GET; the gateway must fail
//     closed with a 5xx, never corrupt or orphan data, and recover
//     when the store heals).
//   - Backend timeout (TestChaos_WasabiTimeout: a 30s provider delay
//     must be cut short by the caller's context deadline rather than
//     pinning the request goroutine).
//   - Concurrent migration (TestChaos_ConcurrentMigration: a
//     Wasabi→local_fs_dev migration via lazy_read_repair running
//     alongside live PUTs/GETs must lose no data and serve no stale
//     reads).
//   - Rate-limiter fail-closed (TestChaos_RateLimiterFailClosed: a
//     resolved-but-exhausted egress budget fails closed today; the
//     unresolvable-budget path is guarded behind failClosedAvailable
//     pending the in-memory limiter's FailClosed work).
//   - Piece loss during EC repair (NVMe-node-loss while the repair
//     queue is mid-flight).
//   - Concurrent multi-component failure (provider AND manifest
//     store down at the same time).
//
// Two of the gateway-level scenarios assert the invariant that holds
// on current main while documenting a hardening gap rather than
// pinning a status code the gateway does not yet return: the metadata
// failover scenario records that a metadata-store error maps to 500
// today where a retryable 503 is the target, and the timeout scenario
// records the 502→504 gap. Both still prove the property that matters
// operationally (fail closed, no corruption, honour the deadline).
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
