import type {
  ApiKey,
  Bucket,
  DedicatedCell,
  PlacementPolicy,
  Tenant,
  TierConfig,
  UsageSnapshot,
} from "./types";

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
      return;
    }
    this.tenantBaseUrl = `${this.rootBaseUrl}/tenants/${encodeURIComponent(tenantId)}`;
  }

  // --- auth -----------------------------------------------------
  //
  // Auth routes intentionally bypass the tenant-scoped baseUrl and
  // hit /api/v1/auth/* directly so the versioned contract in
  // api/console/auth_handler.go is preserved even if the tenant
  // routes ever drop or bump their own version prefix.

  async login(email: string, password: string): Promise<{ tenant: Tenant; token: string }> {
    return this.requestAt("POST", `${this.authBaseUrl}/login`, { email, password });
  }

  async signup(input: {
    email: string;
    password: string;
    tenantName: string;
    captchaToken?: string;
  }): Promise<{ tenant: Tenant; token: string }> {
    return this.requestAt("POST", `${this.authBaseUrl}/signup`, input);
  }

  // --- usage & dashboard ---------------------------------------
  //
  // Backend returns placement_policy-style UsageResponse ({tenant_id,
  // start, end, counters: map[billing.Dimension]uint64}). The SPA
  // renders UsageSnapshot (camelCase, pre-aggregated stat cards) so
  // the client projects counter dimensions onto the snapshot shape.
  // Keep this projection identical to usageFromStreamEvent in
  // DashboardPage.tsx so the REST bootstrap and the SSE live frames
  // populate the same fields.

  async currentUsage(): Promise<UsageSnapshot> {
    const raw = await this.get<BackendUsageResponse>("/usage");
    return backendToUsageSnapshot(raw);
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
    const raw = await this.put<BackendPlacementPolicy>("/placement", body);
    return backendToFrontendPolicy(raw);
  }

  // --- dedicated cells (b2b_dedicated / sovereign only) --------

  async listDedicatedCells(): Promise<DedicatedCell[]> {
    return this.get("/dedicated-cells");
  }

  // --- product tiers (read-only, not tenant-scoped) ------------

  async listTierConfigs(): Promise<TierConfig[]> {
    return this.requestAt("GET", "/api/v1/tiers");
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

// BackendUsageResponse mirrors api/console/handler.go UsageResponse.
// Counter keys are billing.Dimension strings; values are cumulative
// counters over [start, end]. Keep the projection below aligned with
// usageFromStreamEvent in DashboardPage.tsx so the REST bootstrap and
// the SSE live frames render identically.
interface BackendUsageResponse {
  tenant_id: string;
  start: string;
  end: string;
  counters: Record<string, number>;
}

function backendToUsageSnapshot(raw: BackendUsageResponse): UsageSnapshot {
  const c = raw.counters ?? {};
  return {
    tenantId: raw.tenant_id,
    storageBytes: c["storage_bytes_seconds"] ?? 0,
    requestsLast30Days:
      (c["put_requests"] ?? 0) +
      (c["get_requests"] ?? 0) +
      (c["list_requests"] ?? 0) +
      (c["delete_requests"] ?? 0),
    egressBytesThisMonth: c["egress_bytes"] ?? 0,
    monthStart: raw.start,
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
