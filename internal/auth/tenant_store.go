// Package auth implements the Phase 2 multi-tenant authenticator and
// tenant directory for the S3-compatible gateway.
//
// Phase 2 uses an in-memory tenant directory (loaded from config or a
// JSON file) plus an HMAC-based AWS Signature V4 authenticator. Phase 3
// swaps the directory for a Postgres-backed store behind the same
// TenantStore interface.
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// TenantBinding maps a gateway access key to the tenant record it
// authenticates. The secret is held alongside the binding so the
// HMAC authenticator can verify a signature without issuing a second
// lookup.
type TenantBinding struct {
	AccessKey string        `json:"access_key"`
	SecretKey string        `json:"secret_key"`
	Tenant    tenant.Tenant `json:"tenant"`
}

// TenantStore is the read / write surface the authenticator and the
// tenant console need. It is kept narrow so the Phase 3 Postgres
// implementation drops in without touching the authenticator.
type TenantStore interface {
	// LookupByAccessKey returns the binding for accessKey, or
	// (_, false) if no such binding exists.
	LookupByAccessKey(accessKey string) (TenantBinding, bool)

	// LookupByTenantID returns the first binding whose Tenant.ID
	// matches tenantID. When the tenant was created via
	// CreateTenant but has no API key binding yet, the returned
	// binding carries the tenant record with empty AccessKey /
	// SecretKey fields so rate-limit middleware that only needs
	// the Tenant still gets a usable result.
	LookupByTenantID(tenantID string) (TenantBinding, bool)

	// CreateTenant registers a tenant record with no API key
	// bindings yet. It returns an error if a tenant with the same
	// ID is already registered. The tenant-console signup handler
	// calls this before minting an initial API key pair via
	// AddBinding.
	CreateTenant(t tenant.Tenant) error

	// DeleteTenant removes the tenant record registered under
	// tenantID. It is used by the tenant-console signup handler
	// to roll back a half-finished signup so a concurrent
	// duplicate-email race cannot leave an orphaned tenant
	// record behind. Implementations should treat a missing
	// tenantID as a no-op (return nil) rather than an error.
	DeleteTenant(tenantID string) error

	// AddBinding registers a TenantBinding. Implementations should
	// reject duplicate AccessKey values or silently replace them
	// at their discretion; the console adapter layered on top of
	// TenantStore guards against accidental credential swaps.
	AddBinding(b TenantBinding) error

	// Size returns the number of access-key bindings in the
	// store. The gateway's startup path uses this to decide
	// whether to enable the rate limiter / abuse guard; zero
	// bindings means a dev / test deploy with no tenants wired.
	Size() int
}

// MemoryTenantStore is the Phase 2 in-memory TenantStore.
type MemoryTenantStore struct {
	mu       sync.RWMutex
	byAccess map[string]TenantBinding
	// tenants holds tenant records that do not (yet) have a key
	// binding. The signup flow populates this before the caller
	// mints the initial API key pair; LookupByTenantID consults
	// both this map and byAccess.
	tenants map[string]tenant.Tenant
}

// NewMemoryTenantStore returns an empty store.
func NewMemoryTenantStore() *MemoryTenantStore {
	return &MemoryTenantStore{
		byAccess: map[string]TenantBinding{},
		tenants:  map[string]tenant.Tenant{},
	}
}

