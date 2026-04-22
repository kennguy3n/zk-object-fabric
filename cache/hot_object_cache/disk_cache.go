package hot_object_cache

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// DiskCache is an NVMe / block-storage backed HotObjectCache.
//
// Piece bodies are written to {RootPath}/{shard}/{pieceID}.bin and
// metadata is written to {RootPath}/{shard}/{pieceID}.meta.json. A
// shard prefix (the first two bytes of the pieceID) keeps any one
// directory from holding millions of files.
//
// The in-memory index is rebuilt from disk on Open so cache entries
// survive gateway restarts — the principal reason to run a disk
// cache over MemoryCache on production Linode nodes.
type DiskCache struct {
	mu sync.Mutex

	policy EvictionPolicy
	root   string

	hot   *list.List
	main  *list.List
	index map[string]*list.Element

	bytesUsed int64
	hotLimit  int64
	hotBytes  int64

	stats Stats
	clock func() time.Time
}

// DiskCacheConfig captures the on-disk cache's tuning knobs.
type DiskCacheConfig struct {
	// RootPath is the directory on the NVMe / block-storage volume
	// that owns the cache. It is created on Open if missing.
	RootPath string

	// Policy governs capacity, hot-region sizing, and TTL.
	Policy EvictionPolicy

	// Clock, if set, returns the current time. Tests override it.
	Clock func() time.Time
}

