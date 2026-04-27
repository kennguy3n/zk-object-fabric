// Package compliance provides the structured audit trail and
// data-residency enforcement that the gateway needs in order to
// answer "where is my data" questions and reject placements that
// would violate a tenant's country list.
//
// The package is split into three pieces:
//
//   - audit_trail.go  : AuditEntry type, AuditStore interface,
//                       in-memory implementation.
//   - postgres_audit.go: Postgres-backed AuditStore.
//   - residency_enforcer.go: Pre-flight check called from the PUT
//                       hot path. Returns a typed error so the S3
//                       handler can map it to a 403
//                       DataResidencyViolation.
//
// All public types accept and return values in UTC.
package compliance

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// AuditEntry records one observable, billable, or compliance-
// relevant event in the gateway. Entries are append-only.
type AuditEntry struct {
	TenantID       string
	Operation      string
	Bucket         string
	ObjectKey      string
	PieceID        string
	PieceBackend   string
	BackendCountry string
	Timestamp      time.Time
	RequestID      string
}

// TimeRange is a closed interval used for AuditStore.Query.
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// AuditStore is the persistent surface for compliance events.
// Implementations MUST be safe for concurrent use.
type AuditStore interface {
	// Record appends a single audit entry. Implementations MAY
	// enforce a maximum size cap; callers must treat ErrAuditFull
	// as a soft, non-fatal warning.
	Record(ctx context.Context, entry AuditEntry) error

	// Query returns every audit entry for tenantID whose timestamp
	// falls within rng (inclusive on both bounds). Entries are
	// returned in ascending timestamp order. The slice may be
	// empty but is never nil.
	Query(ctx context.Context, tenantID string, rng TimeRange) ([]AuditEntry, error)
}

// ErrAuditFull is returned by capacity-bounded AuditStore
// implementations when the trail can no longer accept new rows.
// It is non-fatal and callers should log+continue.
var ErrAuditFull = errors.New("compliance: audit trail full")

// MemoryAuditStore is the default in-memory AuditStore. Useful in
// tests and for single-process deployments without a Postgres
// dependency.
type MemoryAuditStore struct {
	mu      sync.Mutex
	entries []AuditEntry
}

// NewMemoryAuditStore returns an empty store.
func NewMemoryAuditStore() *MemoryAuditStore {
	return &MemoryAuditStore{}
}

// Record implements AuditStore.
func (s *MemoryAuditStore) Record(_ context.Context, entry AuditEntry) error {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	} else {
		entry.Timestamp = entry.Timestamp.UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

// Query implements AuditStore.
func (s *MemoryAuditStore) Query(_ context.Context, tenantID string, rng TimeRange) ([]AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuditEntry, 0)
	start, end := rng.Start.UTC(), rng.End.UTC()
	for _, e := range s.entries {
		if e.TenantID != tenantID {
			continue
		}
		if !start.IsZero() && e.Timestamp.Before(start) {
			continue
		}
		if !end.IsZero() && e.Timestamp.After(end) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

// Len returns the current number of entries; primarily for tests.
func (s *MemoryAuditStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
