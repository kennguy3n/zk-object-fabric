// In-memory implementation of the content_index Store. It is safe
// for concurrent use and is intended for dev / tests only — restarts
// drop every entry.
package content_index

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is a goroutine-safe in-memory Store. Tests and the
// local_fs_dev profile use it as a substitute for the Postgres
// implementation when no metadata DSN is configured.
type MemoryStore struct {
	mu      sync.Mutex
	entries map[memoryKey]ContentIndexEntry
	now     func() time.Time
}

type memoryKey struct {
	TenantID    string
	ContentHash string
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		entries: map[memoryKey]ContentIndexEntry{},
		now:     time.Now,
	}
}

// Lookup returns the entry for (tenantID, contentHash) or ErrNotFound.
func (s *MemoryStore) Lookup(_ context.Context, tenantID, contentHash string) (*ContentIndexEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[memoryKey{tenantID, contentHash}]
	if !ok {
		return nil, ErrNotFound
	}
	cp := e
	return &cp, nil
}

// Register inserts a new entry with RefCount = 1. Returns an
// error if a row already exists for the (tenantID, contentHash)
// key — the caller should retry via IncrementRef.
func (s *MemoryStore) Register(_ context.Context, entry ContentIndexEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memoryKey{entry.TenantID, entry.ContentHash}
	if _, exists := s.entries[k]; exists {
		return ErrAlreadyExists
	}
	if entry.RefCount <= 0 {
		entry.RefCount = 1
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = s.now()
	}
	s.entries[k] = entry
	return nil
}

// IncrementRef atomically bumps RefCount on an existing row.
func (s *MemoryStore) IncrementRef(_ context.Context, tenantID, contentHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memoryKey{tenantID, contentHash}
	e, ok := s.entries[k]
	if !ok {
		return ErrNotFound
	}
	e.RefCount++
	s.entries[k] = e
	return nil
}

// DecrementRef atomically decrements RefCount and returns the new
// count. Callers must call provider.DeletePiece and then Delete
// when the returned count is 0.
func (s *MemoryStore) DecrementRef(_ context.Context, tenantID, contentHash string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memoryKey{tenantID, contentHash}
	e, ok := s.entries[k]
	if !ok {
		return 0, ErrNotFound
	}
	if e.RefCount <= 0 {
		return 0, ErrInvalidRefCount
	}
	e.RefCount--
	s.entries[k] = e
	return e.RefCount, nil
}

// Delete removes the row only when RefCount is zero. Returns
// ErrRefCountNonZero when a concurrent IncrementRef raced with the
// caller's DecrementRef.
func (s *MemoryStore) Delete(_ context.Context, tenantID, contentHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memoryKey{tenantID, contentHash}
	e, ok := s.entries[k]
	if !ok {
		return ErrNotFound
	}
	if e.RefCount > 0 {
		return ErrRefCountNonZero
	}
	delete(s.entries, k)
	return nil
}

// ScanAll returns every entry for the given tenant. The slice
// order is not stable — callers must not rely on it.
func (s *MemoryStore) ScanAll(_ context.Context, tenantID string) ([]ContentIndexEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ContentIndexEntry, 0)
	for k, e := range s.entries {
		if k.TenantID != tenantID {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// ListTenants returns the distinct tenant IDs that have at least
// one entry in the store.
func (s *MemoryStore) ListTenants(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{})
	for k := range s.entries {
		seen[k.TenantID] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	return out, nil
}

var _ Store = (*MemoryStore)(nil)
