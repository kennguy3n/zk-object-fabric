import type {
  ApiKey,
  Bucket,
  CellStatus,
  CostBreakdown,
  DedicatedCell,
  DedupPolicy,
  MigrationJob,
  PlacementPolicy,
  ProvisionCellInput,
  Tenant,
  TierConfig,
  UsageSnapshot,
} from "./types";

// AuthWire is the JSON shape api/console/auth_handler.go's
// AuthResponse marshals to. It is defined here (rather than imported
// from auth.ts) because auth.ts imports this module, and a circular
// import would force both into one file. auth.ts re-exports the same
// fields as its public AuthResponse, so the two stay structurally
// identical.
export interface AuthWire {
  tenant: Tenant;
  token: string;
  accessKey?: string;
  secretKey?: string;
  createdAt?: string;
  refreshToken?: string;
  refreshTokenExpiresAt?: string;
}

// ApiClient is the thin wrapper the SPA uses to reach the gateway's
// management API. Auth endpoints live under `${rootBaseUrl}/v1/auth`
// (e.g. `/api/v1/auth`); tenant-scoped routes live under
// `${rootBaseUrl}/tenants/${tenantID}/...` to match the mux
// registered in api/console/handler.go. The SPA seeds the tenant
// scope via setTenantScope() immediately after login / signup so
// subsequent calls resolve to the correct tenant; before login only
// the auth endpoints are callable.
export class ApiClient {
  private tenantBaseUrl: string | undefined;
  // tenantId is retained alongside tenantBaseUrl because a handful of
  // routes live under the versioned /api/v1/tenants/{id}/ prefix
  // (cost-breakdown, dedup-policy) rather than the legacy
  // /api/tenants/{id}/ subtree the CRUD calls use, so they need the raw
  // ID to build their own URL.
  private tenantId: string | undefined;

  constructor(
    private readonly rootBaseUrl: string,
    private token?: string,
    private readonly authBaseUrl: string = `${rootBaseUrl}/v1/auth`,
  ) {}

  setToken(token: string | undefined) {
    this.token = token;
  }

  // setTenantScope wires the tenant ID into the path prefix used by
  // every tenant-scoped call on this client. Call it with the
  // tenant ID returned from login/signup. Pass `undefined` to clear
  // the scope on logout so the SPA never accidentally sends stale
  // tenant-scoped requests on behalf of a signed-out user.
  setTenantScope(tenantId: string | undefined) {
    if (!tenantId) {
      this.tenantBaseUrl = undefined;
      this.tenantId = undefined;
      return;
    }
    this.tenantId = tenantId;
    this.tenantBaseUrl = `${this.rootBaseUrl}/tenants/${encodeURIComponent(tenantId)}`;
  }

  // versionedTenantUrl builds a URL under the /api/v1/tenants/{id}
  // subtree used by the cost-breakdown and dedup-policy routes.
  private versionedTenantUrl(suffix: string): string {
    if (!this.tenantId) {
      throw new ApiError(0, "tenant scope is not set; call setTenantScope() after login/signup");
    }
    return `${this.rootBaseUrl}/v1/tenants/${encodeURIComponent(this.tenantId)}${suffix}`;
  }

  // --- auth -----------------------------------------------------
  //
  // Auth routes intentionally bypass the tenant-scoped baseUrl and
  // hit /api/v1/auth/* directly so the versioned contract in
  // api/console/auth_handler.go is preserved even if the tenant
  // routes ever drop or bump their own version prefix.

  async login(email: string, password: string): Promise<AuthWire> {
    return this.requestAt("POST", `${this.authBaseUrl}/login`, { email, password });
  }

  async signup(input: {
    email: string;
    password: string;
    tenantName: string;
    captchaToken?: string;
  }): Promise<AuthWire> {
    return this.requestAt("POST", `${this.authBaseUrl}/signup`, input);
  }

  // refresh exchanges a refresh token for a fresh access token and a
  // rotated refresh token. The presented token is single-use, so the
  // caller must persist the returned refreshToken in its place.
  async refresh(refreshToken: string): Promise<AuthWire> {
    return this.requestAt("POST", `${this.authBaseUrl}/refresh`, { refreshToken });
  }

