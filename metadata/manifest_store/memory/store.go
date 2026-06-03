// Package memory is an in-memory ManifestStore used by tests and the
// local_fs_dev deployment profile. It is not durable and is NOT safe
// for production use.
package memory

import (
	"context"
	"sort"
	"strconv"
	"sync"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
)

// Store is a goroutine-safe in-memory manifest_store.ManifestStore.
type Store struct {
	mu     sync.RWMutex
	byKey  map[manifest_store.ManifestKey]*metadata.ObjectManifest
	latest map[latestKey]manifest_store.ManifestKey
	order  uint64
	seq    map[manifest_store.ManifestKey]uint64
}

type latestKey struct {
	TenantID      string
	Bucket        string
	ObjectKeyHash string
}

// New returns an empty in-memory store.
func New() *Store {
	return &Store{
		byKey:  map[manifest_store.ManifestKey]*metadata.ObjectManifest{},
		latest: map[latestKey]manifest_store.ManifestKey{},
		seq:    map[manifest_store.ManifestKey]uint64{},
	}
}

// Put stores a manifest. It replaces any existing manifest with the
// same key.
func (s *Store) Put(_ context.Context, key manifest_store.ManifestKey, m *metadata.ObjectManifest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order++
	s.byKey[key] = cloneManifest(m)
	s.seq[key] = s.order
	s.latest[latestKey{key.TenantID, key.Bucket, key.ObjectKeyHash}] = key
	return nil
}

// Get returns the manifest at key. If VersionID is empty the most
// recently written version for the (tenant, bucket, object_key_hash)
// triple is returned.
func (s *Store) Get(_ context.Context, key manifest_store.ManifestKey) (*metadata.ObjectManifest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if key.VersionID == "" {
		lk := latestKey{key.TenantID, key.Bucket, key.ObjectKeyHash}
		target, ok := s.latest[lk]
		if !ok {
			return nil, manifest_store.ErrNotFound
		}
		key = target
	}
	m, ok := s.byKey[key]
	if !ok {
		return nil, manifest_store.ErrNotFound
	}
	return cloneManifest(m), nil
}

// Delete removes a manifest. Empty VersionID deletes the latest.
func (s *Store) Delete(_ context.Context, key manifest_store.ManifestKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lk := latestKey{key.TenantID, key.Bucket, key.ObjectKeyHash}
	if key.VersionID == "" {
		target, ok := s.latest[lk]
		if !ok {
			return manifest_store.ErrNotFound
		}
		key = target
	}
	if _, ok := s.byKey[key]; !ok {
		return manifest_store.ErrNotFound
	}
	delete(s.byKey, key)
	delete(s.seq, key)
	if s.latest[lk] == key {
		delete(s.latest, lk)
	}
	return nil
}

// List iterates manifests under (tenantID, bucket). Cursor is the
// decimal-encoded insertion sequence of the last returned manifest.
//
// Only the latest version of each (tenant, bucket, object_key_hash)
// triple is returned — older versions created by overwrite PUTs stay
// addressable by explicit VersionID via Get, but do not appear in
// LIST, matching S3 ListObjectsV2 semantics.
func (s *Store) List(_ context.Context, tenantID, bucket, cursor string, limit int) (manifest_store.ListResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 {
		limit = 1000
	}
	var after uint64
	if cursor != "" {
		n, err := strconv.ParseUint(cursor, 10, 64)
		if err != nil {
			return manifest_store.ListResult{}, err
		}
		after = n
	}
	type row struct {
		seq      uint64
		key      manifest_store.ManifestKey
		manifest *metadata.ObjectManifest
	}
	var rows []row
	for lk, mk := range s.latest {
		if lk.TenantID != tenantID || lk.Bucket != bucket {
			continue
		}
		seq := s.seq[mk]
		if seq <= after {
			continue
		}
		m, ok := s.byKey[mk]
		if !ok {
			continue
		}
		rows = append(rows, row{seq: seq, key: mk, manifest: m})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].seq < rows[j].seq })

	out := manifest_store.ListResult{}
	if len(rows) > limit {
		rows = rows[:limit]
		out.NextCursor = strconv.FormatUint(rows[len(rows)-1].seq, 10)
	}
	for _, r := range rows {
		out.Manifests = append(out.Manifests, cloneManifest(r.manifest))
	}
	return out, nil
}

// HasManifestWithPieceID reports whether the tenant has at least
// one manifest referencing pieceID. Used by the orphan GC worker.
func (s *Store) HasManifestWithPieceID(_ context.Context, tenantID, pieceID string) (bool, error) {
	if tenantID == "" || pieceID == "" {
		return false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, m := range s.byKey {
		if k.TenantID != tenantID {
			continue
		}
		for _, p := range m.Pieces {
			if p.PieceID == pieceID {
				return true, nil
			}
		}
	}
	return false, nil
}

// ListVersions returns every version of (tenantID, bucket,
// objectKeyHash) most-recent (highest insertion sequence) first.
func (s *Store) ListVersions(_ context.Context, tenantID, bucket, objectKeyHash string) ([]*metadata.ObjectManifest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type row struct {
		seq      uint64
		manifest *metadata.ObjectManifest
	}
	var rows []row
	for k, m := range s.byKey {
		if k.TenantID != tenantID || k.Bucket != bucket || k.ObjectKeyHash != objectKeyHash {
			continue
		}
		rows = append(rows, row{seq: s.seq[k], manifest: m})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].seq > rows[j].seq })
	out := make([]*metadata.ObjectManifest, 0, len(rows))
	for _, r := range rows {
		out = append(out, cloneManifest(r.manifest))
	}
	return out, nil
}

