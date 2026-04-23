package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kennguy3n/zk-object-fabric/metadata/placement_policy"
)

// PostgresPlacementStore is the Phase 3 Postgres-backed
// PlacementStore. Each tenant has at most one active placement
// policy row keyed by tenant_id; the full policy body is stored as
// JSON so the schema can evolve without per-field migrations.
//
// Schema: see api/console/schema.sql.
type PostgresPlacementStore struct {
	db *sql.DB
}

// NewPostgresPlacementStore wraps db. The caller is responsible for
// opening the *sql.DB and for running the migration in schema.sql
// before issuing the first query.
func NewPostgresPlacementStore(db *sql.DB) (*PostgresPlacementStore, error) {
	if db == nil {
		return nil, errors.New("console: postgres placement store requires a non-nil *sql.DB")
	}
	return &PostgresPlacementStore{db: db}, nil
}

// GetPlacement implements PlacementStore. A missing row is reported
// as (zero, false, nil) so the handler can render an empty shell
// for the frontend's policy editor to author from scratch.
func (s *PostgresPlacementStore) GetPlacement(ctx context.Context, tenantID string) (placement_policy.Policy, bool, error) {
	if tenantID == "" {
		return placement_policy.Policy{}, false, errors.New("console: tenant_id is required")
	}
	const q = `SELECT policy_json FROM placement_policies WHERE tenant_id = $1`
	var body []byte
	err := s.db.QueryRowContext(ctx, q, tenantID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return placement_policy.Policy{}, false, nil
	}
	if err != nil {
		return placement_policy.Policy{}, false, fmt.Errorf("console: select placement %q: %w", tenantID, err)
	}
	var pol placement_policy.Policy
	if err := json.Unmarshal(body, &pol); err != nil {
		return placement_policy.Policy{}, false, fmt.Errorf("console: decode placement %q: %w", tenantID, err)
	}
	return pol, true, nil
}

// PutPlacement implements PlacementStore. It UPSERTs the row keyed
// by tenant_id so repeat saves from the editor round-trip without
// an explicit delete.
func (s *PostgresPlacementStore) PutPlacement(ctx context.Context, tenantID string, policy placement_policy.Policy) error {
	if tenantID == "" {
		return errors.New("console: tenant_id is required")
	}
	body, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("console: marshal placement %q: %w", tenantID, err)
	}
	const q = `
		INSERT INTO placement_policies (tenant_id, policy_json, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (tenant_id) DO UPDATE
		SET policy_json = EXCLUDED.policy_json,
		    updated_at  = NOW()`
	if _, err := s.db.ExecContext(ctx, q, tenantID, body); err != nil {
		return fmt.Errorf("console: upsert placement %q: %w", tenantID, err)
	}
	return nil
}
