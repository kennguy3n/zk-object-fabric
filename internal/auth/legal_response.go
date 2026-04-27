package auth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// LegalHold is an operator-asserted retention flag on either a
// whole tenant, a specific bucket, or a specific (bucket, key)
// pair. Pieces under an active hold MUST NOT be deleted by any
// path — user-initiated DELETE, lifecycle expiry, or background
// orphan-GC alike. The hold is also written to the compliance
// audit trail so legal teams can later prove the data was
// preserved through the hold window.
type LegalHold struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	Bucket     string    `json:"bucket,omitempty"`
	ObjectKey  string    `json:"object_key,omitempty"`
	Reason     string    `json:"reason"`
	CaseID     string    `json:"case_id,omitempty"`
	IssuedBy   string    `json:"issued_by"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	Released   bool      `json:"released"`
	ReleasedAt time.Time `json:"released_at,omitempty"`
}

// Active returns true if the hold is currently in force.
func (h LegalHold) Active(now time.Time) bool {
	if h.Released {
		return false
	}
	if !h.ExpiresAt.IsZero() && !now.Before(h.ExpiresAt) {
		return false
	}
	return true
}

// Matches returns true if the hold covers the (tenant, bucket,
// objectKey) tuple. Empty Bucket on the hold matches any bucket;
// empty ObjectKey on the hold matches any object within the
// scoped bucket. The hold's TenantID MUST match exactly — there
// is no cross-tenant hold.
func (h LegalHold) Matches(tenantID, bucket, objectKey string) bool {
	if h.TenantID != tenantID {
		return false
	}
	if h.Bucket != "" && h.Bucket != bucket {
		return false
	}
	if h.ObjectKey != "" && h.ObjectKey != objectKey {
		return false
	}
	return true
}

// LegalHoldStore is the persistence interface for LegalHold
// records. The in-memory implementation is provided here; the
// console layer wires a Postgres-backed implementation in
// follow-up.
type LegalHoldStore interface {
	Create(ctx context.Context, hold LegalHold) error
	Release(ctx context.Context, id string) error
	List(ctx context.Context, tenantID string) ([]LegalHold, error)
	Active(ctx context.Context, tenantID, bucket, objectKey string) ([]LegalHold, error)
}

// MemoryLegalHoldStore is an in-memory LegalHoldStore. Safe for
// concurrent use; appends and lookups are O(n) which is fine for
// the typical small (< 1000) population of active holds per
// gateway.
type MemoryLegalHoldStore struct {
	mu    sync.RWMutex
	holds map[string]LegalHold
	clock func() time.Time
}

// NewMemoryLegalHoldStore returns a ready store using time.Now
// for the clock.
func NewMemoryLegalHoldStore() *MemoryLegalHoldStore {
	return &MemoryLegalHoldStore{
		holds: map[string]LegalHold{},
		clock: time.Now,
	}
}

// Create inserts a hold. Duplicate IDs are rejected. CreatedAt
// is filled from the store clock when zero.
func (s *MemoryLegalHoldStore) Create(_ context.Context, hold LegalHold) error {
	if hold.ID == "" || hold.TenantID == "" {
		return errors.New("legal_hold: id and tenant_id are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.holds[hold.ID]; ok {
		return errors.New("legal_hold: id already exists")
	}
	if hold.CreatedAt.IsZero() {
		hold.CreatedAt = s.clock()
	}
	s.holds[hold.ID] = hold
	return nil
}

// Release marks the hold released; the record stays in the
// store for audit trail purposes.
func (s *MemoryLegalHoldStore) Release(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.holds[id]
	if !ok {
		return errors.New("legal_hold: not found")
	}
	h.Released = true
	h.ReleasedAt = s.clock()
	s.holds[id] = h
	return nil
}

// List returns every hold for tenantID.
func (s *MemoryLegalHoldStore) List(_ context.Context, tenantID string) ([]LegalHold, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]LegalHold, 0, len(s.holds))
	for _, h := range s.holds {
		if h.TenantID == tenantID {
			out = append(out, h)
		}
	}
	return out, nil
}

// Active returns the subset of holds that currently apply to
// (tenantID, bucket, objectKey).
func (s *MemoryLegalHoldStore) Active(_ context.Context, tenantID, bucket, objectKey string) ([]LegalHold, error) {
	now := s.clock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]LegalHold, 0)
	for _, h := range s.holds {
		if h.Active(now) && h.Matches(tenantID, bucket, objectKey) {
			out = append(out, h)
		}
	}
	return out, nil
}

// ErrLegalHoldActive is the canonical error returned by the
// delete hot path when one or more holds prevent the operation.
var ErrLegalHoldActive = errors.New("legal_hold: object is under an active legal hold")

// CheckDelete is the helper the s3compat handler should call
// before executing a DELETE: it returns ErrLegalHoldActive if
// any hold matches.
func CheckDelete(ctx context.Context, store LegalHoldStore, tenantID, bucket, objectKey string) error {
	if store == nil {
		return nil
	}
	holds, err := store.Active(ctx, tenantID, bucket, objectKey)
	if err != nil {
		return err
	}
	if len(holds) > 0 {
		return ErrLegalHoldActive
	}
	return nil
}
