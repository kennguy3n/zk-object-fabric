package auth

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// PostgresTenantStore is the Phase 3 Postgres-backed TenantStore. It
// implements the same surface as MemoryTenantStore plus the two
// helper methods (AddBinding, LookupByTenantID) that the gateway's
// main.go adapter wires into the console API.
//
// Schema: see internal/auth/schema.sql. The tenants table owns the
// canonical tenant record; tenant_bindings holds per-access-key
// bindings and denormalizes the tenant JSON so the authenticator's
// hot-path LookupByAccessKey answers without a JOIN.
//
// The store is safe for concurrent use: all methods dispatch to the
// *sql.DB which is itself safe for concurrent callers.
type PostgresTenantStore struct {
	db *sql.DB
}

// NewPostgresTenantStore wraps db. The caller is responsible for
// opening the *sql.DB and for running the migration in schema.sql
// before issuing the first query.
func NewPostgresTenantStore(db *sql.DB) (*PostgresTenantStore, error) {
	if db == nil {
		return nil, errors.New("auth: postgres tenant store requires a non-nil *sql.DB")
	}
	return &PostgresTenantStore{db: db}, nil
}

// LookupByAccessKey implements TenantStore.
func (s *PostgresTenantStore) LookupByAccessKey(accessKey string) (TenantBinding, bool) {
	if accessKey == "" {
		return TenantBinding{}, false
	}
	const q = `SELECT secret_key, tenant_id, tenant_json FROM tenant_bindings WHERE access_key = $1`
	var (
		secretKey  string
		tenantID   string
		tenantJSON []byte
	)
	row := s.db.QueryRow(q, accessKey)
	if err := row.Scan(&secretKey, &tenantID, &tenantJSON); err != nil {
		return TenantBinding{}, false
	}
	var t tenant.Tenant
	if err := json.Unmarshal(tenantJSON, &t); err != nil {
		return TenantBinding{}, false
	}
	return TenantBinding{AccessKey: accessKey, SecretKey: secretKey, Tenant: t}, true
}

// LookupByTenantID returns the first binding whose Tenant.ID matches
// tenantID. When the tenant was created via CreateTenant but has no
// API key binding yet, the returned binding carries the tenant
// record with empty AccessKey / SecretKey fields so callers that
// only need the Tenant (rate-limit middleware, budgets lookup) still
// get a usable result.
func (s *PostgresTenantStore) LookupByTenantID(tenantID string) (TenantBinding, bool) {
	if tenantID == "" {
		return TenantBinding{}, false
	}
	// Bindings win if they exist so callers get a fully-populated
	// (access, secret, tenant) triple on the first lookup.
	const bindingQ = `
		SELECT access_key, secret_key, tenant_json
		  FROM tenant_bindings
		 WHERE tenant_id = $1
		 LIMIT 1`
	var (
		accessKey  string
		secretKey  string
		tenantJSON []byte
	)
	if err := s.db.QueryRow(bindingQ, tenantID).Scan(&accessKey, &secretKey, &tenantJSON); err == nil {
		var t tenant.Tenant
		if err := json.Unmarshal(tenantJSON, &t); err != nil {
			return TenantBinding{}, false
		}
		return TenantBinding{AccessKey: accessKey, SecretKey: secretKey, Tenant: t}, true
	}
	// Fall back to the bare tenant row for tenants created by the
	// signup handler that have not yet had an API key minted.
	const tenantQ = `SELECT tenant_json FROM tenants WHERE tenant_id = $1`
	if err := s.db.QueryRow(tenantQ, tenantID).Scan(&tenantJSON); err != nil {
		return TenantBinding{}, false
	}
	var t tenant.Tenant
	if err := json.Unmarshal(tenantJSON, &t); err != nil {
		return TenantBinding{}, false
	}
	return TenantBinding{Tenant: t}, true
}

// CreateTenant implements TenantStore. It rejects duplicate tenant
// IDs via the tenants.tenant_id primary key.
func (s *PostgresTenantStore) CreateTenant(t tenant.Tenant) error {
	if t.ID == "" {
		return errors.New("auth: tenant.id is required")
	}
	body, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("auth: marshal tenant %q: %w", t.ID, err)
	}
	const q = `INSERT INTO tenants (tenant_id, tenant_json) VALUES ($1, $2)`
	if _, err := s.db.Exec(q, t.ID, body); err != nil {
		return fmt.Errorf("auth: insert tenant %q: %w", t.ID, err)
	}
	return nil
}

// DeleteTenant removes the tenant row and cascades to tenant_bindings
// via ON DELETE CASCADE (see schema.sql). It is idempotent: a
// missing tenantID is not an error.
func (s *PostgresTenantStore) DeleteTenant(tenantID string) error {
	if tenantID == "" {
		return errors.New("auth: tenant.id is required")
	}
	const q = `DELETE FROM tenants WHERE tenant_id = $1`
	if _, err := s.db.Exec(q, tenantID); err != nil {
		return fmt.Errorf("auth: delete tenant %q: %w", tenantID, err)
	}
	return nil
}

// AddBinding writes a new (access_key, secret_key, tenant) row. The
// tenants row must already exist (CreateTenant first); the foreign
// key constraint in schema.sql enforces this at the database level.
func (s *PostgresTenantStore) AddBinding(b TenantBinding) error {
	if b.AccessKey == "" {
		return errors.New("auth: binding access_key is required")
	}
	if b.SecretKey == "" {
		return errors.New("auth: binding secret_key is required")
	}
	if b.Tenant.ID == "" {
		return errors.New("auth: binding tenant.id is required")
	}
	body, err := json.Marshal(b.Tenant)
	if err != nil {
		return fmt.Errorf("auth: marshal tenant %q: %w", b.Tenant.ID, err)
	}
	const q = `
		INSERT INTO tenant_bindings (access_key, secret_key, tenant_id, tenant_json)
		VALUES ($1, $2, $3, $4)`
	if _, err := s.db.Exec(q, b.AccessKey, b.SecretKey, b.Tenant.ID, body); err != nil {
		return fmt.Errorf("auth: insert binding %q: %w", b.AccessKey, err)
	}
	return nil
}

// Size returns the number of (access_key) bindings in the store.
// Mirrors MemoryTenantStore.Size so the gateway's main.go can use
// the same branch to decide whether to enable the rate limiter.
func (s *PostgresTenantStore) Size() int {
	const q = `SELECT COUNT(*) FROM tenant_bindings`
	var n int
	if err := s.db.QueryRow(q).Scan(&n); err != nil {
		return 0
	}
	return n
}
