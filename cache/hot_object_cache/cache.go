// Package hot_object_cache defines the interface for the L0/L1 hot
// object cache that sits between the Linode gateway and the Wasabi
// origin. See docs/PROPOSAL.md §3.6 and §3.10.
//
// The cache stores ciphertext, not plaintext. It is keyed by piece ID
// so that range-aligned encrypted chunks can be served directly
// without reconstruction.
//
// Sizing guidance for the Linode hot cache (Phase 2+):
//
//   - NVMe-backed Linode shapes are preferred for L0 (edge) because
//     per-object lookup latency dominates. Target a minimum of
//     400 GiB of NVMe per gateway for a small cell; 2–4 TiB for a
//     busy cell. Size for roughly 2× the working-set hot bytes so
//     the LRU has headroom.
//
//   - Block-storage Linode volumes are acceptable for L1 (regional)
//     where throughput and cost per GiB matter more than tail
//     latency. Target 4–16 TiB per Linode L1 node. Provision one L1
//     node per cell region unless tenant placement policies require
//     more.
//
//   - Cache capacity should be sized against the product target
//     "Hot tier hit ratio > 90%" (see docs/PROGRESS.md). Under-
//     provisioned caches push Wasabi origin egress toward the
//     fair-use ceiling (egress ÷ stored > 1).
package hot_object_cache

import (
	"context"
	"fmt"
	"io"
	"time"
)

// HotObjectCache is the interface implemented by the L0/L1 cache.
type HotObjectCache interface {
	// Get returns a reader for the cached piece. It returns
	// ErrCacheMiss if the piece is not in the cache.
	Get(ctx context.Context, pieceID string) (io.ReadCloser, CachedPieceMetadata, error)

	// Put stores a piece in the cache. Implementations may stream the
	// body to disk; they MUST record size and hash on completion.
	Put(ctx context.Context, pieceID string, r io.Reader, opts PutOptions) error

	// Evict removes a piece from the cache. It is idempotent.
	Evict(ctx context.Context, pieceID string) error

	// Stats reports cache-wide counters.
	Stats() Stats
}

// PutOptions carries per-entry hints for the cache writer.
type PutOptions struct {
	SizeBytes  int64
	Hash       string
	TTL        time.Duration
	PinHot     bool
	StorageKey string
}

// CachedPieceMetadata describes an in-cache entry.
type CachedPieceMetadata struct {
	PieceID    string
	SizeBytes  int64
	Hash       string
	StoredAt   time.Time
	LastAccess time.Time
	HitCount   uint64
	Pinned     bool
}

// Stats is the aggregate cache snapshot.
type Stats struct {
	Entries    int64
	BytesUsed  int64
	BytesLimit int64
	Hits       uint64
	Misses     uint64
	Evictions  uint64
}

// PromotionSignal is raised by the read path when a piece crosses a
// promotion threshold. The cache consumes these signals to decide
// what to pull from the Wasabi origin into the Linode cache.
type PromotionSignal struct {
	PieceID       string
	TenantID      string
	ReadBytes     int64
	ReadCount     uint64
	ObservedAt    time.Time
	OriginBackend string
}

// CacheTier names a cache layer. The data plane uses three tiers in
// Phase 2+:
//
//   - TierL0: per-gateway NVMe edge cache.
//   - TierL1: regional hot replica (block storage or NVMe).
//   - TierL2: durable origin (Wasabi, or a local cell in later phases).
type CacheTier string

const (
	TierL0 CacheTier = "l0"
	TierL1 CacheTier = "l1"
	TierL2 CacheTier = "l2"
)

// PromotionPolicy specifies when a piece is promoted from the Wasabi
// origin into the Linode hot cache. The policy is evaluated on every
// read-path signal; any threshold crossed at its tier promotes the
// piece.
//
// The thresholds are intentionally conservative so that the Phase 2
// promotion engine can start with these values and tighten them
// against real traffic measured by the benchmark suite.
type PromotionPolicy struct {
	// Tier is the destination tier (L0 or L1).
	Tier CacheTier

	// MonthlyEgressRatioThreshold is the per-tenant egress ratio
	// (origin bytes read ÷ stored bytes) above which pieces are
	// eagerly promoted to this tier. Setting this at or below the
	// Wasabi fair-use ceiling of 1.0 keeps origin egress inside
	// budget.
	MonthlyEgressRatioThreshold float64

	// DailyReadCountThreshold is the per-piece read count over the
	// last 24 hours that triggers promotion. Matches the hot-tier
	// promotion rule referenced in docs/PROPOSAL.md §3.11.
	DailyReadCountThreshold uint64

	// P95LatencyMissMs is the observed p95 miss latency ceiling.
	// When the cache miss path regularly exceeds this, the promotion
	// engine pulls more pieces into the tier rather than letting
	// tail latency climb.
	P95LatencyMissMs int

	// MinPieceSizeBytes skips promotion for tiny pieces where the
	// metadata overhead outweighs the latency win.
	MinPieceSizeBytes int64

	// MaxPieceSizeBytes skips promotion for very large pieces that
	// would evict too many small entries.
	MaxPieceSizeBytes int64

	// PinHotByDefault, when true, pins newly promoted pieces to the
	// hot region of the LRU (see EvictionPolicy) so they are not
	// evicted before they have warmed their real read count.
	PinHotByDefault bool
}

