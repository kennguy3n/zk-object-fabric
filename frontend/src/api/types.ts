// API types mirror the Go-side contracts the tenant console talks to.
// They intentionally stay close to the gateway's /api/v1/ response
// shape rather than introducing camelCase-only wrappers; conversion
// is isolated to src/api/client.ts so the rest of the app only sees
// idiomatic TypeScript.

export type ContractType =
  | "b2c_pooled"
  | "b2b_shared"
  | "b2b_dedicated"
  | "sovereign";

export type LicenseTier = "community" | "standard" | "enterprise";

export interface Tenant {
  id: string;
  name: string;
  contractType: ContractType;
  licenseTier: LicenseTier;
  budgets: TenantBudgets;
  placementDefaultPolicyRef: string;
  createdAt: string;
}

export interface TenantBudgets {
  requestsPerSec: number;
  burstRequests: number;
  egressTbMonth: number;
}

export interface UsageSnapshot {
  tenantId: string;
  storageBytes: number;
  requestsLast30Days: number;
  egressBytesThisMonth: number;
  monthStart: string;
}

export interface Bucket {
  name: string;
  createdAt: string;
  placementPolicyRef: string;
  objectCount: number;
  bytesStored: number;
}

export interface ApiKey {
  id: string;
  accessKey: string;
  // SecretKey is only returned at creation time. Subsequent reads
  // surface accessKey + createdAt only.
  secretKey?: string;
  createdAt: string;
  lastUsedAt?: string;
}

export interface PlacementPolicy {
  id: string;
  name: string;
  yaml: string; // canonical YAML representation (see docs/PROPOSAL.md §3.6)
  updatedAt: string;
}

export interface DedicatedCell {
  id: string;
  region: string;
  country: string;
  status: "provisioning" | "active" | "decommissioning";
  capacityPetabytes: number;
  utilization: number; // 0..1
}
