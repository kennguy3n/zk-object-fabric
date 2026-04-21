// Package hot_object_cache defines the interface for the L0/L1 hot
// object cache that sits between the Linode gateway and the Wasabi
// origin. See docs/PROPOSAL.md §3.6 and §3.10.
//
// The cache stores ciphertext, not plaintext. It is keyed by piece ID
// so that range-aligned encrypted chunks can be served directly
// without reconstruction.
package hot_object_cache

import (
	"context"
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