  // logout revokes a refresh token. The endpoint returns 204 and is
  // idempotent, so an unknown or already-revoked token still resolves.
  async logout(refreshToken: string): Promise<void> {
    await this.requestAt("POST", `${this.authBaseUrl}/logout`, { refreshToken });
  }

  // --- usage & dashboard ---------------------------------------
  //
  // Backend returns placement_policy-style UsageResponse ({tenant_id,
  // start, end, counters: map[billing.Dimension]uint64}). The SPA
  // renders UsageSnapshot (camelCase, pre-aggregated stat cards), so
  // the client projects the counter map onto the snapshot shape via
  // the shared usageSnapshotFromCounters() below. DashboardPage feeds
  // its live SSE frames through the same function, so the REST
  // bootstrap and the streamed frames stay byte-for-byte identical.

  async currentUsage(): Promise<UsageSnapshot> {
    const raw = await this.get<UsageCountersPayload>("/usage");
    return usageSnapshotFromCounters(raw);
  }

  // --- buckets --------------------------------------------------

  async listBuckets(): Promise<Bucket[]> {
    return this.get("/buckets");
  }

  async createBucket(name: string, placementPolicyRef: string): Promise<Bucket> {
    return this.post("/buckets", { name, placementPolicyRef });
  }

  async deleteBucket(name: string): Promise<void> {
    await this.request("DELETE", `/buckets/${encodeURIComponent(name)}`);
  }

  // --- api keys -------------------------------------------------

  async listApiKeys(): Promise<ApiKey[]> {
    return this.get("/keys");
  }

  async createApiKey(): Promise<ApiKey> {
    return this.post("/keys", {});
  }

  async revokeApiKey(accessKey: string): Promise<void> {
    await this.request("DELETE", `/keys/${encodeURIComponent(accessKey)}`);
  }

  // --- placement policies --------------------------------------
  //
  // The backend stores a single Policy per tenant and returns it as
  // placement_policy.Policy ({tenant, bucket, policy: {...}}). The
  // SPA's editor models policies as an editable list keyed by id, so
  // the client adapts the wire shape into a one-element array on
  // read and translates the editor's yaml field back into the
  // backend's JSON Policy on write. The "yaml" editor is JSON under
  // the hood in Phase 1; the same canonical form is what the gateway
  // accepts, so a round-trip through the textarea is lossless.

  async listPlacementPolicies(): Promise<PlacementPolicy[]> {
    const raw = await this.get<BackendPlacementPolicy>("/placement");
    return [backendToFrontendPolicy(raw)];
  }

  async savePlacementPolicy(policy: Omit<PlacementPolicy, "updatedAt">): Promise<PlacementPolicy> {
    const body = frontendToBackendPolicy(policy);
    const raw = await this.put<BackendPlacementPolicy | undefined>("/placement", body);
    // The gateway replies 200 with the persisted Policy, but tolerate a
    // 204/empty response by echoing the submitted document so a save
    // never surfaces a spurious parse error to the user.
    return backendToFrontendPolicy(raw ?? body);
  }

  // --- dedicated cells (b2b_dedicated / sovereign only) --------

  async listDedicatedCells(): Promise<DedicatedCell[]> {
    return this.get("/dedicated-cells");
  }

  // provisionDedicatedCell submits a dedicated-cell provisioning
  // request. The gateway replies 202 with the initial CellStatus
  // (status "provisioning"); the list view reflects the transition to
  // "active" once the provisioner completes.
  async provisionDedicatedCell(input: ProvisionCellInput): Promise<CellStatus> {
    return this.post("/dedicated-cells", {
      region: input.region,
      country: input.country,
      capacity_petabytes: input.capacityPetabytes,
      erasure_profile: input.erasureProfile ?? "",
      node_count: input.nodeCount,
    });
  }

  // --- cost breakdown (admin-gated; best-effort) ---------------
  //
  // Returns null only when the route is legitimately unavailable to
  // this session — the console token is not admin-scoped (401/403),
  // the tenant/route is absent (404), or no cost reporter is wired
  // (503). Callers then fall back to the usage×tier estimate, which
  // is always tenant-accessible. A genuine server fault (5xx) or a
  // network error is NOT masked as "unavailable": it propagates so
  // the caller can surface that the backend is broken rather than
  // silently hiding it behind the gated-feature notice.

