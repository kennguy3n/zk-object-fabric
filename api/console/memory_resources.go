package console

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kennguy3n/zk-object-fabric/internal/cellops"
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
//
// The store also satisfies cellops.CellSink so a ManualProvisioner
// wired in cmd/gateway/main.go can use it as the persistence
// backend in dev / test mode without dragging in Postgres.
type MemoryDedicatedCellStore struct {
	mu    sync.RWMutex
	cells map[string][]DedicatedCellDescriptor // tenantID → cells
	// records is the operator-facing view keyed by cell ID. It
	// holds the richer cellops.CellStatus payload (NodeCount,
	// ErasureProfile, timestamps) that the SPA descriptor does
	// not surface.
	records map[string]cellops.CellStatus
}

// NewMemoryDedicatedCellStore returns an empty store.
func NewMemoryDedicatedCellStore() *MemoryDedicatedCellStore {
	return &MemoryDedicatedCellStore{
		cells:   map[string][]DedicatedCellDescriptor{},
		records: map[string]cellops.CellStatus{},
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

// UpsertDedicatedCell implements cellops.CellSink. The operator
// view is recorded by cell ID; the SPA descriptor is rebuilt for
// the tenant's cell slice so ListDedicatedCells reflects the new
// cell on the next read.
func (s *MemoryDedicatedCellStore) UpsertDedicatedCell(ctx context.Context, status cellops.CellStatus) error {
	if status.CellID == "" {
		return fmt.Errorf("console: cell_id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, existed := s.records[status.CellID]
	s.records[status.CellID] = status
	if !existed || prev.TenantID != status.TenantID {
		// The cell either is new or moved to a different
		// tenant; rebuild the per-tenant slice from records.
		s.rebuildTenantSliceLocked(status.TenantID)
		if existed && prev.TenantID != status.TenantID {
			s.rebuildTenantSliceLocked(prev.TenantID)
		}
		return nil
	}
	s.rebuildTenantSliceLocked(status.TenantID)
	return nil
}

// GetDedicatedCell implements cellops.CellSink.
func (s *MemoryDedicatedCellStore) GetDedicatedCell(ctx context.Context, cellID string) (cellops.CellStatus, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.records[cellID]
	return c, ok, nil
}

// ListAllCells implements cellops.CellLister.
func (s *MemoryDedicatedCellStore) ListAllCells(ctx context.Context) ([]cellops.CellStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]cellops.CellStatus, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, rec)
	}
	return out, nil
}

// ListCellsByTenant implements cellops.CellLister.
func (s *MemoryDedicatedCellStore) ListCellsByTenant(ctx context.Context, tenantID string) ([]cellops.CellStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]cellops.CellStatus, 0)
	for _, rec := range s.records {
		if rec.TenantID == tenantID {
			out = append(out, rec)
		}
	}
	return out, nil
}

// UpdateCellStatus implements cellops.CellSink.
func (s *MemoryDedicatedCellStore) UpdateCellStatus(ctx context.Context, cellID string, status cellops.ProvisionStatus) error {
	if cellID == "" {
		return fmt.Errorf("console: cell_id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.records[cellID]
	if !ok {
		return fmt.Errorf("console: cell %q not found", cellID)
	}
	c.Status = status
	c.UpdatedAt = time.Now()
	s.records[cellID] = c
	s.rebuildTenantSliceLocked(c.TenantID)
	return nil
}

// rebuildTenantSliceLocked recomputes s.cells[tenantID] from the
// authoritative records map. The caller must hold s.mu for write.
func (s *MemoryDedicatedCellStore) rebuildTenantSliceLocked(tenantID string) {
	if tenantID == "" {
		return
	}
	out := make([]DedicatedCellDescriptor, 0)
	for _, rec := range s.records {
		if rec.TenantID != tenantID {
			continue
		}
		out = append(out, dedicatedCellDescriptorFrom(rec))
	}
	if len(out) == 0 {
		delete(s.cells, tenantID)
		return
	}
	s.cells[tenantID] = out
}

// dedicatedCellDescriptorFrom projects the operator-facing
// cellops.CellStatus down to the SPA-facing DedicatedCellDescriptor
// shape. The operator-only fields (NodeCount, ErasureProfile,
// timestamps) are dropped on purpose — the SPA does not render
// them.
func dedicatedCellDescriptorFrom(s cellops.CellStatus) DedicatedCellDescriptor {
	return DedicatedCellDescriptor{
		ID:                s.CellID,
		Region:            s.Region,
		Country:           s.Country,
		Status:            string(s.Status),
		CapacityPetabytes: s.CapacityPetabytes,
		Utilization:       s.Utilization,
	}
}
