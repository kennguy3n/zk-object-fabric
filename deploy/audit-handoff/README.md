# zk-object-fabric — external audit hand-off

You have just received `zkof-audit-handoff-<DATE>-<COMMIT>.tar.gz`.
This document is the first thing to read.

The hand-off bundles every artifact a third-party auditor needs to
evaluate the system without us in the loop. There is no separate
"appendix" repository to clone, no proprietary inspection tool to
install, and no portal login to provision. Everything cited in the
audit documents resolves to a file inside this tarball; everything
this README references is enumerated in `MANIFEST.txt` with a
SHA-256 hash anchored to a public git commit.

## Provenance

Every bundle records (in `MANIFEST.txt` at the top of the tarball):

- the **commit SHA** on `main` from which it was produced;
- the **build timestamp** in UTC;
- the **SHA-256** of every file the bundle contains.

To verify the bundle has not been tampered with after delivery:

```sh
$ tar -tf zkof-audit-handoff-*.tar.gz | head -1     # top-level dir
zkof-audit-handoff-2026-05-30-abcd123/
$ tar -xf zkof-audit-handoff-*.tar.gz
$ cd zkof-audit-handoff-*/
$ sha256sum -c MANIFEST.txt                          # all lines OK
```

The commit SHA in `MANIFEST.txt` is publicly visible at
`https://github.com/kennguy3n/zk-object-fabric/commit/<SHA>`. If
any file in the tarball does not match the file at that path in
the public tree, the bundle has been modified post-delivery —
discard it and request a re-build.

The `MANIFEST.txt` header lines (project name, commit SHA, build
time) are prefixed with `#` so `sha256sum -c` ignores them and
only verifies the `<hex>  <path>` data lines. No warnings are
emitted on a clean bundle.

## What you are auditing

zk-object-fabric is an S3-compatible object store with three
properties under audit:

1. **Cryptographic confidentiality and integrity** of object data
   at rest and in flight (per-chunk AEAD + AAD, HKDF DEK derivation,
   convergent nonces for deduplication, SigV4 HMAC chain).
2. **Multi-tenant isolation**: a tenant cannot read, list, modify,
   or even probe the existence of another tenant's objects through
   the gateway or the manifest store.
3. **Operational durability**: the system meets the published
   capacity envelope (CAPACITY.md) and recovers from named
   failure surfaces within the published RPO/RTO targets
   (`docs/runbooks/dr*.md`).

Out of scope: anything inside the third-party object stores
(Wasabi, Linode S3-compat, etc.) that we depend on as a backend.
Their durability promises are documented as upstream-trust
assertions in §11.4 of `docs/PROPOSAL.md`.

## First: start here (≈30 minutes)

Read in this order. Each step has a target read time.

1. **`docs/PROPOSAL.md`** (~10min skim) — system overview, trust
   boundaries, design decisions. Sections worth a slower read:
   §3 (data path), §6 (multi-tenant isolation), §11.4 (durability
   non-goals).

2. **`docs/ARCHITECTURE.md`** (~5min) — the component map. Every
   component in this hand-off resolves to a package and call site
   documented here.

3. **`docs/CAPACITY.md`** (~10min) — the *only* place where the
   numeric envelope is committed to in prose. Every per-operation
   target an auditor will see in a benchmark plot or SLA document
   resolves back here.

4. **`docs/security/README.md`** (~5min) — the index that points
   into the two audit packages below.

After step 4 you have enough context to evaluate the per-claim
audit packages.

## Next: per-claim audit packages

Three documents, each enumerating concrete claims (C1..Cn) with
exact `path:line` references back into the source tree included
in this tarball. None of them require running code.

- **`docs/security/audit-package-security.md`** — SigV4 anchor,
  multi-tenant isolation, manifest seal, fail-closed CMK guard,
  rate-limit / abuse surface.
- **`docs/security/audit-package-cryptography.md`** — per-chunk
  AEAD framing + AAD, HKDF DEK derivation, convergent nonces,
  SigV4 HMAC chain, body AEAD AAD, CMK envelope.
- **`docs/security/threat-model.md`** — actors, trust boundaries,
  out-of-scope. Read this first if any claim in the two packages
  above is unclear about which actor it defends against.

Every `path:line` reference in these documents was grep-verified
against the source commit at bundle time. If a reference does
not resolve to the file/line cited, a later change has landed
on `main` and the bundle should be regenerated — please flag.

## Then: operational dossier

