# S3 Conformance Runbook

This runbook describes how to run the gateway's S3 conformance
matrix, how to publish a Markdown report from it, and how to drive
the same gateway through the two external open-source harnesses
(Ceph `s3-tests` and MinIO `mint`) for a second, independent
opinion on AWS S3 surface fidelity. It also documents the matrix
format, the regression-gate semantics, and what to do when an
operation flips from `passed` to `failed` or `unsupported`.

## What the conformance matrix is

Workstream 1.5 of the Production Validation plan asks for "a
published S3 conformance report covering: PUT, GET, HEAD, DELETE,
LIST, multipart, range, ACL, versioning, lifecycle, tagging".
The matrix is the deliverable. It is a deterministic, signed JSON +
Markdown document recording the outcome of every operation in
that surface against a real running gateway. Each row is one
`OpStatus`:

| Status        | Meaning                                                                                                                                                                                                                |
| ------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `passed`      | Server returned the response the AWS S3 reference behaviour requires.                                                                                                                                                  |
| `failed`      | Server returned a response, but it did not match AWS S3 reference behaviour. `Detail` describes the divergence (e.g. "ETag mismatch", "wrong status 200, expected 416", "ListObjects returned 0 keys, expected 3"). **Defect — must triage.** |
| `unsupported` | Operation is not implemented today; server returned a 4xx (typically 501 NotImplemented or 405 MethodNotAllowed). **Documented gap, not a defect.**                                                                    |
| `errored`     | Runner could not get a meaningful response (network error, panic, unexpected 5xx). **Triage as infrastructure issue, not API behaviour.**                                                                              |

`AllPassed()` returns true iff there are zero `failed` and zero
`errored` entries — `unsupported` entries are expected. This is
the CI gate.

## Running the in-process matrix

The runner in `tests/s3_conformance` drives the gateway in-process
against the `local_fs_dev` provider. This is the canonical "smoke"
gate that runs on every commit. It is hermetic — no network calls,
no external dependencies, no flakes — and finishes in ~1 second.

```bash
go test -race -count=1 ./tests/s3_conformance/...
```

The test (`TestRunConformance_LocalFSDev`) asserts:

- Every operation the matrix tracks has a non-empty `Op` and
  `Category` field.
- The core surface (PUT, GET, HEAD, DELETE, LIST, range, copy,
  multipart) all pass.
- Every intentionally-unsupported operation (ACL, tagging,
  lifecycle, bucket-versioning, bulk DeleteObjects) returns a
  4xx — i.e. the gateway never silently accepts an operation
  it does not honour.
- The matrix serialises losslessly through JSON.
- The Markdown renderer produces the expected section headers and
  per-operation rows.

A new operation flipping from `passed` to `failed` in this test is
a hard CI failure. A new operation appearing as `unsupported` is
not a failure — it's documented in the matrix as a gap.

## Generating a published Markdown report

The same runner can be invoked from a small CLI to produce a
human-readable report for `docs/conformance/` or for sharing with
external auditors:

```go
// scripts/conformance-report/main.go (sketch)
matrix := (&s3_conformance.Runner{
    Client:       client, // configured for the target endpoint
    Endpoint:     endpoint,
    Bucket:       bucket,
    CreateBucket: true,
    Cleanup:      true,
}).Run(context.Background())

f, _ := os.Create("docs/conformance/local_fs_dev.md")
defer f.Close()
matrix.WriteMarkdown(f)
```

The Markdown output is deterministic — two runs against the same
gateway with the same outcomes produce byte-identical files, which
makes it suitable for `git diff` review.

## Running against a real deployment

For a "real" published matrix (e.g. for an external audit), point
the runner at the deployed gateway endpoint with real credentials
and a dedicated test bucket. The bucket is deliberately
configurable so the test does not collide with production data:

```bash
go run ./scripts/conformance-report \
    -endpoint https://gw.example.com \
    -bucket  zkof-conformance-$(date +%s) \
    -region  us-east-1 \
    -output  docs/conformance/prod-$(date +%Y%m%d).md
```