// CreateTenant registers a bare tenant record. It returns an error
// if a tenant with the same ID already exists (either as a standalone
// record or attached to an existing binding).
func (s *MemoryTenantStore) CreateTenant(t tenant.Tenant) error {
	if t.ID == "" {
		return fmt.Errorf("auth: tenant.id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tenants[t.ID]; ok {
		return fmt.Errorf("auth: tenant %q already exists", t.ID)
	}
	for _, b := range s.byAccess {
		if b.Tenant.ID == t.ID {
			return fmt.Errorf("auth: tenant %q already exists", t.ID)
		}
	}
	s.tenants[t.ID] = t
	return nil
}

// DeleteTenant removes the tenant record registered via
// CreateTenant and every TenantBinding (access-key row) that
// references tenantID. It is idempotent: deleting an unknown
// tenantID is a no-op.
//
// Scanning byAccess is necessary because the signup rollback path
// runs after AddAPIKey has already registered an initial binding;
// without this the orphaned access key would remain valid for S3
// authentication even though the tenant record is gone. Tenant IDs
// are minted by a CSPRNG (defaultTenantIDGenerator), so there is
// no risk of revoking a concurrent login session that reuses the
// same ID — the ID collision probability is cryptographic noise.
func (s *MemoryTenantStore) DeleteTenant(tenantID string) error {
	if tenantID == "" {
		return fmt.Errorf("auth: tenant.id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tenants, tenantID)
	for accessKey, b := range s.byAccess {
		if b.Tenant.ID == tenantID {
			delete(s.byAccess, accessKey)
		}
	}
	return nil
}

// AddBinding registers a TenantBinding. It replaces any existing
// binding with the same AccessKey.
func (s *MemoryTenantStore) AddBinding(b TenantBinding) error {
	if b.AccessKey == "" {
		return fmt.Errorf("auth: binding access_key is required")
	}
	if b.SecretKey == "" {
		return fmt.Errorf("auth: binding secret_key is required")
	}
	if b.Tenant.ID == "" {
		return fmt.Errorf("auth: binding tenant.id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byAccess[b.AccessKey] = b
	return nil
}

// LookupByAccessKey implements TenantStore.
func (s *MemoryTenantStore) LookupByAccessKey(accessKey string) (TenantBinding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.byAccess[accessKey]
	return b, ok
}

// LookupByTenantID returns the first binding whose Tenant.ID matches
// tenantID. It is used by rate-limit middleware that already knows
// the tenant ID and wants the associated budgets. When the tenant
// was created via CreateTenant but has no API key binding yet, the
// returned binding carries the tenant record with empty
// AccessKey / SecretKey fields.
func (s *MemoryTenantStore) LookupByTenantID(tenantID string) (TenantBinding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.byAccess {
		if b.Tenant.ID == tenantID {
			return b, true
		}
	}
	if t, ok := s.tenants[tenantID]; ok {
		return TenantBinding{Tenant: t}, true
	}
	return TenantBinding{}, false
}

// Size returns the number of registered bindings.
func (s *MemoryTenantStore) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byAccess)
}

// ListBindingsByTenantID returns every binding whose Tenant.ID
// matches tenantID. The tenant console uses this to render the API
// keys list; the slice is freshly allocated so callers may safely
// retain it. Returns an empty (non-nil) slice when no bindings are
// registered for the tenant.
//
// The error return mirrors the PostgresTenantStore signature so
// callers can surface a 500 on a real backend failure; the
// in-memory implementation never errors and always returns nil.
func (s *MemoryTenantStore) ListBindingsByTenantID(tenantID string) ([]TenantBinding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TenantBinding, 0)
	for _, b := range s.byAccess {
		if b.Tenant.ID == tenantID {
			out = append(out, b)
		}
	}
	return out, nil
}

// RemoveBinding deletes the binding identified by accessKey. It is
// idempotent: a missing access key is not an error. The tenant
// console's revoke-key handler calls this so a compromised access
// key stops authenticating S3 requests immediately.
func (s *MemoryTenantStore) RemoveBinding(accessKey string) error {
	if accessKey == "" {
		return fmt.Errorf("auth: access_key is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byAccess, accessKey)
	return nil
}

// LoadBindingsFromJSON reads a JSON file containing a list of
// TenantBindings and registers them in the store.
func (s *MemoryTenantStore) LoadBindingsFromJSON(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("auth: read bindings %q: %w", path, err)
	}
	var bindings []TenantBinding
	if err := json.Unmarshal(data, &bindings); err != nil {
		return fmt.Errorf("auth: parse bindings %q: %w", path, err)
	}
	for i, b := range bindings {
		if err := s.AddBinding(b); err != nil {
			return fmt.Errorf("auth: binding[%d]: %w", i, err)
		}
	}
	return nil
}
