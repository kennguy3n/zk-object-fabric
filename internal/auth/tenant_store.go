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

// TenantStore is the read surface the authenticator needs. It is kept
// narrow so the Phase 3 Postgres implementation drops in without
// touching the authenticator.
type TenantStore interface {
	// LookupByAccessKey returns the binding for accessKey, or
	// (_, false) if no such binding exists.
	LookupByAccessKey(accessKey string) (TenantBinding, bool)
}

// MemoryTenantStore is the Phase 2 in-memory TenantStore.
type MemoryTenantStore struct {
	mu       sync.RWMutex
	byAccess map[string]TenantBinding
}

// NewMemoryTenantStore returns an empty store.
func NewMemoryTenantStore() *MemoryTenantStore {
	return &MemoryTenantStore{byAccess: map[string]TenantBinding{}}
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
// the tenant ID and wants the associated budgets.
func (s *MemoryTenantStore) LookupByTenantID(tenantID string) (TenantBinding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.byAccess {
		if b.Tenant.ID == tenantID {
			return b, true
		}
	}
	return TenantBinding{}, false
}

// Size returns the number of registered bindings.
func (s *MemoryTenantStore) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byAccess)
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
