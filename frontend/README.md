# ZK Object Fabric — Tenant Console

The tenant console is the operator-facing SPA that sits in front of the
gateway's `/api/v1/` management API. It is a Vite + React + TypeScript
application.

## What the console does

- **Login / signup**: B2C self-service onboarding. The form targets
  `POST /api/v1/auth/login` and `POST /api/v1/auth/signup`.
- **Dashboard**: per-tenant storage, request counts, and egress bytes
  pulled from `GET /api/v1/usage`. This data is produced by the
  billing pipeline (see `billing/metering.go`, backed by ClickHouse in
  production).
- **Buckets**: create / list / delete via `/api/v1/buckets`.
- **API keys**: create / revoke / list via `/api/v1/api-keys`.
- **Placement policy editor**: visual + YAML editor for the placement
  policy schema documented in [`docs/PROPOSAL.md`](../docs/PROPOSAL.md).
  Loads and persists through `/api/v1/placement-policies`.
- **B2B section**: the dedicated-cell provisioning workflow. Visible
  only for tenants whose `contract_type` is `b2b_dedicated` or
  `sovereign`.
- **Tier comparison**: read-only public tier comparison at
  `/api/v1/tiers`.

## API shape

The console only ever talks to `/api/v1/...`. The S3-compatible
routes (`GET /bucket/key`, `PUT /bucket/key`, …) remain untouched and
are reserved for SDKs. Keeping the two surfaces on separate prefixes
means the gateway can enforce different auth headers, CORS policies,
and rate limits on each.

The API client lives in `src/api/` and is typed against the contracts
defined in [`docs/PROPOSAL.md`](../docs/PROPOSAL.md) and the
`metadata/tenant` package.

## Development

```bash
npm install
npm run dev   # proxies /api/v1 → http://localhost:8080
npm run build
npm run lint
npm run test
```