  async costBreakdown(month?: string): Promise<CostBreakdown | null> {
    const qs = month ? `?month=${encodeURIComponent(month)}` : "";
    try {
      return await this.requestAt<CostBreakdown>(
        "GET",
        this.versionedTenantUrl(`/cost-breakdown${qs}`),
      );
    } catch (e) {
      if (isFeatureUnavailable(e)) return null;
      throw e;
    }
  }

  // --- per-bucket dedup policy (admin-gated; best-effort) ------
  //
  // Same contract as costBreakdown: null means "this session may not
  // see the policy" (401/403/404/503); 5xx and network failures
  // propagate so the caller distinguishes a broken backend from a
  // gated feature.

  async getDedupPolicy(bucket: string): Promise<DedupPolicy | null> {
    try {
      return await this.requestAt<DedupPolicy>(
        "GET",
        this.versionedTenantUrl(`/buckets/${encodeURIComponent(bucket)}/dedup-policy`),
      );
    } catch (e) {
      if (isFeatureUnavailable(e)) return null;
      throw e;
    }
  }

  // setDedupPolicy enables (level given) or disables (enabled:false)
  // intra-tenant dedup for a bucket. Surfaces the gateway's error
  // verbatim so the UI can show the precise rejection reason (e.g.
  // object+block requiring a Ceph RGW backend).
  async setDedupPolicy(
    bucket: string,
    input: { enabled: boolean; level?: "object" | "object+block" },
  ): Promise<DedupPolicy> {
    return this.requestAt(
      "POST",
      this.versionedTenantUrl(`/buckets/${encodeURIComponent(bucket)}/dedup-policy`),
      { enabled: input.enabled, scope: "intra_tenant", level: input.level ?? "object" },
    );
  }

  // --- migrations (fleet-wide, read-only) ----------------------

  async listMigrations(): Promise<MigrationJob[]> {
    return this.requestAt("GET", `${this.rootBaseUrl}/v1/migrations`);
  }

  // --- product tiers (read-only, not tenant-scoped) ------------

  async listTierConfigs(): Promise<TierConfig[]> {
    return this.requestAt("GET", `${this.rootBaseUrl}/v1/tiers`);
  }

  // --- transport ------------------------------------------------

  private async get<T>(path: string): Promise<T> {
    return this.request("GET", path);
  }

  private async post<T>(path: string, body: unknown): Promise<T> {
    return this.request("POST", path, body);
  }

  private async put<T>(path: string, body: unknown): Promise<T> {
    return this.request("PUT", path, body);
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    if (!this.tenantBaseUrl) {
      throw new ApiError(
        0,
        "tenant scope is not set; call setTenantScope() after login/signup before issuing tenant-scoped calls",
      );
    }
    return this.requestAt(method, `${this.tenantBaseUrl}${path}`, body);
  }