The runner cleans up every key it wrote under the configured
`KeyPrefix` (default empty — be careful) when `Cleanup=true`, but
this uses single-key `DeleteObject` calls in a loop rather than
bulk `DeleteObjects` (which is in the unsupported set), so a 10K
key cleanup takes seconds, not milliseconds. For very large test
buckets, prefer running with `Cleanup=false` and a dedicated
bucket you can drop wholesale afterwards.

## External harness #1: Ceph `s3-tests`

[Ceph `s3-tests`](https://github.com/ceph/s3-tests) is the most
widely used external S3 conformance suite — Ceph RGW, MinIO, and
several other S3 implementations all run it as their primary
compliance gate. We use it as a second opinion on the gateway's
surface fidelity.

```bash
# One-time setup
git clone https://github.com/ceph/s3-tests /tmp/s3-tests
cd /tmp/s3-tests
python3 -m venv venv
source venv/bin/activate
pip install -r requirements.txt
pip install -e .

# Each run — write a config pointing at the gateway
cat > s3tests.conf <<EOF
[DEFAULT]
host = gw.example.com
port = 443
is_secure = yes

[s3 main]
display_name = zkof-conformance
user_id = zkof-conformance
email = devnull@example.com
api_name = default
access_key = $AWS_ACCESS_KEY_ID
secret_key = $AWS_SECRET_ACCESS_KEY
EOF

S3TEST_CONF=s3tests.conf ./virtualenv/bin/nosetests s3tests.functional \
  -a '!fails_on_aws,!encryption,!sse-c,!versioning,!lifecycle,!tagging' \
  --with-xunit --xunit-file=s3-tests-report.xml
```

The skipped tag list (`-a '!…'`) deliberately excludes the
features we know we don't implement (versioning, lifecycle,
tagging, SSE-C). These match the `unsupported` rows in our
matrix — if you remove an `unsupported` row by implementing the
feature, also remove the corresponding skip tag here so the
external harness picks up the new surface area.

The xunit report goes into `docs/conformance/external/s3-tests-{date}.xml`
for archiving and is referenced in the published matrix.

## External harness #2: MinIO `mint`

