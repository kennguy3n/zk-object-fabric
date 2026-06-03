package bucket_config

import (
	"context"
	"errors"
	"sync"

	"github.com/kennguy3n/zk-object-fabric/metadata/cors"
	"github.com/kennguy3n/zk-object-fabric/metadata/object_lock"
)

// MemoryStore is an in-memory bucket_config.Store for tests and the
// dev profile. Entries do NOT survive a restart.
type MemoryStore struct {
	mu         sync.RWMutex
	versioning map[string]VersioningState    // key: tenantID + "\x00" + bucket
	objectLock map[string]object_lock.Config // key: tenantID + "\x00" + bucket
	cors       map[string]cors.Config        // key: tenantID + "\x00" + bucket
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		versioning: make(map[string]VersioningState),
		objectLock: make(map[string]object_lock.Config),
		cors:       make(map[string]cors.Config),
	}
}

func memKey(tenantID, bucket string) string {
	return tenantID + "\x00" + bucket
}

// GetVersioning returns the stored state or VersioningUnset.
func (s *MemoryStore) GetVersioning(_ context.Context, tenantID, bucket string) (VersioningState, error) {
	if tenantID == "" || bucket == "" {
		return VersioningUnset, errors.New("bucket_config: tenant_id and bucket are required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.versioning[memKey(tenantID, bucket)], nil
}

// SetVersioning upserts the state for (tenantID, bucket).
func (s *MemoryStore) SetVersioning(_ context.Context, tenantID, bucket string, state VersioningState) error {
	if tenantID == "" || bucket == "" {
		return errors.New("bucket_config: tenant_id and bucket are required")
	}
	if !state.Valid() {
		return errors.New("bucket_config: state must be Enabled or Suspended")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.versioning[memKey(tenantID, bucket)] = state
	return nil
}

// GetObjectLock returns the stored config or the zero Config.
func (s *MemoryStore) GetObjectLock(_ context.Context, tenantID, bucket string) (object_lock.Config, error) {
	if tenantID == "" || bucket == "" {
		return object_lock.Config{}, errors.New("bucket_config: tenant_id and bucket are required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.objectLock[memKey(tenantID, bucket)], nil
}

// SetObjectLock upserts the Object Lock config for (tenantID, bucket).
func (s *MemoryStore) SetObjectLock(_ context.Context, tenantID, bucket string, cfg object_lock.Config) error {
	if tenantID == "" || bucket == "" {
		return errors.New("bucket_config: tenant_id and bucket are required")
	}
	if err := cfg.Valid(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objectLock[memKey(tenantID, bucket)] = cfg
	return nil
}

// GetCORS returns a deep copy of the stored CORS config, or the zero
// Config when the bucket has none.
func (s *MemoryStore) GetCORS(_ context.Context, tenantID, bucket string) (cors.Config, error) {
	if tenantID == "" || bucket == "" {
		return cors.Config{}, errors.New("bucket_config: tenant_id and bucket are required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneCORS(s.cors[memKey(tenantID, bucket)]), nil
}

// SetCORS upserts the CORS config for (tenantID, bucket).
func (s *MemoryStore) SetCORS(_ context.Context, tenantID, bucket string, cfg cors.Config) error {
	if tenantID == "" || bucket == "" {
		return errors.New("bucket_config: tenant_id and bucket are required")
	}
	if err := cfg.Valid(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cors[memKey(tenantID, bucket)] = cloneCORS(cfg)
	return nil
}

// DeleteCORS removes the CORS config for (tenantID, bucket). Deleting
// an unconfigured bucket is a no-op.
func (s *MemoryStore) DeleteCORS(_ context.Context, tenantID, bucket string) error {
	if tenantID == "" || bucket == "" {
		return errors.New("bucket_config: tenant_id and bucket are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cors, memKey(tenantID, bucket))
	return nil
}

// cloneCORS deep-copies a CORS config so the store never shares slice
// backing arrays with callers (a caller mutating its slice after Set,
// or after Get, must not corrupt the stored copy).
func cloneCORS(c cors.Config) cors.Config {
	if len(c.Rules) == 0 {
		return cors.Config{}
	}
	rules := make([]cors.Rule, len(c.Rules))
	for i, r := range c.Rules {
		rules[i] = cors.Rule{
			ID:             r.ID,
			AllowedOrigins: append([]string(nil), r.AllowedOrigins...),
			AllowedMethods: append([]string(nil), r.AllowedMethods...),
			AllowedHeaders: append([]string(nil), r.AllowedHeaders...),
			ExposeHeaders:  append([]string(nil), r.ExposeHeaders...),
			MaxAgeSeconds:  r.MaxAgeSeconds,
		}
	}
	return cors.Config{Rules: rules}
}