  private async requestAt<T>(method: string, url: string, body?: unknown): Promise<T> {
    const res = await fetch(url, {
      method,
      headers: {
        "Content-Type": "application/json",
        ...(this.token ? { Authorization: `Bearer ${this.token}` } : {}),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!res.ok) {
      const text = await res.text().catch(() => res.statusText);
      throw new ApiError(res.status, text);
    }
    if (res.status === 204) {
      return undefined as T;
    }
    return (await res.json()) as T;
  }
}

export class ApiError extends Error {
  constructor(public readonly status: number, message: string) {
    super(`API error ${status}: ${message}`);
    this.name = "ApiError";
  }
}

// FEATURE_UNAVAILABLE_STATUSES are the HTTP statuses that mean an
// admin-gated, best-effort endpoint is legitimately not visible to
// this session rather than broken: the console token is not
// admin-scoped (401/403), the tenant or route is absent (404), or the
// backing reporter/handler is not wired (503). Any other failure —
// notably 5xx server faults and 4xx contract errors — is a real
// problem callers should not paper over as "feature unavailable".
const FEATURE_UNAVAILABLE_STATUSES = new Set([401, 403, 404, 503]);

// isFeatureUnavailable reports whether an error from a best-effort
// gated endpoint should degrade to null (feature simply not available
// to this session) rather than propagate. Network errors (which
// surface as a non-ApiError TypeError from fetch) and 5xx faults are
// deliberately excluded so a broken backend is never hidden.
function isFeatureUnavailable(e: unknown): boolean {
  return e instanceof ApiError && FEATURE_UNAVAILABLE_STATUSES.has(e.status);
}

// BackendPlacementPolicy mirrors placement_policy.Policy on the
// gateway side (metadata/placement_policy/policy.go). Phase 1 does not
// emit an updated_at timestamp, so the frontend synthesizes one at
// read time for display purposes only.
interface BackendPlacementPolicy {
  tenant: string;
  bucket?: string;
  policy: Record<string, unknown>;
}

function backendToFrontendPolicy(raw: BackendPlacementPolicy): PlacementPolicy {
  // id is stable per (tenant, bucket) so the editor's keyed list
  // does not lose selection across saves. name is surfaced to the
  // sidebar as a label; default buckets render as "default".
  const bucket = raw.bucket ?? "";
  return {
    id: bucket ? `${raw.tenant}/${bucket}` : raw.tenant,
    name: bucket || "default",
    yaml: JSON.stringify({ tenant: raw.tenant, bucket, policy: raw.policy ?? {} }, null, 2),
    updatedAt: new Date().toISOString(),
  };
}

// UsageCountersPayload is the shape shared by the REST GET /usage
// response (api/console/handler.go UsageResponse) and each SSE usage
// frame (sse_handler.go UsageStreamEvent): a tenant id, the
// [start, end] window the counters were aggregated over, and the raw
// billing.Dimension -> value map. Counter values are cumulative over
// that window.
export interface UsageCountersPayload {
  tenant_id: string;
  start: string;
  end: string;
  counters?: Record<string, number>;
}

// averageStoredBytes converts the StorageBytesSeconds dimension (the
// time-integral of stored ciphertext bytes — byte-seconds, sampled
// at the control-plane cadence, NOT an instantaneous byte count)
// into the average bytes held over [start, end] by dividing by the
// window length in seconds. This mirrors billing/cost_usage_reader.go,
// which divides the same counter by the month's seconds before
// pricing storage per GiB-month. Treating the raw counter as bytes
// would overstate stored volume — and every cost derived from it —
// by the number of seconds in the window (~2.6M for a 30-day window).
function averageStoredBytes(counters: Record<string, number>, start: string, end: string): number {
  const windowSeconds = (Date.parse(end) - Date.parse(start)) / 1000;
  const byteSeconds = counters["storage_bytes_seconds"] ?? 0;
  return windowSeconds > 0 ? byteSeconds / windowSeconds : 0;
}

// usageSnapshotFromCounters projects a UsageCountersPayload onto the
// UsageSnapshot the dashboard / billing screens render. It is the
// single source of truth for that projection: currentUsage() feeds
// it the REST bootstrap and DashboardPage feeds it each live SSE
// frame, so the two can never drift apart.
export function usageSnapshotFromCounters(raw: UsageCountersPayload): UsageSnapshot {
  const c = raw.counters ?? {};
  return {
    tenantId: raw.tenant_id,
    storageBytes: averageStoredBytes(c, raw.start, raw.end),
    requestsLast30Days:
      (c["put_requests"] ?? 0) +
      (c["get_requests"] ?? 0) +
      (c["list_requests"] ?? 0) +
      (c["delete_requests"] ?? 0),
    egressBytesThisMonth: c["egress_bytes"] ?? 0,
    monthStart: raw.start,
    counters: c,
  };
}

function frontendToBackendPolicy(p: Omit<PlacementPolicy, "updatedAt">): BackendPlacementPolicy {
  // The editor stores the canonical JSON Policy document in the
  // yaml field; we parse it back into the wire shape the gateway
  // expects. Invalid JSON surfaces as a client-side error via the
  // thrown SyntaxError before any network round-trip is wasted.
  const parsed = JSON.parse(p.yaml) as Partial<BackendPlacementPolicy>;
  return {
    tenant: parsed.tenant ?? p.id.split("/")[0] ?? "",
    bucket: parsed.bucket ?? "",
    policy: (parsed.policy ?? {}) as Record<string, unknown>,
  };
}

// Shared default client. Tenant-scoped routes (tenants, usage,
// keys, placement) live under /api/; auth endpoints are versioned
// under /api/v1/auth/. Both dev (Vite proxy) and prod (same-origin
// gateway) resolve these correctly without an explicit base URL.
export const api = new ApiClient("/api");