// metaFile is the on-disk sidecar record. Piece bodies are stored
// next to their sidecars so an operator can inspect the cache with
// just a filesystem browser.
type metaFile struct {
	PieceID   string    `json:"piece_id"`
	SizeBytes int64     `json:"size_bytes"`
	Hash      string    `json:"hash,omitempty"`
	StoredAt  time.Time `json:"stored_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Pinned    bool      `json:"pinned,omitempty"`
	HitCount  uint64    `json:"hit_count,omitempty"`
}

type diskEntry struct {
	pieceID    string
	sizeBytes  int64
	hash       string
	storedAt   time.Time
	lastAccess time.Time
	expiresAt  time.Time
	pinned     bool
	hits       uint64
}

// NewDiskCache constructs (and warms from disk) a DiskCache.
func NewDiskCache(cfg DiskCacheConfig) (*DiskCache, error) {
	if cfg.RootPath == "" {
		return nil, errors.New("hot_object_cache: disk cache root_path is required")
	}
	if cfg.Policy.Kind == "" {
		cfg.Policy.Kind = EvictionLRU
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.RootPath, 0o755); err != nil {
		return nil, fmt.Errorf("hot_object_cache: create cache root %q: %w", cfg.RootPath, err)
	}
	c := &DiskCache{
		policy: cfg.Policy,
		root:   cfg.RootPath,
		hot:    list.New(),
		main:   list.New(),
		index:  map[string]*list.Element{},
		stats:  Stats{BytesLimit: cfg.Policy.MaxBytes},
		clock:  cfg.Clock,
	}
	if c.clock == nil {
		c.clock = time.Now
	}
	if cfg.Policy.Kind == EvictionLRUHotPin && cfg.Policy.HotRegionFraction > 0 && cfg.Policy.MaxBytes > 0 {
		c.hotLimit = int64(float64(cfg.Policy.MaxBytes) * cfg.Policy.HotRegionFraction)
	}
	if err := c.warm(); err != nil {
		return nil, err
	}
	return c, nil
}

// testHookGetAfterUnlock is invoked (when non-nil) by Get between
// the first c.mu.Unlock() and the subsequent os.Open. Tests set it
// to deterministically interleave a concurrent Put with the Get
// corruption-recovery path; production leaves it nil.
var testHookGetAfterUnlock func()

// Get returns a reader for the cached piece, or ErrCacheMiss.
func (c *DiskCache) Get(_ context.Context, pieceID string) (io.ReadCloser, CachedPieceMetadata, error) {
	c.mu.Lock()
	el, ok := c.index[pieceID]
	if !ok {
		c.stats.Misses++
		c.mu.Unlock()
		return nil, CachedPieceMetadata{}, ErrCacheMiss
	}
	entry := el.Value.(*diskEntry)
	now := c.clock()
	if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
		c.removeLocked(el)
		c.stats.Misses++
		c.mu.Unlock()
		return nil, CachedPieceMetadata{}, ErrCacheMiss
	}

	entry.hits++
	entry.lastAccess = now
	c.stats.Hits++
	if entry.pinned {
		c.hot.MoveToFront(el)
	} else {
		c.main.MoveToFront(el)
	}
	bodyPath := c.bodyPath(pieceID)
	md := CachedPieceMetadata{
		PieceID:    entry.pieceID,
		SizeBytes:  entry.sizeBytes,
		Hash:       entry.hash,
		StoredAt:   entry.storedAt,
		LastAccess: entry.lastAccess,
		HitCount:   entry.hits,
		Pinned:     entry.pinned,
	}
	c.mu.Unlock()

	if testHookGetAfterUnlock != nil {
		testHookGetAfterUnlock()
	}

	f, err := os.Open(bodyPath)
	if err != nil {
		// On-disk file disappeared under us (corruption, manual
		// delete). Treat as a miss and drop the index entry so the
		// next PUT repopulates cleanly. The optimistic hit
		// recorded above is compensated so Stats() reports the
		// true hit ratio that the Wasabi fair-use guardrails and
		// the PROGRESS.md hot-tier targets rely on.
		//
		// Between the first c.mu.Unlock() above and this re-lock
		// a concurrent Put() may have replaced the corrupt entry
		// with a fresh one. Only evict the index entry when it is
		// still the element we originally observed; otherwise the
		// new body belongs to the new entry and must survive.
		c.mu.Lock()
		if el2, ok := c.index[pieceID]; ok && el2 == el {
			c.removeLocked(el2)
		}
		if c.stats.Hits > 0 {
			c.stats.Hits--
		}
		c.stats.Misses++
		c.mu.Unlock()
		return nil, CachedPieceMetadata{}, ErrCacheMiss
	}
	return f, md, nil
}

// Put stores a piece in the cache.
func (c *DiskCache) Put(_ context.Context, pieceID string, r io.Reader, opts PutOptions) error {
	if pieceID == "" {
		return errors.New("hot_object_cache: piece_id is required")
	}
	if r == nil {
		return errors.New("hot_object_cache: reader is required")
	}

	shardDir := c.shardDir(pieceID)
	if err := os.MkdirAll(shardDir, 0o755); err != nil {
		return fmt.Errorf("hot_object_cache: create shard dir: %w", err)
	}
	tmp, err := os.CreateTemp(shardDir, pieceID+".*.tmp")
	if err != nil {
		return fmt.Errorf("hot_object_cache: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	size, copyErr := io.Copy(tmp, r)
	closeErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("hot_object_cache: write body: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("hot_object_cache: close body: %w", closeErr)
	}
	if c.policy.MaxBytes > 0 && size > c.policy.MaxBytes {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("hot_object_cache: piece %d bytes exceeds cache capacity %d", size, c.policy.MaxBytes)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.index[pieceID]; ok {
		c.removeLocked(existing)
	}

	now := c.clock()
	pin := opts.PinHot && c.policy.Kind == EvictionLRUHotPin
	entry := &diskEntry{
		pieceID:    pieceID,
		sizeBytes:  size,
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

	bodyPath := c.bodyPath(pieceID)
	if err := os.Rename(tmpPath, bodyPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("hot_object_cache: publish body: %w", err)
	}
	if err := writeMeta(c.metaPath(pieceID), entry); err != nil {
		_ = os.Remove(bodyPath)
		return err
	}

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

// Evict removes a piece from the cache. It is idempotent.
func (c *DiskCache) Evict(_ context.Context, pieceID string) error {
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
func (c *DiskCache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// warm walks the root directory and rebuilds the in-memory index
// from every sidecar file it finds. Files whose sidecar has expired
// are removed. Bodies without a sidecar are considered stale and
// removed too.
func (c *DiskCache) warm() error {
	entries, err := readShards(c.root)
	if err != nil {
		return err
	}
	// Hydrate in StoredAt order so the LRU reflects the order the
	// pieces were originally written. Newest-first matches how
	// MemoryCache's PushFront ordering works. Without the sort the
	// list order is whatever os.ReadDir yielded, which is
	// implementation-defined and makes post-restart eviction
	// non-deterministic (documented contract violation).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].storedAt.Before(entries[j].storedAt)
	})
	now := c.clock()
	for _, e := range entries {
		if !e.expiresAt.IsZero() && now.After(e.expiresAt) {
			_ = os.Remove(c.bodyPath(e.pieceID))
			_ = os.Remove(c.metaPath(e.pieceID))
			continue
		}
		e.lastAccess = e.storedAt
		var el *list.Element
		if e.pinned && c.policy.Kind == EvictionLRUHotPin {
			el = c.hot.PushFront(e)
			c.hotBytes += e.sizeBytes
		} else {
			e.pinned = false
			el = c.main.PushFront(e)
		}
		c.index[e.pieceID] = el
		c.bytesUsed += e.sizeBytes
	}
	c.stats.Entries = int64(len(c.index))
	c.stats.BytesUsed = c.bytesUsed
	// If the warm set exceeds MaxBytes (e.g. operator shrunk the
	// policy between restarts) evict until we fit.
	if c.policy.MaxBytes > 0 {
		c.evictMainLocked(0)
	}
	return nil
}

func (c *DiskCache) evictMainLocked(incoming int64) {
	if c.policy.MaxBytes <= 0 {
		return
	}
	for c.bytesUsed+incoming > c.policy.MaxBytes {
		back := c.main.Back()
		if back == nil {
			back = c.hot.Back()
			if back == nil {
				return
			}
		}
		c.removeLocked(back)
	}
}

func (c *DiskCache) evictHotLocked(incoming int64) {
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

func (c *DiskCache) removeLocked(el *list.Element) {
	entry := el.Value.(*diskEntry)
	if entry.pinned {
		c.hot.Remove(el)
		c.hotBytes -= entry.sizeBytes
	} else {
		c.main.Remove(el)
	}
	delete(c.index, entry.pieceID)
	c.bytesUsed -= entry.sizeBytes
	c.stats.Entries = int64(len(c.index))
	c.stats.BytesUsed = c.bytesUsed
	c.stats.Evictions++
	_ = os.Remove(c.bodyPath(entry.pieceID))
	_ = os.Remove(c.metaPath(entry.pieceID))
}

// shardDir returns {root}/{first two bytes of pieceID}.
func (c *DiskCache) shardDir(pieceID string) string {
	shard := shardOf(pieceID)
	return filepath.Join(c.root, shard)
}

func (c *DiskCache) bodyPath(pieceID string) string {
	return filepath.Join(c.shardDir(pieceID), pieceID+".bin")
}

func (c *DiskCache) metaPath(pieceID string) string {
	return filepath.Join(c.shardDir(pieceID), pieceID+".meta.json")
}

func shardOf(pieceID string) string {
	if len(pieceID) < 2 {
		return "_"
	}
	return pieceID[:2]
}

func writeMeta(path string, e *diskEntry) error {
	body, err := json.Marshal(metaFile{
		PieceID:   e.pieceID,
		SizeBytes: e.sizeBytes,
		Hash:      e.hash,
		StoredAt:  e.storedAt,
		ExpiresAt: e.expiresAt,
		Pinned:    e.pinned,
		HitCount:  e.hits,
	})
	if err != nil {
		return fmt.Errorf("hot_object_cache: marshal meta: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("hot_object_cache: write meta: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("hot_object_cache: publish meta: %w", err)
	}
	return nil
}

// readShards walks root/*/ and returns every valid (body + meta)
// pair it finds. Orphan bodies (no sidecar) and orphan sidecars (no
// body) are cleaned up.
func readShards(root string) ([]*diskEntry, error) {
	shards, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("hot_object_cache: read root %q: %w", root, err)
	}
	var out []*diskEntry
	for _, sd := range shards {
		if !sd.IsDir() {
			continue
		}
		shardPath := filepath.Join(root, sd.Name())
		files, err := os.ReadDir(shardPath)
		if err != nil {
			return nil, fmt.Errorf("hot_object_cache: read shard %q: %w", shardPath, err)
		}
		// Build a set of body piece IDs to match against sidecars.
		bodies := map[string]struct{}{}
		metas := map[string]string{}
		for _, f := range files {
			name := f.Name()
			switch {
			case filepath.Ext(name) == ".bin":
				bodies[name[:len(name)-len(".bin")]] = struct{}{}
			case hasSuffix(name, ".meta.json"):
				metas[name[:len(name)-len(".meta.json")]] = filepath.Join(shardPath, name)
			case filepath.Ext(name) == ".tmp":
				// Partial write from a crash; remove it.
				_ = os.Remove(filepath.Join(shardPath, name))
			}
		}
		for pieceID, metaPath := range metas {
			if _, ok := bodies[pieceID]; !ok {
				_ = os.Remove(metaPath)
				continue
			}
			entry, err := loadMeta(metaPath)
			if err != nil {
				// Corrupt sidecar; drop both files.
				_ = os.Remove(metaPath)
				_ = os.Remove(filepath.Join(shardPath, pieceID+".bin"))
				continue
			}
			out = append(out, entry)
			delete(bodies, pieceID)
		}
		for orphan := range bodies {
			_ = os.Remove(filepath.Join(shardPath, orphan+".bin"))
		}
	}
	return out, nil
}

func loadMeta(path string) (*diskEntry, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m metaFile
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return &diskEntry{
		pieceID:   m.PieceID,
		sizeBytes: m.SizeBytes,
		hash:      m.Hash,
		storedAt:  m.StoredAt,
		expiresAt: m.ExpiresAt,
		pinned:    m.Pinned,
		hits:      m.HitCount,
	}, nil
}

func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}

var _ HotObjectCache = (*DiskCache)(nil)
