import type {
  ApiKey,
  Bucket,
  DedicatedCell,
  PlacementPolicy,
  Tenant,
  UsageSnapshot,
} from "./types";

// ApiClient is the thin wrapper the SPA uses to reach the gateway's
// /api/v1/ management API. It is intentionally tiny: just fetch +
// JSON + an Authorization header. Feature code never pokes fetch
// directly so swapping the transport (msw for tests, a batching
// client, etc.) is a one-file change.
export class ApiClient {
  constructor(private readonly baseUrl: string, private token?: string) {}

  setToken(token: string | undefined) {
    this.token = token;
  }

  // --- auth -----------------------------------------------------

  async login(email: string, password: string): Promise<{ tenant: Tenant; token: string }> {
    return this.post("/auth/login", { email, password });
  }

  async signup(input: {
    email: string;
    password: string;
    tenantName: string;
  }): Promise<{ tenant: Tenant; token: string }> {
    return this.post("/auth/signup", input);
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
    const res = await fetch(`${this.baseUrl}${path}`, {
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

// Shared default client. In production it points at the relative
// /api/v1 path so both dev (Vite proxy) and prod (same-origin
// gateway) work without an explicit base URL.
export const api = new ApiClient("/api/v1");
