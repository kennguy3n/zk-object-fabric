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
	return &cp
}

var _ manifest_store.ManifestStore = (*Store)(nil)
