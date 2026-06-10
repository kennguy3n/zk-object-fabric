# Security & Cryptography Audit Packages

This directory contains the hand-off packages for the **external**
security review (Workstream 1.3) and the **external** cryptography
review (Workstream 1.4) listed in [`docs/PROGRESS.md`](../PROGRESS.md).

Each package is a single Markdown document that an external auditor
can read end-to-end without needing a guided code walkthrough. The
docs are intentionally cross-referenced to specific files and line
ranges in the repository so the auditor can pivot directly into the
source without searching.

| Package | Audience | File |
|---|---|---|
| Security audit package | Third-party security firm (e.g. Trail of Bits, Cure53, NCC Group) | [`audit-package-security.md`](audit-package-security.md) |
| Cryptography audit package | Specialist cryptography auditor | [`audit-package-cryptography.md`](audit-package-cryptography.md) |
| Threat model | Read alongside either package | [`threat-model.md`](threat-model.md) |

## How the packages are kept honest

- Every `path:line` reference is grep-verified against `main` at the
  time of the package's last update. The package header records the
  commit SHA used.
- The `Makefile`'s `audit-bundle` target (see `Makefile` in repo
  root) builds a tarball containing the package, the referenced
  source files, the threat model, and the latest test reports.
  Run `make audit-bundle` before sending anything to an auditor so
  the references are guaranteed to match.
- The bundle script also runs `go vet`, `staticcheck`, and
  `govulncheck` and embeds their output. An auditor who receives
  the bundle has the same static-analysis baseline we use in CI.

## Findings tracking

External audit findings land in this directory under
`findings/<vendor>-<YYYY-MM-DD>/`. The structure mirrors what
the Trail of Bits report templates use: one Markdown file per
finding, with severity, impact, remediation, and a status field.
See [`findings/README.md`](findings/README.md) for the layout.

## Automated supply-chain gates

Alongside the periodic external review, the repo runs continuous
supply-chain checks. `govulncheck` and `staticcheck` run in the core
`ci` workflow; secret scanning, frontend dependency audit, and SBOM
generation run in a dedicated `security` workflow
(`.github/workflows/security.yml`). All are reproducible locally:

```
# Secret scanning — full git history, uses .gitleaks.toml
gitleaks detect --no-banner --redact --config .gitleaks.toml

# Go vulnerability scanning — call-graph-aware, only reachable vulns
govulncheck ./...

# Frontend dependency audit — gate on the shipped (production) tree
cd frontend && npm audit --omit=dev --audit-level=high
```

`gitleaks` extends the upstream ruleset and allowlists only specific,
audited non-secret fixtures **by value** (see `.gitleaks.toml`); it
does not blanket-skip `_test.go` files. `npm audit` gates on the
production tree at `high` severity (the console bundle that ships);
dev/build-tooling advisories are reported informationally.

### SBOM (Software Bill of Materials)

The production Docker image generates an SPDX 2.3 SBOM during the build
(a dedicated `sbom` stage runs Syft over the gateway binary, the Go
module graph, and the console's npm lockfile) and ships it at
`/usr/share/sbom/zk-object-fabric.spdx.json`. The `security` workflow
also exports it as a build artifact (`zk-object-fabric-sbom-spdx`). To
produce it locally:

```
docker build --target sbom -o type=local,dest=./sbom-out .
jq .spdxVersion ./sbom-out/sbom.spdx.json   # -> "SPDX-2.3"
```