// DefaultPromotionPolicies returns the conservative Phase 2 defaults
// for L0 (edge NVMe) and L1 (regional) tiers. Operators may override
// any field per tenant or per cell.
func DefaultPromotionPolicies() []PromotionPolicy {
	return []PromotionPolicy{
		{
			Tier:                        TierL0,
			MonthlyEgressRatioThreshold: 0.7,
			DailyReadCountThreshold:     3,
			P95LatencyMissMs:            250,
			MinPieceSizeBytes:           64 * 1024,        // 64 KiB
			MaxPieceSizeBytes:           64 * 1024 * 1024, // 64 MiB
			PinHotByDefault:             true,
		},
		{
			Tier:                        TierL1,
			MonthlyEgressRatioThreshold: 0.5,
			DailyReadCountThreshold:     2,
			P95LatencyMissMs:            500,
			MinPieceSizeBytes:           1 * 1024,               // 1 KiB
			MaxPieceSizeBytes:           1 * 1024 * 1024 * 1024, // 1 GiB
			PinHotByDefault:             false,
		},
	}
}

// Validate performs structural checks on a single promotion policy.
func (p PromotionPolicy) Validate() error {
	switch p.Tier {
	case TierL0, TierL1, TierL2:
	case "":
		return fmt.Errorf("hot_object_cache: promotion policy tier is required")
	default:
		return fmt.Errorf("hot_object_cache: unknown tier %q", p.Tier)
	}
	if p.MonthlyEgressRatioThreshold < 0 {
		return fmt.Errorf("hot_object_cache: monthly_egress_ratio_threshold must be non-negative")
	}
	if p.P95LatencyMissMs < 0 {
		return fmt.Errorf("hot_object_cache: p95_latency_miss_ms must be non-negative")
	}
	if p.MinPieceSizeBytes < 0 {
		return fmt.Errorf("hot_object_cache: min_piece_size_bytes must be non-negative")
	}
	if p.MaxPieceSizeBytes != 0 && p.MinPieceSizeBytes > p.MaxPieceSizeBytes {
		return fmt.Errorf("hot_object_cache: min_piece_size_bytes (%d) must be <= max_piece_size_bytes (%d)", p.MinPieceSizeBytes, p.MaxPieceSizeBytes)
	}
	return nil
}

// EvictionPolicyKind names the eviction algorithm used by a cache
// tier. Phase 2 ships LRU with a hot-pin extension; the enum is wider
// so later phases can add LFU or TinyLFU without breaking consumers.
type EvictionPolicyKind string

const (
	// EvictionLRU is standard Least-Recently-Used eviction.
	EvictionLRU EvictionPolicyKind = "lru"

	// EvictionLRUHotPin is LRU with a distinguished "hot" region:
	// pieces promoted with PinHotByDefault=true live in the hot
	// region until their hit count decays below HotDemotionHitCount,
	// at which point they re-enter the normal LRU.
	EvictionLRUHotPin EvictionPolicyKind = "lru_hot_pin"
)

// EvictionPolicy configures the eviction algorithm for a cache tier.
type EvictionPolicy struct {
	// Kind selects the algorithm.
	Kind EvictionPolicyKind

	// MaxBytes is the upper bound on total cached bytes on this
	// tier. Zero means "unbounded".
	MaxBytes int64

	// HotRegionFraction is the fraction of MaxBytes reserved for the
	// hot-pin region. Only meaningful for EvictionLRUHotPin. A value
	// of 0.1 reserves 10% of capacity for hot-pinned pieces.
	HotRegionFraction float64

	// HotDemotionHitCount is the hit-count threshold below which a
	// hot-pinned piece is demoted back into the main LRU. Only
	// meaningful for EvictionLRUHotPin.
	HotDemotionHitCount uint64

	// TTL, when non-zero, forces eviction of any piece older than
	// this regardless of recency. Useful for regulated tenants that
	// require bounded residency in a tier.
	TTL time.Duration
}

// DefaultEvictionPolicy returns the Phase 2 default: LRU with
// hot-pin support and a 10% hot region.
func DefaultEvictionPolicy(maxBytes int64) EvictionPolicy {
	return EvictionPolicy{
		Kind:                EvictionLRUHotPin,
		MaxBytes:            maxBytes,
		HotRegionFraction:   0.1,
		HotDemotionHitCount: 1,
	}
}

// Validate performs structural checks on an eviction policy.
func (e EvictionPolicy) Validate() error {
	switch e.Kind {
	case EvictionLRU, EvictionLRUHotPin:
	case "":
		return fmt.Errorf("hot_object_cache: eviction policy kind is required")
	default:
		return fmt.Errorf("hot_object_cache: unknown eviction kind %q", e.Kind)
	}
	if e.MaxBytes < 0 {
		return fmt.Errorf("hot_object_cache: eviction max_bytes must be non-negative")
	}
	if e.HotRegionFraction < 0 || e.HotRegionFraction >= 1 {
		return fmt.Errorf("hot_object_cache: eviction hot_region_fraction must be in [0, 1)")
	}
	if e.TTL < 0 {
		return fmt.Errorf("hot_object_cache: eviction ttl must be non-negative")
	}
	if e.Kind == EvictionLRU && (e.HotRegionFraction != 0 || e.HotDemotionHitCount != 0) {
		return fmt.Errorf("hot_object_cache: hot-pin fields only apply to eviction kind %q", EvictionLRUHotPin)
	}
	return nil
}
