// opsClient centralizes access to the admin-gated /api/v1/ops/* surface
// (served by api/console/ops_handler.go) shared by OperationsPage and
// WasabiHealthPage. Keeping the fetch helper and response shapes in one
// place means a future change — retry/backoff, auth handling, or a
// contract tweak — happens once instead of drifting across pages.
//
// Field names mirror the Go response structs, which use a single
// camelCase convention across every ops endpoint (see OpsHealthNode,
// OpsCacheStatsResponse, WasabiBudget).

// OpsHealth is GET /api/v1/ops/health. `status` is the coarse
// ok/degraded rollup; `node` is the camelCase projection of the
// gateway's internal health snapshot.
export interface OpsHealth {
  status: string;
  node: {
    nodeId: string;
    cellId?: string;
    state: string;
    quorumThreshold: number;
    healthyPeers?: Record<string, boolean>;
    inflight: number;
  };
}

// OpsCacheStats is GET /api/v1/ops/cache-stats. hitRatio and
// utilization are derived server-side and already clamped to [0,1].
export interface OpsCacheStats {
  entries: number;
  bytesUsed: number;
  bytesLimit: number;
  hits: number;
  misses: number;
  evictions: number;
  hitRatio: number;
  utilization: number;
}

// WasabiBudget is one tenant's row in GET /api/v1/ops/wasabi-budgets.
export interface WasabiBudget {
  tenantId: string;
  storedBytes: number;
  egressBytes: number;
  egressBudgetBytes: number;
  egressRatio: number;
  remainingBytes: number;
  status: string;
}

export interface WasabiBudgetsResponse {
  budgets: WasabiBudget[];
}

// opsGet fetches an admin-gated /api/v1/ops/* endpoint with the console
// session's bearer token. It returns null (rather than throwing) on any
// non-2xx or network error so each ops card can independently render an
// "unavailable" state without taking down the page.
export async function opsGet<T>(resource: string, token: string | null): Promise<T | null> {
  try {
    const res = await fetch(`/api/v1/ops/${resource}`, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
    if (!res.ok) return null;
    return (await res.json()) as T;
  } catch {
    return null;
  }
}
