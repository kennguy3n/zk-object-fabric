// Package s3_conformance is the WS1.5 S3 conformance harness.
//
// It exercises the full S3 API surface (core ops, listing, range,
// multipart, copy, versioning, plus the operations that are
// deliberately unsupported today — ACL, tagging, lifecycle, bucket
// versioning toggle, multi-object delete) against an S3 endpoint and
// produces a structured Matrix describing the outcome of every
// operation, including which ones are unsupported and what response
// the server returned in the unsupported case.
//
// The package is parameterised by an *s3.Client (AWS SDK v2) so it
// can be pointed at:
//
//   - The in-process zk-object-fabric gateway behind an
//     httptest.Server (see TestRunConformance_LocalFSDev). This is
//     the CI gate.
//   - A live zk-object-fabric gateway deployment (the
//     cmd/s3-conformance-runner CLI binary points the same Runner
//     at an arbitrary endpoint).
//   - A reference S3 server (real AWS S3, Ceph RGW, MinIO) — the
//     Runner does not know which server it is targeting, so a
//     side-by-side matrix against AWS S3 gives a parity baseline.
//
// The published Matrix is the WS1.5 deliverable required by
// docs/PROGRESS.md ("Production Readiness — S3 conformance report").
// A committed baseline lives at docs/conformance/s3-matrix.md so
// regressions in supported-operation coverage are caught by reviewers
// (the test snapshot is regenerated from the in-process gateway run).
//
// Why this and not just shell out to Ceph s3-tests / MinIO mint?
// Both external harnesses are also documented in
// docs/runbooks/s3-conformance.md, but they target Python and Go
// docker images with their own dependency stacks and expect a
// long-running endpoint. The in-package Go runner runs in the
// existing `go test ./...` battery with zero extra tooling, produces
// the same matrix format the external runs publish, and gives
// reviewers a single source of truth for which operations the
// gateway is expected to support today.
package s3_conformance
