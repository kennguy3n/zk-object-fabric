# External Audit Findings

This directory holds findings reported by external auditors against
the packages in [`../audit-package-security.md`](../audit-package-security.md)
and [`../audit-package-cryptography.md`](../audit-package-cryptography.md).

The structure is one subdirectory per audit engagement:

```
findings/
├── README.md                                 # this file
├── <vendor>-<YYYY-MM-DD>/                   # one per engagement
│   ├── 00-summary.md                        # exec summary + severity counts
│   ├── 01-<short-title>-<HIGH|MED|LOW>.md   # one file per finding
│   ├── 02-<short-title>-<HIGH|MED|LOW>.md
│   └── ...
└── ...
```

Naming convention for engagement folders: lowercase vendor slug
(`trail-of-bits`, `cure53`, `ncc-group`, `nccgroup-crypto-services`,
etc.) plus the engagement start date in `YYYY-MM-DD`. This is what
gets shipped to vendors as the canonical reference path.

## Per-finding file format

Each finding file uses this front-matter header so the build script
can aggregate severities across files:

```markdown
---
id: TOB-001
title: Convergent DEK salt is hex-encoded contentHash, not raw bytes
severity: low
component: encryption/client_sdk
status: fixed
fixed_in: <commit-sha-or-pr-link>
---

## Description
<what the finding is>

## Impact
<what an attacker gains>

## Reproduction
<minimal repro or grep pointer>

## Remediation
<what we did and why>
```

Severity is one of `critical | high | medium | low | informational`.
Status is one of `open | acknowledged | fixed | wontfix | duplicate`.

## How to add a new engagement

1. Create the directory: `findings/<vendor>-<YYYY-MM-DD>/`.
2. Drop the vendor's report into `00-summary.md` (or convert from
   PDF, preserving the original under `_originals/`).
3. Split the report into per-finding files using the format above.
4. For each `fixed` finding, link to the commit / PR that closed it.
5. Update [`../../PROGRESS.md`](../../PROGRESS.md) WS1.3 / WS1.4
   sections with the engagement summary.

## Public disclosure

Findings in this directory may be public-facing. **Do NOT include**:

- Any plaintext credential, token, or key material that appeared in
  a vendor's reproduction. Replace with `<REDACTED>` or describe
  the shape (e.g. "a 32-byte hex string").
- Customer-identifying information that may have appeared in
  reproduction harnesses.
- Vendor-confidential internal tooling unless the vendor's report
  template explicitly permits republication.
