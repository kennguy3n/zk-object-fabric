package bucket_config

import (
	"context"
	"errors"
	"sync"

	"github.com/kennguy3n/zk-object-fabric/metadata/object_lock"
)

// MemoryStore is an in-memory bucket_config.Store for tests and the
// dev profile. Entries do NOT survive a restart.
type MemoryStore struct {
	mu         sync.RWMutex
	versioning map[string]VersioningState   // key: tenantID + "\x00" + bucket
	objectLock map[string]object_lock.Config // key: tenantID + "\x00" + bucket
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		versioning: make(map[string]VersioningState),
		objectLock: make(map[string]object_lock.Config),
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
