// SPDX-License-Identifier: Apache-2.0
package main

// Register the Postgres SQL driver in the gateway binary.
//
// main.go opens the control-plane metadata store with
// sql.Open("postgres", ...) (see openMetadataDB), and every
// Postgres-backed store in the tree documents that "the gateway binary
// registers github.com/lib/pq" (e.g. metadata/content_index/postgres,
// metadata/bucket_config/postgres, api/console, api/s3compat/multipart,
// migration). database/sql resolves a driver by name only if some
// package has registered it via an import side effect, exactly as the
// embedded SQLite store does with `_ "modernc.org/sqlite"` in
// internal/embeddeddb. Without this blank import the production binary
// fails at startup with `sql: unknown driver "postgres" (forgotten
// import?)` the moment a metadata_dsn is configured — so any
// Postgres-backed deployment (SME single-pool, Linode/RDS) cannot run.
//
// lib/pq registers the driver once in its own init, so this single
// blank import in the main package covers the whole binary; adding it
// here mirrors the test-only registration already present in
// rls_guard_test.go.
import _ "github.com/lib/pq"
