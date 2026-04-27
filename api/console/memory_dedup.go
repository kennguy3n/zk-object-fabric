// In-memory DedupPolicyStore for dev / tests. The Postgres
// implementation lands in api/console/postgres_dedup.go in a future
// phase; until then operators use this store via the local_fs_dev
// profile and via tests that exercise the dedup-policy routes.
package console

import (
	"context"
	"sync"

	"github.com/kennguy3n/zk-object-fabric/metadata"
)

// MemoryDedupPolicyStore is a process-local DedupPolicyStore. It
// is safe for concurrent use; restarts drop every policy.
type MemoryDedupPolicyStore struct {
	mu       sync.Mutex
	policies map[memoryDedupKey]metadata.DedupPolicy
}

type memoryDedupKey struct {
	TenantID string
	Bucket   string
}

// NewMemoryDedupPolicyStore returns an empty in-memory store.
func NewMemoryDedupPolicyStore() *MemoryDedupPolicyStore {
	return &MemoryDedupPolicyStore{policies: map[memoryDedupKey]metadata.DedupPolicy{}}
}

func (s *MemoryDedupPolicyStore) GetDedupPolicy(_ context.Context, tenantID, bucket string) (*metadata.DedupPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.policies[memoryDedupKey{tenantID, bucket}]
	if !ok {
		return nil, nil
	}
	cp := p
	return &cp, nil
}

func (s *MemoryDedupPolicyStore) PutDedupPolicy(_ context.Context, tenantID, bucket string, policy metadata.DedupPolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies[memoryDedupKey{tenantID, bucket}] = policy
	return nil
}

func (s *MemoryDedupPolicyStore) DeleteDedupPolicy(_ context.Context, tenantID, bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.policies, memoryDedupKey{tenantID, bucket})
	return nil
}

var _ DedupPolicyStore = (*MemoryDedupPolicyStore)(nil)
