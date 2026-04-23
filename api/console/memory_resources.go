package console

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemoryBucketStore is a process-local BucketStore used by dev and
// test deployments that do not run the Phase-3 Postgres bucket
// registry yet. The manifest store is the authoritative source of
// truth for object placement; this registry only tracks bucket
// metadata (name → placement policy ref) so the console can render
// the "Buckets" tab without scanning every manifest.
type MemoryBucketStore struct {
	mu      sync.RWMutex
	buckets map[string]map[string]BucketDescriptor // tenantID → name → descriptor
	now     func() time.Time
}

// NewMemoryBucketStore returns an empty MemoryBucketStore.
func NewMemoryBucketStore() *MemoryBucketStore {
	return &MemoryBucketStore{
		buckets: map[string]map[string]BucketDescriptor{},
		now:     time.Now,
	}
}

// ListBuckets implements BucketStore.
func (s *MemoryBucketStore) ListBuckets(ctx context.Context, tenantID string) ([]BucketDescriptor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]BucketDescriptor, 0)
	for _, b := range s.buckets[tenantID] {
		out = append(out, b)
	}
	return out, nil
}

// CreateBucket implements BucketStore. Returns an error when a
// bucket with the same name already exists for the tenant; callers
// surface that as an HTTP 409 once we wire the manifest store.
func (s *MemoryBucketStore) CreateBucket(ctx context.Context, tenantID, name, placementPolicyRef string) (BucketDescriptor, error) {
	if tenantID == "" {
		return BucketDescriptor{}, fmt.Errorf("console: tenant_id is required")
	}
	if name == "" {
		return BucketDescriptor{}, fmt.Errorf("console: bucket name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bucketsForTenant, ok := s.buckets[tenantID]
	if !ok {
		bucketsForTenant = map[string]BucketDescriptor{}
		s.buckets[tenantID] = bucketsForTenant
	}
	if _, exists := bucketsForTenant[name]; exists {
		return BucketDescriptor{}, fmt.Errorf("console: bucket %q already exists", name)
	}
	desc := BucketDescriptor{
		Name:               name,
		CreatedAt:          s.now(),
		PlacementPolicyRef: placementPolicyRef,
	}
	bucketsForTenant[name] = desc
	return desc, nil
}

// DeleteBucket implements BucketStore. Idempotent: a missing bucket
// is not an error.
func (s *MemoryBucketStore) DeleteBucket(ctx context.Context, tenantID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bucketsForTenant, ok := s.buckets[tenantID]; ok {
		delete(bucketsForTenant, name)
	}
	return nil
}

// MemoryDedicatedCellStore is the dev / test fallback for the
// dedicated-cells listing in the tenant console. It returns an empty
// slice by default so B2C self-service tenants see an empty cells
// table; operators wire a real registry (e.g. the cell controller)
// via DedicatedCellStore in production.
type MemoryDedicatedCellStore struct {
	mu    sync.RWMutex
	cells map[string][]DedicatedCellDescriptor // tenantID → cells
}

// NewMemoryDedicatedCellStore returns an empty store.
func NewMemoryDedicatedCellStore() *MemoryDedicatedCellStore {
	return &MemoryDedicatedCellStore{
		cells: map[string][]DedicatedCellDescriptor{},
	}
}

// SeedCells registers a static list of cells for tenantID. Useful
// for tests that want a non-empty dedicated-cells response without
// wiring the full cell-controller.
func (s *MemoryDedicatedCellStore) SeedCells(tenantID string, cells []DedicatedCellDescriptor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copyCells := make([]DedicatedCellDescriptor, len(cells))
	copy(copyCells, cells)
	s.cells[tenantID] = copyCells
}

// ListDedicatedCells implements DedicatedCellStore.
func (s *MemoryDedicatedCellStore) ListDedicatedCells(ctx context.Context, tenantID string) ([]DedicatedCellDescriptor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cells := s.cells[tenantID]
	out := make([]DedicatedCellDescriptor, len(cells))
	copy(out, cells)
	return out, nil
}
