package console

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kennguy3n/zk-object-fabric/internal/cellops"
)

// PostgresDedicatedCellStore is the Phase 3 Postgres-backed
// DedicatedCellStore. It satisfies both DedicatedCellStore (the
// SPA-facing read interface used by the console handler) and
// cellops.CellSink (the operator-facing write interface used by
// the cell provisioner).
//
// Schema: see api/console/schema.sql.
type PostgresDedicatedCellStore struct {
	db  *sql.DB
	ctx context.Context
}

// NewPostgresDedicatedCellStore wraps db. Callers must register a
// Postgres driver (lib/pq or jackc/pgx/v5/stdlib) before opening
// the *sql.DB and run schema.sql before issuing the first query.
func NewPostgresDedicatedCellStore(db *sql.DB) (*PostgresDedicatedCellStore, error) {
	if db == nil {
		return nil, errors.New("console: postgres dedicated cell store requires a non-nil *sql.DB")
	}
	return &PostgresDedicatedCellStore{db: db}, nil
}

func (s *PostgresDedicatedCellStore) cx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// ListDedicatedCells implements DedicatedCellStore.
func (s *PostgresDedicatedCellStore) ListDedicatedCells(ctx context.Context, tenantID string) ([]DedicatedCellDescriptor, error) {
	const q = `
		SELECT cell_id, region, country, status, capacity_petabytes, utilization
		FROM dedicated_cells
		WHERE tenant_id = $1
		ORDER BY cell_id`
	rows, err := s.db.QueryContext(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("console: query dedicated cells: %w", err)
	}
	defer rows.Close()
	out := make([]DedicatedCellDescriptor, 0)
	for rows.Next() {
		var d DedicatedCellDescriptor
		if err := rows.Scan(&d.ID, &d.Region, &d.Country, &d.Status, &d.CapacityPetabytes, &d.Utilization); err != nil {
			return nil, fmt.Errorf("console: scan dedicated cell: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("console: iterate dedicated cells: %w", err)
	}
	return out, nil
}

// UpsertDedicatedCell implements cellops.CellSink. New cells are
// inserted; existing cells (matched by cell_id) update every
// mutable column. created_at is preserved across upserts.
func (s *PostgresDedicatedCellStore) UpsertDedicatedCell(ctx context.Context, status cellops.CellStatus) error {
	if status.CellID == "" {
		return errors.New("console: cell_id is required")
	}
	const q = `
		INSERT INTO dedicated_cells (
			cell_id, tenant_id, region, country, status,
			capacity_petabytes, utilization, erasure_profile,
			node_count, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (cell_id) DO UPDATE
		SET tenant_id          = EXCLUDED.tenant_id,
		    region             = EXCLUDED.region,
		    country            = EXCLUDED.country,
		    status             = EXCLUDED.status,
		    capacity_petabytes = EXCLUDED.capacity_petabytes,
		    utilization        = EXCLUDED.utilization,
		    erasure_profile    = EXCLUDED.erasure_profile,
		    node_count         = EXCLUDED.node_count,
		    updated_at         = EXCLUDED.updated_at`
	now := time.Now()
	created := status.CreatedAt
	if created.IsZero() {
		created = now
	}
	updated := status.UpdatedAt
	if updated.IsZero() {
		updated = now
	}
	_, err := s.db.ExecContext(ctx, q,
		status.CellID, status.TenantID, status.Region, status.Country, string(status.Status),
		status.CapacityPetabytes, status.Utilization, status.ErasureProfile,
		status.NodeCount, created, updated,
	)
	if err != nil {
		return fmt.Errorf("console: upsert dedicated cell: %w", err)
	}
	return nil
}

// GetDedicatedCell implements cellops.CellSink.
func (s *PostgresDedicatedCellStore) GetDedicatedCell(ctx context.Context, cellID string) (cellops.CellStatus, bool, error) {
	const q = `
		SELECT cell_id, tenant_id, region, country, status,
		       capacity_petabytes, utilization, erasure_profile,
		       node_count, created_at, updated_at
		FROM dedicated_cells WHERE cell_id = $1`
	var (
		c          cellops.CellStatus
		statusText string
	)
	err := s.db.QueryRowContext(ctx, q, cellID).Scan(
		&c.CellID, &c.TenantID, &c.Region, &c.Country, &statusText,
		&c.CapacityPetabytes, &c.Utilization, &c.ErasureProfile,
		&c.NodeCount, &c.CreatedAt, &c.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return cellops.CellStatus{}, false, nil
	}
	if err != nil {
		return cellops.CellStatus{}, false, fmt.Errorf("console: select dedicated cell: %w", err)
	}
	c.Status = cellops.ProvisionStatus(statusText)
	return c, true, nil
}

// ListAllCells implements cellops.CellLister.
func (s *PostgresDedicatedCellStore) ListAllCells(ctx context.Context) ([]cellops.CellStatus, error) {
	return s.queryCells(ctx, `SELECT cell_id, tenant_id, region, country, status,
		capacity_petabytes, utilization, erasure_profile, node_count,
		created_at, updated_at FROM dedicated_cells ORDER BY cell_id`)
}

// ListCellsByTenant implements cellops.CellLister.
func (s *PostgresDedicatedCellStore) ListCellsByTenant(ctx context.Context, tenantID string) ([]cellops.CellStatus, error) {
	return s.queryCells(ctx, `SELECT cell_id, tenant_id, region, country, status,
		capacity_petabytes, utilization, erasure_profile, node_count,
		created_at, updated_at FROM dedicated_cells WHERE tenant_id = $1
		ORDER BY cell_id`, tenantID)
}

func (s *PostgresDedicatedCellStore) queryCells(ctx context.Context, q string, args ...interface{}) ([]cellops.CellStatus, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("console: query cells: %w", err)
	}
	defer rows.Close()
	out := make([]cellops.CellStatus, 0)
	for rows.Next() {
		var c cellops.CellStatus
		var statusText string
		if err := rows.Scan(
			&c.CellID, &c.TenantID, &c.Region, &c.Country, &statusText,
			&c.CapacityPetabytes, &c.Utilization, &c.ErasureProfile,
			&c.NodeCount, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("console: scan cell: %w", err)
		}
		c.Status = cellops.ProvisionStatus(statusText)
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCellStatus implements cellops.CellSink.
func (s *PostgresDedicatedCellStore) UpdateCellStatus(ctx context.Context, cellID string, status cellops.ProvisionStatus) error {
	if cellID == "" {
		return errors.New("console: cell_id is required")
	}
	const q = `UPDATE dedicated_cells SET status = $2, updated_at = NOW() WHERE cell_id = $1`
	res, err := s.db.ExecContext(ctx, q, cellID, string(status))
	if err != nil {
		return fmt.Errorf("console: update dedicated cell status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("console: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("console: cell %q not found", cellID)
	}
	return nil
}
