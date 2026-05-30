package chaos

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
)

// FaultManifestStore wraps a manifest_store.ManifestStore with the
// same fault-injection vocabulary as FaultProvider, but applied to
// the control-plane metadata path rather than the data path.
//
// The chaos failure modes this models are the ones that actually
// happen in production:
//
//   - The Postgres primary is down and replicas are read-only
//     (writes fault, reads pass).
//   - The Postgres write path is in a degraded "fail every Nth"
//     state under connection-pool exhaustion.
//   - The whole tier is partitioned for a known interval.
//
// FaultManifestStore is safe for concurrent use.
type FaultManifestStore struct {
	// Inner is the wrapped store. Required.
	Inner manifest_store.ManifestStore

	// Now is the clock used by ModeFailUntilTime. Defaults to
	// time.Now if nil. Tests override this for determinism.
	Now func() time.Time

	// Per-operation faults. Each Get/Put/Delete/List/HasManifest/
	// ListVersions consults exactly one. Embedding by-value keeps
	// the zero-value usage clean (everything passes through).
	PutFault          FaultConfig
	GetFault          FaultConfig
	DeleteFault       FaultConfig
	ListFault         FaultConfig
	HasFault          FaultConfig
	ListVersionsFault FaultConfig

	mu       sync.Mutex
	counters map[string]int64

	// Calls and Failures track total / faulted operations across
	// all method kinds, for chaos assertions like "the gateway
	// retried Put at least 3 times under fault".
	Calls    atomic.Int64
	Failures atomic.Int64
}

// NewFaultManifestStore returns a fault-injecting wrapper around
// inner. inner must be non-nil; passing nil is a programmer error
// and is caught by every downstream call.
func NewFaultManifestStore(inner manifest_store.ManifestStore) *FaultManifestStore {
	return &FaultManifestStore{Inner: inner}
}

func (s *FaultManifestStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// shouldFail mirrors FaultProvider.shouldFail. We do not share the
// implementation because the operation-name namespace is different
// (manifest-store calls vs. piece calls) and conflating them would
// make ModeFailEveryNth behave unintuitively for tests that wrap both.
func (s *FaultManifestStore) shouldFail(op string, cfg FaultConfig) (bool, error) {
	if s.Inner == nil {
		return true, errors.New("chaos: FaultManifestStore.Inner is nil")
	}
	switch cfg.Mode {
	case ModeNone:
		return false, nil
	case ModeAlwaysFail:
		return true, faultErr(cfg)
	case ModeFailEveryNth:
		n := cfg.EveryNth
		if n <= 1 {
			return true, faultErr(cfg)
		}
		c := s.bumpCounter(op)
		if c%int64(n) == 0 {
			return true, faultErr(cfg)
		}
		return false, nil
	case ModeFailFirstN:
		if cfg.FirstN <= 0 {
			return false, nil
		}
		c := s.bumpCounter(op)
		if c <= int64(cfg.FirstN) {
			return true, faultErr(cfg)
		}
		return false, nil
	case ModeFailUntilTime:
		if s.now().Before(cfg.FailUntil) {
			return true, faultErr(cfg)
		}
		return false, nil
	case ModeTruncatedRead, ModeSlowResponse:
		// Not meaningful for the manifest store (no streaming
		// reader, latency injection isn't useful here in
		// practice). Treat as pass-through rather than panic so
		// a misconfigured test still surfaces a coherent
		// behaviour.
		return false, nil
	default:
		return true, errors.New("chaos: unknown FaultMode " + cfg.Mode.String())
	}
}

func (s *FaultManifestStore) bumpCounter(op string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counters == nil {
		s.counters = map[string]int64{}
	}
	s.counters[op]++
	return s.counters[op]
}

// Put dispatches through PutFault.
func (s *FaultManifestStore) Put(ctx context.Context, key manifest_store.ManifestKey, m *metadata.ObjectManifest) error {
	s.Calls.Add(1)
	fail, err := s.shouldFail("PUT", s.PutFault)
	if fail {
		s.Failures.Add(1)
		return err
	}
	return s.Inner.Put(ctx, key, m)
}

// Get dispatches through GetFault.
func (s *FaultManifestStore) Get(ctx context.Context, key manifest_store.ManifestKey) (*metadata.ObjectManifest, error) {
	s.Calls.Add(1)
	fail, err := s.shouldFail("GET", s.GetFault)
	if fail {
		s.Failures.Add(1)
		return nil, err
	}
	return s.Inner.Get(ctx, key)
}

// Delete dispatches through DeleteFault.
func (s *FaultManifestStore) Delete(ctx context.Context, key manifest_store.ManifestKey) error {
	s.Calls.Add(1)
	fail, err := s.shouldFail("DELETE", s.DeleteFault)
	if fail {
		s.Failures.Add(1)
		return err
	}
	return s.Inner.Delete(ctx, key)
}

// List dispatches through ListFault.
func (s *FaultManifestStore) List(ctx context.Context, tenantID, bucket, cursor string, limit int) (manifest_store.ListResult, error) {
	s.Calls.Add(1)
	fail, err := s.shouldFail("LIST", s.ListFault)
	if fail {
		s.Failures.Add(1)
		return manifest_store.ListResult{}, err
	}
	return s.Inner.List(ctx, tenantID, bucket, cursor, limit)
}

// HasManifestWithPieceID dispatches through HasFault.
func (s *FaultManifestStore) HasManifestWithPieceID(ctx context.Context, tenantID, pieceID string) (bool, error) {
	s.Calls.Add(1)
	fail, err := s.shouldFail("HAS", s.HasFault)
	if fail {
		s.Failures.Add(1)
		return false, err
	}
	return s.Inner.HasManifestWithPieceID(ctx, tenantID, pieceID)
}

// ListVersions dispatches through ListVersionsFault.
func (s *FaultManifestStore) ListVersions(ctx context.Context, tenantID, bucket, objectKeyHash string) ([]*metadata.ObjectManifest, error) {
	s.Calls.Add(1)
	fail, err := s.shouldFail("LIST_VERSIONS", s.ListVersionsFault)
	if fail {
		s.Failures.Add(1)
		return nil, err
	}
	return s.Inner.ListVersions(ctx, tenantID, bucket, objectKeyHash)
}