The capacity envelope, conformance matrix, and disaster-recovery
runbooks are operational claims. They are not part of the
cryptographic audit per se, but the audit firm typically reviews
them because the cryptographic guarantees are conditional on the
operational envelope (e.g. "no nonce reuse" is conditional on
"objects per convergent-DEK does not exceed 2^32").

- **`docs/CAPACITY.md`** — per-operation P99 latency targets,
  per-cell sizing, per-node sustained RPS, S3 protocol envelope,
  derived availability commitment.
- **`tests/capacity/targets.go`** — Go single source of truth.
  Every constant here is re-exported from the production
  `benchmark.Target*` set, so a future drift breaks compile.
- **`docs/runbooks/s3-conformance.md`** — the in-process matrix
  runner; what we publish on every CI run vs. what we re-run
  through the external third-party harnesses (Ceph s3-tests,
  MinIO mint) on a fixed cadence.
- **`docs/runbooks/conformance-external.md`** — operator runbook
  for the external harness wiring (the audit firm receives the
  raw harness output as `tests/conformance/` artifacts on
  request).
- **`docs/runbooks/dr.md`** — index of every DR runbook, with
  published RPO/RTO targets.
- **`docs/runbooks/dr-postgres-restore.md`**, **`dr-cross-cell-failover.md`**,
  **`dr-manifest-resume.md`**, **`dr-verification.md`** — per-surface
  recovery flows with explicit pre-conditions and "DR drill"
  sections.

The in-process DR verifier (`tests/dr/verifier.go`) is what
gates every PR against an RTO regression — it asserts
`MeasuredRTO <= RTOTarget` and `LostObjects == InFlightObjects`
on every CI run.

## Then: staging-run evidence (optional)

If the audit engagement includes a Linode + Wasabi staging burn,
the `deploy/staging/` directory contains:

- Terraform + cloud-init for the load-driver VM
  (`load-driver/terraform/`, `load-driver/scripts/run_tier3.sh`).
- The Tier 3 in-process verifier (`cmd/tier3-verify/`) that the
  harness shells out to.
- The evidence-collection script (`scripts/collect_evidence.sh`)
  that the operator runs post-burn.

The auditor can re-run the burn end-to-end against fresh Linode
+ Wasabi credentials, or accept the operator's evidence bundle
as the input.

## Finally: filing findings

If you identify a real or suspected vulnerability, please use
the `docs/security/findings/` schema:

```
docs/security/findings/<vendor>-<YYYY-MM-DD>/
    finding-001-<short-slug>.md
    finding-002-<short-slug>.md
    ...
```

Each finding has the front-matter format documented in
`docs/security/findings/README.md`. We will respond to each
finding by either (a) accepting and tracking it through a public
PR, (b) accepting and tracking it through a private security
advisory if disclosure timing matters, or (c) explaining why we
believe the finding is by-design or already mitigated, in which
case the response goes back into the per-engagement directory
as a sibling document.

## Bundle component map

The sections above are organised by reading flow. The table
below cross-walks each prose section to the manifest's component
identifier (the slug used as the top-level directory inside the
tarball). When `INDEX.md` says a component is "MISSING (optional)"
because its source PR has not landed yet, this is where you look
up which prose section it belonged to:

| Component id           | Title                                                  | Where it appears above                                         |
|------------------------|--------------------------------------------------------|----------------------------------------------------------------|
| `audit_bundle`         | External security + cryptography audit packages        | "Next: per-claim audit packages"                               |
| `capacity_dossier`     | Capacity envelope dossier                              | "Then: operational dossier" (CAPACITY.md sub-section)          |
| `conformance_matrix`   | S3 protocol conformance matrix (in-process + external) | "Then: operational dossier" (s3-conformance sub-section)       |
| `dr_runbooks`          | Disaster-recovery runbooks + verifier                  | "Then: operational dossier" (dr.md sub-section)                |
| `staging_deploy`       | Linode + Wasabi staging deploy + Tier 3 verifier       | "Then: staging-run evidence (optional)"                        |
| `overview_pin`         | System overview + scope pin                            | "First: start here" (PROPOSAL.md + ARCHITECTURE.md)            |

The exact layout of this tarball is enumerated in
`deploy/audit-handoff/manifest.yaml`, which is included at the
top of the bundle alongside `INDEX.md`. Drift between this
README and the manifest is caught at build time by
`tests/audit/handoff_test.go`; you should never encounter a
component id here that does not resolve to a manifest entry,
nor vice versa.

If you do, the bundle is malformed — discard it and request a
re-build.
