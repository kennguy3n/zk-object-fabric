package console

import (
	"context"
	"sync"

	"github.com/kennguy3n/zk-object-fabric/metadata/placement_policy"
)

// MemoryPlacementStore is a process-local PlacementStore suitable for
// the Phase 3 console scaffold and tests. The real control plane
// will replace this with a Postgres-backed store behind the same
// PlacementStore interface; the handler does not care which
// implementation is wired in.
type MemoryPlacementStore struct {
	mu       sync.RWMutex
	policies map[string]placement_policy.Policy
}

// NewMemoryPlacementStore returns an empty store.
func NewMemoryPlacementStore() *MemoryPlacementStore {
	return &MemoryPlacementStore{policies: map[string]placement_policy.Policy{}}
}

// GetPlacement implements PlacementStore.
func (s *MemoryPlacementStore) GetPlacement(ctx context.Context, tenantID string) (placement_policy.Policy, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.policies[tenantID]
	return p, ok, nil
}

// PutPlacement implements PlacementStore.
func (s *MemoryPlacementStore) PutPlacement(ctx context.Context, tenantID string, policy placement_policy.Policy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies[tenantID] = policy
	return nil
}
