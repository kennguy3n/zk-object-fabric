import type {
  ApiKey,
  Bucket,
  DedicatedCell,
  PlacementPolicy,
  Tenant,
  UsageSnapshot,
} from "./types";

// ApiClient is the thin wrapper the SPA uses to reach the gateway's
// management API. Auth endpoints live under /api/v1/auth/ because
// the backend handler registers them under that versioned prefix
// (see api/console/auth_handler.go), while tenant-scoped routes
// (tenants, usage, keys, placement) live under /api/ to match the
// mux registered in api/console/handler.go:Handler.Register. Feature
// code never pokes fetch directly so swapping the transport (msw
// for tests, a batching client, etc.) is a one-file change.
export class ApiClient {
  constructor(
    private readonly baseUrl: string,
    private token?: string,
    private readonly authBaseUrl: string = `${baseUrl}/v1/auth`,
  ) {}

  setToken(token: string | undefined) {
    this.token = token;
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
  }): Promise<{ tenant: Tenant; token: string }> {
    return this.requestAt("POST", `${this.authBaseUrl}/signup`, input);
  }

  // --- usage & dashboard ---------------------------------------

  async currentUsage(): Promise<UsageSnapshot> {
    return this.get("/usage");
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
    return this.get("/api-keys");
  }

  async createApiKey(): Promise<ApiKey> {
    return this.post("/api-keys", {});
  }

  async revokeApiKey(id: string): Promise<void> {
    await this.request("DELETE", `/api-keys/${encodeURIComponent(id)}`);
  }

  // --- placement policies --------------------------------------

  async listPlacementPolicies(): Promise<PlacementPolicy[]> {
    return this.get("/placement-policies");
  }

  async savePlacementPolicy(policy: Omit<PlacementPolicy, "updatedAt">): Promise<PlacementPolicy> {
    return this.put(`/placement-policies/${encodeURIComponent(policy.id)}`, policy);
  }

  // --- dedicated cells (b2b_dedicated / sovereign only) --------

  async listDedicatedCells(): Promise<DedicatedCell[]> {
    return this.get("/dedicated-cells");
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
    return this.requestAt(method, `${this.baseUrl}${path}`, body);
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

// Shared default client. Tenant-scoped routes (tenants, usage,
// keys, placement) live under /api/; auth endpoints are versioned
// under /api/v1/auth/. Both dev (Vite proxy) and prod (same-origin
// gateway) resolve these correctly without an explicit base URL.
export const api = new ApiClient("/api");
