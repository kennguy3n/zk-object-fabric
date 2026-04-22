package hot_object_cache

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ErrCacheMiss is returned by HotObjectCache.Get when a piece is not
// present in the cache.
var ErrCacheMiss = errors.New("hot_object_cache: miss")

// MemoryCache is the Phase 2 in-memory HotObjectCache. It uses a
// doubly-linked list keyed by a hash map for O(1) LRU bookkeeping
// and supports a hot-pin region per EvictionPolicy.
//
// The implementation buffers piece bodies in memory. Phase 2 uses it
// for the L0/L1 hot cache on small gateways and for tests; Phase 3
// adds an NVMe-backed disk cache behind the same interface.
type MemoryCache struct {
	mu        sync.Mutex
	policy    EvictionPolicy
	hot       *list.List          // hot-pin region (MRU at front)
	main      *list.List          // main LRU region
	index     map[string]*list.Element
	bytesUsed int64
	// hotLimit caches int64(policy.HotRegionFraction * policy.MaxBytes)
	// so the hot sub-region cannot grow past its reserved slice of
	// total capacity. Zero means "hot region unbounded".
	hotLimit  int64
	hotBytes  int64
	stats     Stats
	clock     func() time.Time
}

type cacheEntry struct {
	pieceID    string
	body       []byte
	hash       string
	storedAt   time.Time
	lastAccess time.Time
	hits       uint64
	pinned     bool
	expiresAt  time.Time
}

// NewMemoryCache builds a MemoryCache honouring the eviction policy.
// The policy is validated before the cache is constructed.
func NewMemoryCache(policy EvictionPolicy) (*MemoryCache, error) {
	if policy.Kind == "" {
		policy.Kind = EvictionLRU
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	c := &MemoryCache{
		policy: policy,
		hot:    list.New(),
		main:   list.New(),
		index:  map[string]*list.Element{},
		stats:  Stats{BytesLimit: policy.MaxBytes},
		clock:  time.Now,
	}
	if policy.Kind == EvictionLRUHotPin && policy.HotRegionFraction > 0 && policy.MaxBytes > 0 {
		c.hotLimit = int64(float64(policy.MaxBytes) * policy.HotRegionFraction)
	}
	return c, nil
}

// Get returns a reader for the cached piece, or ErrCacheMiss.
func (c *MemoryCache) Get(_ context.Context, pieceID string) (io.ReadCloser, CachedPieceMetadata, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.index[pieceID]
	if !ok {
		c.stats.Misses++
		return nil, CachedPieceMetadata{}, ErrCacheMiss
	}
	entry := el.Value.(*cacheEntry)
	now := c.clock()
	if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
		c.removeLocked(el)
		c.stats.Misses++
		return nil, CachedPieceMetadata{}, ErrCacheMiss
	}

	entry.hits++
	entry.lastAccess = now
	c.stats.Hits++
	if entry.pinned {
		c.hot.MoveToFront(el)
		if c.policy.HotDemotionHitCount > 0 && entry.hits < c.policy.HotDemotionHitCount {
			// Entry has not yet warmed past the demotion threshold.
			// Leave it in the hot region.
		}
	} else {
		c.main.MoveToFront(el)
	}

	md := CachedPieceMetadata{
		PieceID:    entry.pieceID,
		SizeBytes:  int64(len(entry.body)),
		Hash:       entry.hash,
		StoredAt:   entry.storedAt,
		LastAccess: entry.lastAccess,
		HitCount:   entry.hits,
		Pinned:     entry.pinned,
	}
	return io.NopCloser(bytes.NewReader(entry.body)), md, nil
}

// Put stores a piece in the cache.
func (c *MemoryCache) Put(_ context.Context, pieceID string, r io.Reader, opts PutOptions) error {
	if pieceID == "" {
		return errors.New("hot_object_cache: piece_id is required")
	}
	if r == nil {
		return errors.New("hot_object_cache: reader is required")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("hot_object_cache: buffer piece: %w", err)
	}
	size := int64(len(data))
	if c.policy.MaxBytes > 0 && size > c.policy.MaxBytes {
		return fmt.Errorf("hot_object_cache: piece %d bytes exceeds cache capacity %d", size, c.policy.MaxBytes)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.index[pieceID]; ok {
		c.removeLocked(existing)
	}

	now := c.clock()
	pin := opts.PinHot && c.policy.Kind == EvictionLRUHotPin
	entry := &cacheEntry{
		pieceID:    pieceID,
		body:       data,
		hash:       opts.Hash,
		storedAt:   now,
		lastAccess: now,
		pinned:     pin,
	}
	if opts.TTL > 0 {
		entry.expiresAt = now.Add(opts.TTL)
	} else if c.policy.TTL > 0 {
		entry.expiresAt = now.Add(c.policy.TTL)
	}

	if pin {
		c.evictHotLocked(size)
	}
	c.evictMainLocked(size)

	var el *list.Element
	if pin {
		el = c.hot.PushFront(entry)
		c.hotBytes += size
	} else {
		el = c.main.PushFront(entry)
	}
	c.index[pieceID] = el
	c.bytesUsed += size
	c.stats.Entries = int64(len(c.index))
	c.stats.BytesUsed = c.bytesUsed
	return nil
}

// Evict removes a piece from the cache. Missing pieces are silent.
func (c *MemoryCache) Evict(_ context.Context, pieceID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[pieceID]
	if !ok {
		return nil
	}
	c.removeLocked(el)
	return nil
}

// Stats returns a snapshot of cache-wide counters.
func (c *MemoryCache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// evictMainLocked evicts main-region entries until the cache has
// capacity for incoming bytes.
func (c *MemoryCache) evictMainLocked(incoming int64) {
	if c.policy.MaxBytes <= 0 {
		return
	}
	for c.bytesUsed+incoming > c.policy.MaxBytes {
		back := c.main.Back()
		if back == nil {
			// Main region is empty; fall back to evicting coldest
			// hot entries. This keeps the cache bounded even when
			// every entry is pinned — the hot region is advisory,
			// not a hard reservation.
			back = c.hot.Back()
			if back == nil {
				return
			}
		}
		c.removeLocked(back)
	}
}

// evictHotLocked evicts oldest hot entries to make space for a new
// pinned piece inside the hot-region budget.
func (c *MemoryCache) evictHotLocked(incoming int64) {
	if c.hotLimit <= 0 {
		return
	}
	for c.hotBytes+incoming > c.hotLimit {
		back := c.hot.Back()
		if back == nil {
			return
		}
		c.removeLocked(back)
	}
}

func (c *MemoryCache) removeLocked(el *list.Element) {
	entry := el.Value.(*cacheEntry)
	size := int64(len(entry.body))
	if entry.pinned {
		c.hot.Remove(el)
		c.hotBytes -= size
	} else {
		c.main.Remove(el)
	}
	delete(c.index, entry.pieceID)
	c.bytesUsed -= size
	c.stats.Entries = int64(len(c.index))
	c.stats.BytesUsed = c.bytesUsed
	c.stats.Evictions++
}

var _ HotObjectCache = (*MemoryCache)(nil)
