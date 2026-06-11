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

// LicenseTier mirrors metadata/tenant.LicenseTier on the backend.
// The backend is the source of truth — adding a value here without
// adding it to the Go LicenseTier.Valid() switch would let the SPA
// render a tier the gateway immediately rejects.
export type LicenseTier =
  | "beta"
  | "archive"
  | "standard"
  | "hot"
  | "dedicated"
  | "sovereign";

export interface Tenant {
  id: string;
  name: string;
  contractType: ContractType;
  licenseTier: LicenseTier;
  budgets: TenantBudgets;
  placementDefaultPolicyRef: string;
  // The backend populates createdAt on signup but older tenant
  // records minted before the field existed omit it, and the
  // login endpoint does not echo it back yet. Keep it optional so
  // neither case renders "Invalid Date" on the tenant card.
  createdAt?: string;
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
  // counters is the raw billing.Dimension -> value map the backend
  // returns, retained so the dashboard can render the per-verb request
  // mix without a second projection. Optional because older callers
  // that only read the aggregated fields never populate it.
  counters?: Record<string, number>;
}

export interface Bucket {
  name: string;
  createdAt: string;
  placementPolicyRef: string;
  objectCount: number;
  bytesStored: number;
}

// ApiKey mirrors the backend's APIKeyDescriptor (api/console/handler.go).
// The list endpoint only populates accessKey + createdAt; secretKey is
// additionally present on the create response (one-time display) and
// lastUsedAt is surfaced by the gateway when the auth layer starts
// recording it — both are optional so the list view renders without
// a second fetch.
export interface ApiKey {
  accessKey: string;
  createdAt: string;
  secretKey?: string;
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

// CostBreakdown mirrors billing.CostBreakdown
// (billing/cost_aggregator.go), returned by
// GET /api/v1/tenants/{id}/cost-breakdown. Field names stay snake_case
// to match the wire contract; the page maps them to display rows. This
// route is admin-gated, so the console treats it as best-effort and
// degrades to the usage×tier estimate when it is not reachable.
export interface CostBreakdown {
  tenant_id: string;
  month: string;
  wasabi_storage_usd: number;
  linode_compute_usd: number;
  aws_control_plane_usd: number;
  dedup_savings_usd: number;
  total_usd: number;
}

// MigrationJobState mirrors migration.JobState on the backend.
export type MigrationJobState = "pending" | "running" | "done" | "failed";

// MigrationJob mirrors migration.MigrationJob
// (migration/fleet_orchestrator.go), returned by GET /api/v1/migrations.
export interface MigrationJob {
  job_id: string;
  tenant_id: string;
  bucket: string;
  source_backend: string;
  dest_cell_id: string;
  dest_backend: string;
  bytes_per_second: number;
  state: MigrationJobState;
  started_at?: string;
  completed_at?: string;
  error?: string;
  bytes_copied: number;
  pieces_copied: number;
}

// DedupPolicy mirrors the GET/POST dedup-policy response
// (api/console/dedup_handler.go). scope/level are absent when dedup has
// never been enabled for the bucket.
export interface DedupPolicy {
  tenant_id: string;
  bucket: string;
  enabled: boolean;
  scope?: string;
  level?: "object" | "object+block";
}

// ProvisionCellInput is the POST /dedicated-cells request body
// (api/console.ProvisionDedicatedCellRequest).
export interface ProvisionCellInput {
  region: string;
  country: string;
  capacityPetabytes: number;
  erasureProfile?: string;
  nodeCount: number;
}

// CellStatus mirrors cellops.CellStatus, the 202 response from
// provisioning a dedicated cell.
export interface CellStatus {
  cell_id: string;
  tenant_id: string;
  region: string;
  country: string;
  status: "provisioning" | "active" | "decommissioning";
  capacity_petabytes: number;
  utilization: number;
  erasure_profile: string;
  node_count: number;
  created_at: string;
  updated_at: string;
}

// TierConfig mirrors metadata/tenant/tier_config.go TierConfig.
// Surfaced by GET /api/v1/tiers.
export interface TierConfig {
  tier: LicenseTier;
  display_name: string;
  default_ec_profile: string;
  cache_policy: string;
  dedup_policy: string;
  egress_budget_tb_month: number;
  price_per_tb_month: number;
  placement_mode: string;
  country_locked: boolean;
}