// ScanManifests paginates over every stored manifest version across
// all tenants and buckets, ordered by the full primary key
// (tenant_id, bucket, object_key_hash, version_id). The cursor is the
// encoded key of the last returned entry; pass "" to start.
func (s *Store) ScanManifests(_ context.Context, cursor string, limit int) (manifest_store.ScanResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 {
		limit = 1000
	}
	after, err := manifest_store.DecodeScanCursor(cursor)
	if err != nil {
		return manifest_store.ScanResult{}, err
	}

	keys := make([]manifest_store.ManifestKey, 0, len(s.byKey))
	for k := range s.byKey {
		if cursor != "" && !scanKeyLess(after, k) {
			// Skip keys at or before the cursor.
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return scanKeyLess(keys[i], keys[j]) })

	out := manifest_store.ScanResult{}
	if len(keys) > limit {
		last := keys[limit-1]
		out.NextCursor = manifest_store.EncodeScanCursor(last)
		keys = keys[:limit]
	}
	for _, k := range keys {
		out.Manifests = append(out.Manifests, manifest_store.ScannedManifest{
			Key:      k,
			Manifest: cloneManifest(s.byKey[k]),
		})
	}
	return out, nil
}

// scanKeyLess orders ManifestKeys by the full primary key tuple,
// matching the Postgres store's ORDER BY so the two implementations
// page identically.
func scanKeyLess(a, b manifest_store.ManifestKey) bool {
	if a.TenantID != b.TenantID {
		return a.TenantID < b.TenantID
	}
	if a.Bucket != b.Bucket {
		return a.Bucket < b.Bucket
	}
	if a.ObjectKeyHash != b.ObjectKeyHash {
		return a.ObjectKeyHash < b.ObjectKeyHash
	}
	return a.VersionID < b.VersionID
}

func cloneManifest(m *metadata.ObjectManifest) *metadata.ObjectManifest {
	if m == nil {
		return nil
	}
	cp := *m
	if m.Pieces != nil {
		cp.Pieces = append([]metadata.Piece(nil), m.Pieces...)
	}
	if m.PlacementPolicy.Residency != nil {
		cp.PlacementPolicy.Residency = append([]string(nil), m.PlacementPolicy.Residency...)
	}
	if m.PlacementPolicy.AllowedBackends != nil {
		cp.PlacementPolicy.AllowedBackends = append([]string(nil), m.PlacementPolicy.AllowedBackends...)
	}
	// Deep-clone the pointer-typed policy fields nested under
	// PlacementPolicy. The shallow struct copy above (`cp := *m`)
	// duplicates the PlacementPolicy struct value, but pointer
	// members of that struct still alias the source's pointees —
	// so a caller that mutates the policy after a Put would see
	// the mutation reflected in the stored copy (and vice versa,
	// since Get returns a clone). The memory store is the
	// reference contract for the ManifestStore interface, so it
	// has to honour the "stored manifests are immutable once
	// Put-ed" invariant byte-for-byte; the Postgres store honours
	// it via SQL round-trip serialisation. Both pointer fields
	// are documented as nilable in metadata/manifest.go (DedupPolicy
	// nil = no dedup, ReplicationPolicy nil = no replication), so
	// a nil check is the correct branch for the zero value.
	if m.PlacementPolicy.ReplicationPolicy != nil {
		rp := *m.PlacementPolicy.ReplicationPolicy
		cp.PlacementPolicy.ReplicationPolicy = &rp
	}
	if m.PlacementPolicy.DedupPolicy != nil {
		dp := *m.PlacementPolicy.DedupPolicy
		cp.PlacementPolicy.DedupPolicy = &dp
	}
	// Deep-clone the EncryptionConfig.WrappedDEK byte slice. The
	// shallow struct copy above duplicates the EncryptionConfig
	// struct value, but []byte is a slice header and `cp.Encryption.
	// WrappedDEK` still aliases the source's backing array — so a
	// caller that mutates DEK bytes after a Put would corrupt the
	// stored copy. In practice WrappedDEK is freshly allocated on
	// each PUT path (via KMS/Vault wrap) and unlikely to be mutated,
	// but the "stored manifests are immutable once Put-ed"
	// invariant the doc above commits to must hold byte-for-byte;
	// the cost of an extra alloc per Put/Get on the in-memory store
	// is negligible compared to the cost of a latent aliasing bug
	// in a security-sensitive code path. The Postgres store
	// honours the invariant via SQL round-trip serialisation.
	if m.Encryption.WrappedDEK != nil {
		cp.Encryption.WrappedDEK = append([]byte(nil), m.Encryption.WrappedDEK...)
	}
	return &cp
}

var _ manifest_store.ManifestStore = (*Store)(nil)
