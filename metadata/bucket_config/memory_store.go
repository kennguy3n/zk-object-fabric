package bucket_config

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/kennguy3n/zk-object-fabric/metadata/cors"
	"github.com/kennguy3n/zk-object-fabric/metadata/lifecycle"
	"github.com/kennguy3n/zk-object-fabric/metadata/object_lock"
	"github.com/kennguy3n/zk-object-fabric/metadata/sse"
)

// MemoryStore is an in-memory bucket_config.Store for tests and the
// dev profile. Entries do NOT survive a restart.
type MemoryStore struct {
	mu         sync.RWMutex
	versioning map[string]VersioningState    // key: tenantID + "\x00" + bucket
	objectLock map[string]object_lock.Config // key: tenantID + "\x00" + bucket
	cors       map[string]cors.Config        // key: tenantID + "\x00" + bucket
	lifecycle  map[string]lifecycle.Config   // key: tenantID + "\x00" + bucket
	encryption map[string]sse.Config         // key: tenantID + "\x00" + bucket
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		versioning: make(map[string]VersioningState),
		objectLock: make(map[string]object_lock.Config),
		cors:       make(map[string]cors.Config),
		lifecycle:  make(map[string]lifecycle.Config),
		encryption: make(map[string]sse.Config),
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

// GetLifecycle returns a deep copy of the stored lifecycle config, or
// the zero Config when the bucket has none.
func (s *MemoryStore) GetLifecycle(_ context.Context, tenantID, bucket string) (lifecycle.Config, error) {
	if tenantID == "" || bucket == "" {
		return lifecycle.Config{}, errors.New("bucket_config: tenant_id and bucket are required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneLifecycle(s.lifecycle[memKey(tenantID, bucket)]), nil
}

// SetLifecycle upserts the lifecycle config for (tenantID, bucket).
func (s *MemoryStore) SetLifecycle(_ context.Context, tenantID, bucket string, cfg lifecycle.Config) error {
	if tenantID == "" || bucket == "" {
		return errors.New("bucket_config: tenant_id and bucket are required")
	}
	if err := cfg.Valid(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifecycle[memKey(tenantID, bucket)] = cloneLifecycle(cfg)
	return nil
}

// DeleteLifecycle removes the lifecycle config for (tenantID, bucket).
// Deleting an unconfigured bucket is a no-op.
func (s *MemoryStore) DeleteLifecycle(_ context.Context, tenantID, bucket string) error {
	if tenantID == "" || bucket == "" {
		return errors.New("bucket_config: tenant_id and bucket are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.lifecycle, memKey(tenantID, bucket))
	return nil
}

// ListLifecycle returns a deep copy of every configured bucket
// lifecycle entry across all tenants, for the background evaluator.
func (s *MemoryStore) ListLifecycle(_ context.Context) ([]LifecycleEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]LifecycleEntry, 0, len(s.lifecycle))
	for k, cfg := range s.lifecycle {
		tenantID, bucket, ok := strings.Cut(k, "\x00")
		if !ok {
			continue
		}
		out = append(out, LifecycleEntry{
			TenantID: tenantID,
			Bucket:   bucket,
			Config:   cloneLifecycle(cfg),
		})
	}
	return out, nil
}

// GetEncryption returns the stored bucket default SSE config, or the
// zero Config when the bucket has none. sse.Config is a flat value
// type (no slices/maps/pointers), so it is safe to return by value
// without a deep copy.
func (s *MemoryStore) GetEncryption(_ context.Context, tenantID, bucket string) (sse.Config, error) {
	if tenantID == "" || bucket == "" {
		return sse.Config{}, errors.New("bucket_config: tenant_id and bucket are required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.encryption[memKey(tenantID, bucket)], nil
}

// SetEncryption upserts the bucket default SSE config for (tenantID,
// bucket).
func (s *MemoryStore) SetEncryption(_ context.Context, tenantID, bucket string, cfg sse.Config) error {
	if tenantID == "" || bucket == "" {
		return errors.New("bucket_config: tenant_id and bucket are required")
	}
	if err := cfg.Valid(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.encryption[memKey(tenantID, bucket)] = cfg
	return nil
}

// DeleteEncryption removes the bucket default SSE config for (tenantID,
// bucket). Deleting an unconfigured bucket is a no-op.
func (s *MemoryStore) DeleteEncryption(_ context.Context, tenantID, bucket string) error {
	if tenantID == "" || bucket == "" {
		return errors.New("bucket_config: tenant_id and bucket are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.encryption, memKey(tenantID, bucket))
	return nil
}

// cloneLifecycle deep-copies a lifecycle config so the store never
// shares slice/map/pointer state with callers.
func cloneLifecycle(c lifecycle.Config) lifecycle.Config {
	if len(c.Rules) == 0 {
		return lifecycle.Config{}
	}
	rules := make([]lifecycle.Rule, len(c.Rules))
	for i, r := range c.Rules {
		nr := lifecycle.Rule{
			ID:     r.ID,
			Status: r.Status,
			Filter: lifecycle.Filter{
				Prefix:                r.Filter.Prefix,
				Tags:                  cloneStringMap(r.Filter.Tags),
				ObjectSizeGreaterThan: cloneInt64Ptr(r.Filter.ObjectSizeGreaterThan),
				ObjectSizeLessThan:    cloneInt64Ptr(r.Filter.ObjectSizeLessThan),
			},
		}
		if r.Expiration != nil {
			e := *r.Expiration
			nr.Expiration = &e
		}
		if len(r.Transitions) > 0 {
			nr.Transitions = append([]lifecycle.Transition(nil), r.Transitions...)
		}
		if r.AbortIncompleteMultipartUpload != nil {
			a := *r.AbortIncompleteMultipartUpload
			nr.AbortIncompleteMultipartUpload = &a
		}
		rules[i] = nr
	}
	return lifecycle.Config{Rules: rules}
}

func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneInt64Ptr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
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
