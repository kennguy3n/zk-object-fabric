// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/internal/compliance"
	"github.com/kennguy3n/zk-object-fabric/internal/config"
	"github.com/kennguy3n/zk-object-fabric/lifecycle/evaluator"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/lifecycle"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
)

// recordingBillingSink captures emitted usage events for assertions.
type recordingBillingSink struct {
	mu     sync.Mutex
	events []billing.UsageEvent
}

func (s *recordingBillingSink) Emit(e billing.UsageEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *recordingBillingSink) nodeFor(d billing.Dimension) (node string, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Dimension == d {
			return e.SourceNodeID, true
		}
	}
	return "", false
}

func (s *recordingBillingSink) dimensions() []billing.Dimension {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]billing.Dimension, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.Dimension)
	}
	return out
}

// TestBuildBucketConfigStore_MemoryFallback verifies that with no
// metadata DB and no embedded DB the gateway falls back to a usable
// in-memory bucket_config store rather than returning nil (which
// would leave every bucket sub-resource handler and the evaluator
// dormant).
func TestBuildBucketConfigStore_MemoryFallback(t *testing.T) {
	store := buildBucketConfigStore(config.Default(), nil, nil)
	if store == nil {
		t.Fatal("buildBucketConfigStore returned nil with no DB configured")
	}
	if _, err := store.GetVersioning(context.Background(), "t", "b"); err != nil {
		t.Fatalf("GetVersioning on memory store: %v", err)
	}
}

// TestStartLifecycleEvaluator_NotStarted asserts the worker is not
// started (nil channel) when it is disabled or has no bucket-config
// store to enumerate.
func TestStartLifecycleEvaluator_NotStarted(t *testing.T) {
	bc := bucket_config.NewMemoryStore()
	if done := startLifecycleEvaluator(context.Background(), config.LifecycleConfig{Enabled: false},
		bc, memory.New(), nil, nil, nil, nil, nil, "test"); done != nil {
		t.Fatal("disabled lifecycle evaluator returned a non-nil channel")
	}
	if done := startLifecycleEvaluator(context.Background(), config.LifecycleConfig{Enabled: true},
		nil, memory.New(), nil, nil, nil, nil, nil, "test"); done != nil {
		t.Fatal("nil bucketConfig returned a non-nil channel")
	}
}

// TestStartLifecycleEvaluator_ExpiresAuditsMeters is an end-to-end
// wiring test: it seeds a bucket lifecycle rule and an aged object,
// starts the worker, and asserts the object is expired, recorded in
// the shared compliance audit store, and metered on the dedicated
// LifecycleExpirations dimension with the wired NodeID — proving the
// store, audit adapter, billing sink, and NodeID are all wired into
// the evaluator. It then asserts the goroutine drains on shutdown.
func TestStartLifecycleEvaluator_ExpiresAuditsMeters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bc := bucket_config.NewMemoryStore()
	if err := bc.SetLifecycle(ctx, "tenant-1", "bucket-1", lifecycle.Config{
		Rules: []lifecycle.Rule{{
			Status:     lifecycle.StatusEnabled,
			Filter:     lifecycle.Filter{Prefix: "logs/"},
			Expiration: &lifecycle.Expiration{Days: 30},
		}},
	}); err != nil {
		t.Fatalf("SetLifecycle: %v", err)
	}

	mans := memory.New()
	key := manifest_store.ManifestKey{TenantID: "tenant-1", Bucket: "bucket-1", ObjectKeyHash: "h-old", VersionID: "v1"}
	if err := mans.Put(ctx, key, &metadata.ObjectManifest{
		TenantID:      "tenant-1",
		Bucket:        "bucket-1",
		ObjectKey:     "logs/old.txt",
		ObjectKeyHash: "h-old",
		VersionID:     "v1",
		CreatedAt:     time.Now().UTC().AddDate(0, 0, -40),
		Pieces:        []metadata.Piece{{PieceID: "p1", Backend: "local"}},
	}); err != nil {
		t.Fatalf("Put manifest: %v", err)
	}

	auditStore := compliance.NewMemoryAuditStore()
	bill := &recordingBillingSink{}

	done := startLifecycleEvaluator(ctx, config.LifecycleConfig{Enabled: true, Interval: config.Duration(time.Hour)},
		bc, mans, nil, nil, nil, auditStore, bill, "node-A")
	if done == nil {
		t.Fatal("enabled lifecycle evaluator returned nil channel")
	}

	// The worker runs one pass immediately on start; poll for the
	// observable side effects with a timeout.
	deadline := time.After(2 * time.Second)
	for {
		_, gerr := mans.Get(ctx, key)
		expired := errors.Is(gerr, manifest_store.ErrNotFound)
		_, metered := bill.nodeFor(billing.LifecycleExpirations)
		if expired && auditStore.Len() > 0 && metered {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("lifecycle wiring side effects not observed: expired=%v audits=%d billing=%v",
				expired, auditStore.Len(), bill.dimensions())
		case <-time.After(20 * time.Millisecond):
		}
	}

	if node, _ := bill.nodeFor(billing.LifecycleExpirations); node != "node-A" {
		t.Errorf("billing SourceNodeID = %q, want node-A", node)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle goroutine did not drain after context cancel")
	}
}

// TestLifecycleAuditAdapter_ForwardsToStore verifies the adapter
// forwards an evaluator audit entry verbatim to a compliance store.
func TestLifecycleAuditAdapter_ForwardsToStore(t *testing.T) {
	store := compliance.NewMemoryAuditStore()
	a := &lifecycleAuditAdapter{store: store}
	if err := a.Record(context.Background(), evaluator.AuditEntry{
		TenantID:  "tenant-1",
		Operation: "LifecycleExpiration",
		Bucket:    "bucket-1",
		ObjectKey: "logs/old.txt",
		PieceID:   "p1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if store.Len() != 1 {
		t.Fatalf("audit store Len = %d, want 1", store.Len())
	}
}
