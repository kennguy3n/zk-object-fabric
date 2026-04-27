// Cell registry. Reads the dedicated_cells table and surfaces
// the active cells to the gateway startup wiring so each one is
// registered as a ceph_rgw provider in the provider registry.
package cellops

import (
	"context"
	"errors"
)

// CellLister is the read surface of the dedicated-cells store
// the registry consumes. PostgresDedicatedCellStore and
// MemoryDedicatedCellStore both satisfy it.
//
// The interface is intentionally narrower than CellSink (the
// write surface used by the provisioner) so a read-only consumer
// like the startup wiring does not need a sink implementation.
type CellLister interface {
	ListAllCells(ctx context.Context) ([]CellStatus, error)
	ListCellsByTenant(ctx context.Context, tenantID string) ([]CellStatus, error)
	GetDedicatedCell(ctx context.Context, cellID string) (CellStatus, bool, error)
}

// CellRegistry exposes the canonical cell catalogue. It is a
// thin facade over a CellLister; the abstraction exists so the
// rest of the gateway depends on a stable type rather than the
// underlying SQL store.
type CellRegistry struct {
	store CellLister
}

// NewCellRegistry binds reg to store.
func NewCellRegistry(store CellLister) *CellRegistry {
	return &CellRegistry{store: store}
}

// ListActiveCells returns every cell whose status is "active".
// Pending and decommissioning cells are intentionally excluded;
// the gateway hot path can only route to fully provisioned
// cells.
func (r *CellRegistry) ListActiveCells(ctx context.Context) ([]CellStatus, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("cellops: nil cell registry")
	}
	all, err := r.store.ListAllCells(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CellStatus, 0, len(all))
	for _, c := range all {
		if c.Status == StatusActive {
			out = append(out, c)
		}
	}
	return out, nil
}

// GetCellsByTenant returns every cell bound to tenantID,
// regardless of status.
func (r *CellRegistry) GetCellsByTenant(ctx context.Context, tenantID string) ([]CellStatus, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("cellops: nil cell registry")
	}
	return r.store.ListCellsByTenant(ctx, tenantID)
}

// GetCellByID returns the cell with the given ID. The boolean
// is false (with a nil error) when the cell does not exist.
func (r *CellRegistry) GetCellByID(ctx context.Context, cellID string) (CellStatus, bool, error) {
	if r == nil || r.store == nil {
		return CellStatus{}, false, errors.New("cellops: nil cell registry")
	}
	return r.store.GetDedicatedCell(ctx, cellID)
}
