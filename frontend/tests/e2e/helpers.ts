import type { Page } from "@playwright/test";

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
