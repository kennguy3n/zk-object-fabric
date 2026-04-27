// Package billing defines the per-tenant billing counter types used by
// the control plane. See docs/PROPOSAL.md §3.5 and §5.1.
//
// The metering types are intentionally minimal for Phase 1. The full
// billing system (ClickHouse ingestion, invoice generation, SLA
// reporting) is deferred until the Phase 2 prototype.
package billing

import "time"

// Counter is a single monotonically-increasing usage dimension. It is
// safe to store and advance for a given (tenant, bucket, dimension)
// tuple.
type Counter struct {
	TenantID    string
	Bucket      string
	Dimension   Dimension
	Value       uint64
	PeriodStart time.Time
	PeriodEnd   time.Time
}

// Dimension names the billable usage dimension.
type Dimension string

const (
	// StorageBytesSeconds is the integral of stored ciphertext bytes
	// over time, sampled at the control-plane cadence.
	StorageBytesSeconds Dimension = "storage_bytes_seconds"

	// PutRequests counts successful PUT requests.
	PutRequests Dimension = "put_requests"

	// GetRequests counts successful GET requests.
	GetRequests Dimension = "get_requests"

	// ListRequests counts successful LIST requests.
	ListRequests Dimension = "list_requests"

	// DeleteRequests counts successful DELETE requests.
	DeleteRequests Dimension = "delete_requests"

	// EgressBytes counts ciphertext bytes served to clients.
	EgressBytes Dimension = "egress_bytes"

	// OriginEgressBytes counts ciphertext bytes read from the Wasabi
	// origin. Used to monitor the Wasabi fair-use ratio (§3.11).
	OriginEgressBytes Dimension = "origin_egress_bytes"

	// CacheHits counts hot-cache hits. Reported as a product metric
	// per §3.11.
	CacheHits Dimension = "cache_hits"

	// CacheMisses counts hot-cache misses.
	CacheMisses Dimension = "cache_misses"

	// AbuseAnomalyAlert is emitted by the gateway's rate limiter
	// when a tenant's recent request or egress rate exceeds the
	// configured multiple of its historical baseline. The Delta
	// field on these events carries the ratio (current / baseline)
	// rounded to the nearest integer so downstream consumers can
	// threshold on severity.
	AbuseAnomalyAlert Dimension = "abuse_anomaly_alert"

	// AbuseBudgetExhausted is emitted when a tenant's monthly
	// egress budget (Budgets.EgressTBMonth) is exhausted and the
	// rate limiter starts rejecting requests with HTTP 429.
	AbuseBudgetExhausted Dimension = "abuse_budget_exhausted"

	// TenantCreated is emitted once per tenant, at signup time, so
	// downstream billing systems (ClickHouse, invoice generation)
	// start tracking the tenant from its creation instant rather
	// than waiting for the first S3 request. Delta is always 1.
	TenantCreated Dimension = "tenant_created"

	// DedupHits counts PUT requests that landed on an existing
	// content_index entry within the tenant. Each event represents
	// one PUT that did NOT trigger a backend write because the
	// gateway recognized convergent ciphertext. Delta is always 1.
	DedupHits Dimension = "dedup_hits"

	// DedupBytesSaved sums the on-wire ciphertext sizes the gateway
	// avoided writing because of intra-tenant dedup. Operators
	// reconcile DedupBytesSaved against StorageBytesSeconds to
	// quantify the dedup ratio per tenant per bucket.
	DedupBytesSaved Dimension = "dedup_bytes_saved"

	// DedupRefCount is a sample, not a counter: emitted on PUT and
	// DELETE so the billing pipeline can reconstruct the running
	// reference count for hot content. Delta carries the new
	// refcount value.
	DedupRefCount Dimension = "dedup_ref_count"
)

// UsageEvent is a single raw event emitted by the gateway. The billing
// pipeline aggregates these into Counter rows.
type UsageEvent struct {
	TenantID     string
	Bucket       string
	Dimension    Dimension
	Delta        uint64
	ObservedAt   time.Time
	SourceNodeID string
}

// BudgetPolicy bounds a tenant's usage for a single dimension within a
// monthly window.
type BudgetPolicy struct {
	TenantID    string
	Dimension   Dimension
	SoftCap     uint64
	HardCap     uint64
	BurstPerSec uint64
	Currency    string
}
