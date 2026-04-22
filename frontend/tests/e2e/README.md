# Tenant console e2e suite

The specs in this directory are Playwright scaffolds that drive the
real tenant-console SPA against a running `./cmd/gateway`. They are
**not** executed by CI and are gated behind the `CONSOLE_E2E=1`
environment variable so an accidental `npm run test:e2e` in an
unconfigured shell reports every suite as skipped rather than
passing a stubbed assertion.

## Runbook

1. In one terminal, start the gateway with the in-memory console
   stores enabled (the default when no Postgres DSN is configured):

   ```
   go run ./cmd/gateway -listen-console :8081
   ```

2. In a second terminal, run the e2e suite. The Playwright config
   boots a Vite preview on `http://127.0.0.1:4173` that proxies
   `/api/v1/*` to the gateway; if you already have the SPA served
   elsewhere (e.g. a staging build) set `PLAYWRIGHT_BASE_URL` and
   the local preview is skipped.

   ```
   cd frontend
   CONSOLE_E2E=1 npm run test:e2e
   ```

3. To run a single spec while iterating:

   ```
   CONSOLE_E2E=1 npx playwright test tests/e2e/login.spec.ts
   ```

## What these tests cover

- `login.spec.ts` — B2C signup + login flow against
  `/api/v1/auth/{signup,login}`.
- `dashboard.spec.ts` — the three usage stat cards render and the
  SSE stream opens against `/api/v1/usage/stream/{tenantID}`.
- `buckets.spec.ts` — bucket list + create round-trip through
  `/api/v1/buckets`.
- `api-keys.spec.ts` — API-key creation reveals the one-time secret.
- `placement.spec.ts` — YAML placement-policy round-trip through
  `/api/v1/placement-policies/{ref}`.
- `b2b-cells.spec.ts` — dedicated-cells page visibility gated on
  tenant.contractType.

## Not covered

The specs do not seed fixture data in the gateway; most tests
tolerate an empty tenant by asserting the UI surface and the
outbound HTTP request, rather than expecting a specific response
body. Exhaustive fixture-driven coverage is tracked in
`docs/FRONTEND_PLAN.md` §6.
