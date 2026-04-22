import { test, type Page } from "@playwright/test";

// The e2e suite is a scaffold: every spec is gated on the
// CONSOLE_E2E=1 environment variable so `npm run test:e2e` does not
// run against a nonexistent gateway in CI, and `npm run build`
// never tries to execute it. Operators opt in by starting
// ./cmd/gateway and the Vite preview (or setting
// PLAYWRIGHT_BASE_URL) and then running `CONSOLE_E2E=1 npm run
// test:e2e`. See frontend/tests/e2e/README.md for the full
// runbook.
export const e2eEnabled = process.env.CONSOLE_E2E === "1";

// requireGateway skips the current suite when CONSOLE_E2E is not
// set. Call it from the top level of every spec file so the
// suite reports as "skipped" rather than "passed" in a
// no-gateway run, which would otherwise mask genuine regressions.
export function requireGateway(): void {
  test.skip(!e2eEnabled, "CONSOLE_E2E=1 required to run console e2e suite");
}

// seedAuth injects a fake tenant + token into sessionStorage so
// tests that exercise authenticated routes do not depend on the
// gateway's in-memory stores being populated.
//
// The storage key and shape mirror AuthContext.tsx — the SPA reads
// the same blob on mount and sets the Authorization header for
// every ApiClient request.
export interface SeedOptions {
  contractType?: "b2c_pooled" | "b2b_shared" | "b2b_dedicated" | "sovereign";
  tenantId?: string;
  token?: string;
}

const STORAGE_KEY = "zk-fabric.auth";

export async function seedAuth(page: Page, opts: SeedOptions = {}): Promise<void> {
  const contractType = opts.contractType ?? "b2c_pooled";
  const tenantId = opts.tenantId ?? "t-e2e";
  const token = opts.token ?? "e2e-token";
  const payload = JSON.stringify({
    tenant: {
      id: tenantId,
      name: "E2E Tenant",
      contractType,
      licenseTier: "standard",
      placementDefaultPolicyRef: "b2c_pooled_default",
      budgets: { requestsPerSec: 50, burstRequests: 100, egressTbMonth: 1.0 },
      createdAt: new Date().toISOString(),
    },
    token,
  });
  // addInitScript runs before any page script so the SPA's first
  // render already sees the seeded auth state.
  await page.addInitScript(
    ([key, value]: [string, string]) => {
      try {
        sessionStorage.setItem(key, value);
      } catch {
        // sessionStorage may be unavailable in some sandboxed
        // contexts (e.g. data: URLs); tests that need auth will
        // fail on subsequent assertions, which is the correct
        // signal.
      }
    },
    [STORAGE_KEY, payload] as [string, string],
  );
}