[MinIO `mint`](https://github.com/minio/mint) is a Docker-based
test harness covering AWS SDK behaviour across Go, Java, Python,
JS, .NET, and Ruby. It complements `s3-tests` (which is
nose/python-only) by exercising the gateway with multiple SDK
implementations.

```bash
docker run --rm \
  -e "SERVER_ENDPOINT=gw.example.com:443" \
  -e "ACCESS_KEY=$AWS_ACCESS_KEY_ID" \
  -e "SECRET_KEY=$AWS_SECRET_ACCESS_KEY" \
  -e "ENABLE_HTTPS=1" \
  -e "MINT_MODE=core" \
  -v $(pwd)/mint-logs:/mint/log \
  minio/mint:edge
```

`MINT_MODE=core` runs the headline correctness suite. Other modes:

| Mode    | What it adds                                                                      |
| ------- | --------------------------------------------------------------------------------- |
| `core`  | PUT/GET/HEAD/DELETE/LIST/Copy/Multipart/Presigned.                                |
| `full`  | `core` plus encryption, replication, lifecycle, locking. Most of `full` is `skip`'d when run against this gateway; use it to verify the gaps surface cleanly. |
| `quick` | Smoke subset; useful in CI for time-bounded runs.                                 |

The per-SDK logs land in `mint-logs/{date}/{sdk}/`. Archive the
whole directory under `docs/conformance/external/mint-{date}/` and
reference it from the published matrix.

## Regression gates

Three gates exist; each catches a different class of regression:

1. **In-process matrix gate** (`go test ./tests/s3_conformance/...`).
   Catches behaviour drift on every commit. Fast, hermetic, hard
   blocker on CI. A `failed` here means a code change broke a
   previously-passing operation; a previously-`unsupported`
   operation moving to `passed` is fine but should also remove the
   corresponding entry from the `unsupportedSubresources` map in
   `api/s3compat/handler.go` (otherwise the dispatcher will keep
   rejecting requests for it).

2. **External harness gate** (`s3-tests` + `mint` against a real
   deployment). Run on every release-candidate build, not on every
   commit. Catches SDK-specific edge cases and protocol-level
   issues the in-process runner does not exercise (e.g. chunked
   transfer encoding from boto3, presigned-URL signature
   differences across SDK versions).

3. **Published matrix diff** (`git diff docs/conformance/`).
   Reviewers compare the new matrix against the previous one. A
   diff is expected when surface area changes; an unexplained diff
   is a regression that escaped the first two gates.

## Triaging a `failed` row

When the in-process matrix flips an operation to `failed`:

1. Read the `Detail` column. The runner records the specific
   divergence ("HTTP 500 instead of 416", "ETag mismatch",
   "ListObjects returned 0 keys, expected 3"). The detail is
   usually enough to point at the regression.
2. Re-run the operation in isolation with the AWS SDK against a
   local gateway to reproduce.
3. If the divergence is intentional (e.g. an SLA-driven choice
   that breaks strict AWS parity), document the deviation in
   `docs/s3-compat-deviations.md` and move the row to `unsupported`
   in the matrix, with a `Detail` explaining the deviation.
4. Otherwise, fix the gateway behaviour. The matrix is the
   contract; a `failed` row is a defect.

## Triaging a `errored` row

`errored` means the runner could not reach a meaningful response.
Causes:

- Network error reaching the gateway endpoint.
- The gateway returned 5xx (other than 501 NotImplemented). This
  is a server-side bug — likely a panic, an unhandled error path,
  or an upstream provider failure cascading through.
- The runner itself panicked. Check the test output for a stack
  trace.

`errored` should never persist across runs. If it does, it is
either an environment issue (DNS, TLS cert, credentials) or a
real server-side bug that needs immediate triage.

## Adding a new operation to the matrix

When the gateway gains a new operation (e.g. versioning lands):

1. Remove the corresponding entry from `unsupportedSubresources`
   in `api/s3compat/handler.go` so the dispatcher stops rejecting
   it.
2. Add the new operation to the relevant group in
   `tests/s3_conformance/runner.go` (e.g. `versioningOps`,
   `lifecycleOps`). Drive it with the AWS SDK and assert the AWS
   reference behaviour.
3. Move the operation's expected status in
   `TestRunConformance_LocalFSDev` from `mustBeUnsupported` to
   `mustPass`.
4. Re-run `go test ./tests/s3_conformance/...` and confirm the
   matrix snapshot under `docs/conformance/` updates as expected.
5. Update the external-harness skip lists (`-a '!…'` for s3-tests,
   the `mint` skip set) so the external runs start exercising the
   new surface.

### Partial per-method support for a sub-resource

`rejectUnsupportedSubresource` is intentionally method-agnostic: any
sub-resource listed in `unsupportedSubresources` is refused for every
HTTP method (the rejection runs before the per-method dispatch). That
is the right behaviour today because every entry in the map is
unsupported across all methods.

If a future change implements support for ONE method of a sub-resource
but not the others (e.g. `GET ?acl` is wired up but `PUT ?acl` is not),
the operator MUST:

1. Remove the sub-resource key from `unsupportedSubresources`
   entirely — leaving it in the map would 501 every request including
   the newly-wired GET, because the global rejection runs before the
   GET dispatch.
2. Add a method-specific rejection inside the un-implemented method's
   dispatch arm (the PUT arm in the example), emitting the same
   `501 NotImplemented` + `NotImplemented` S3 code.
3. Add the working method to `tests/s3_conformance/runner.go`'s
   relevant op group AND keep an `unsupportedOps` probe for the
   un-implemented method so the matrix continues to assert that
   PUT-side 501.

## Where this matrix is consumed

- **Internal**: the in-process gate runs on every commit; broken
  on PR means broken on merge.
- **External audits**: the published matrix + the s3-tests /
  mint reports are part of the WS1.3 / WS1.4 audit hand-off
  bundle (see `Makefile#audit-bundle`).
- **Customers**: the published matrix is the canonical answer to
  "does your gateway support tagging?" — if the row says
  `unsupported`, the answer is no, today.
