package migration

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func waitForState(t *testing.T, o *FleetOrchestrator, id string, want JobState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := o.Job(id); ok && j.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s never reached state %q", id, want)
}

func TestFleetOrchestrator_HappyPath(t *testing.T) {
	var ran atomic.Int32
	o := NewFleetOrchestrator(
		[]CellLimits{{CellID: "cell-a", MaxConcurrentJobs: 2}},
		func(_ context.Context, _ MigrationJob) (int64, int, error) {
			ran.Add(1)
			return 1024, 2, nil
		},
	)
	if err := o.Enqueue(MigrationJob{JobID: "j1", TenantID: "T", Bucket: "b", DestCellID: "cell-a"}); err != nil {
		t.Fatal(err)
	}
	o.RunOnce(context.Background())
	waitForState(t, o, "j1", JobDone)
	if ran.Load() != 1 {
		t.Errorf("runner called %d times, want 1", ran.Load())
	}
	j, _ := o.Job("j1")
	if j.BytesCopied != 1024 || j.PiecesCopied != 2 {
		t.Errorf("stats not propagated: %+v", j)
	}
}

func TestFleetOrchestrator_PerCellConcurrencyCap(t *testing.T) {
	var inflight atomic.Int32
	var maxInflight atomic.Int32
	gate := make(chan struct{}, 10) // buffered so release does not block before the worker reads
	o := NewFleetOrchestrator(
		[]CellLimits{{CellID: "cell-a", MaxConcurrentJobs: 1}},
		func(ctx context.Context, _ MigrationJob) (int64, int, error) {
			cur := inflight.Add(1)
			defer inflight.Add(-1)
			for {
				m := maxInflight.Load()
				if cur <= m || maxInflight.CompareAndSwap(m, cur) {
					break
				}
			}
			<-gate
			return 0, 0, nil
		},
	)
	for i := 0; i < 3; i++ {
		_ = o.Enqueue(MigrationJob{JobID: "j" + string(rune('1'+i)), TenantID: "T", Bucket: "b", DestCellID: "cell-a"})
		gate <- struct{}{} // pre-fill release tokens so workers never deadlock
	}
	// Drive the queue forward until every job has settled; each
	// RunOnce dispatches at most one (per-cell cap = 1) and the
	// next call only picks j2 once j1 has transitioned to Done.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		o.RunOnce(context.Background())
		done := 0
		for _, j := range o.Jobs() {
			if j.State == JobDone || j.State == JobFailed {
				done++
			}
		}
		if done == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, id := range []string{"j1", "j2", "j3"} {
		waitForState(t, o, id, JobDone)
	}
	if maxInflight.Load() > 1 {
		t.Errorf("maxInflight=%d, want 1 (cap respected)", maxInflight.Load())
	}
}

func TestFleetOrchestrator_FailedRunnerSetsFailedState(t *testing.T) {
	o := NewFleetOrchestrator(nil, func(_ context.Context, _ MigrationJob) (int64, int, error) {
		return 0, 0, errors.New("boom")
	})
	_ = o.Enqueue(MigrationJob{JobID: "j", TenantID: "T", DestCellID: "c"})
	o.RunOnce(context.Background())
	waitForState(t, o, "j", JobFailed)
	j, _ := o.Job("j")
	if j.Error == "" {
		t.Error("FailedJob must record the error string")
	}
}

func TestFleetOrchestrator_RejectsDuplicateID(t *testing.T) {
	o := NewFleetOrchestrator(nil, nil)
	job := MigrationJob{JobID: "j", TenantID: "T", DestCellID: "c"}
	_ = o.Enqueue(job)
	if err := o.Enqueue(job); err == nil {
		t.Error("duplicate enqueue must error")
	}
}

// TestFleetOrchestrator_DistributedClaim_TwoNodesSingleJob asserts
// the invariant that drives every other multi-gateway property:
// when two FleetOrchestrators sharing one JobStore both call
// RunOnce on the same pending job, exactly one of them dispatches
// the worker. The other sees AcquireJob return (false, nil) and
// silently moves on. Without this, two gateways would each spawn
// their own background_rebalancer pass against the same manifest
// and produce duplicate PutPiece calls — the exact bug v0.1.0
// section 7 was added to prevent.
func TestFleetOrchestrator_DistributedClaim_TwoNodesSingleJob(t *testing.T) {
	store := NewInMemoryJobStore(nil)

	var ranA, ranB atomic.Int32
	nodeA := mustOrch(t, store, "node-a", func(_ context.Context, _ MigrationJob) (int64, int, error) {
		ranA.Add(1)
		return 100, 1, nil
	})
	nodeB := mustOrch(t, store, "node-b", func(_ context.Context, _ MigrationJob) (int64, int, error) {
		ranB.Add(1)
		return 200, 2, nil
	})

	if err := nodeA.Enqueue(MigrationJob{
		JobID: "j-single", TenantID: "T", Bucket: "b",
		DestCellID: "cell-a", SourceBackend: "wasabi", DestBackend: "ceph",
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Both nodes call RunOnce; exactly one acquires.
	nodeA.RunOnce(context.Background())
	nodeB.RunOnce(context.Background())

	waitForState(t, nodeA, "j-single", JobDone)

	totalRuns := ranA.Load() + ranB.Load()
	if totalRuns != 1 {
		t.Fatalf("runner fired %d times (a=%d b=%d), want exactly 1 across both nodes",
			totalRuns, ranA.Load(), ranB.Load())
	}
}

// TestFleetOrchestrator_ClaimExpiryRecovery asserts the
// crash-recovery primitive: a node that claims a job and then
// stops heartbeating (simulating a crash) MUST not block the
// queue forever. After the claim's TTL expires another node
// re-acquires the job and runs it to completion. Without this,
// a single hung gateway would freeze every (tenant, bucket) it
// had claimed at the moment of failure.
func TestFleetOrchestrator_ClaimExpiryRecovery(t *testing.T) {
	// Controllable clock so the test can fast-forward past
	// the claim TTL without sleeping.
	wallClock := time.Now()
	clock := func() time.Time { return wallClock }
	store := NewInMemoryJobStore(clock)

	if err := store.PutJob(context.Background(), MigrationJob{
		JobID: "j-recover", TenantID: "T", Bucket: "b",
		DestCellID: "cell-a", SourceBackend: "wasabi", DestBackend: "ceph",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Node A claims and then "crashes" (we never heartbeat).
	if ok, err := store.AcquireJob(context.Background(), "j-recover", "node-a", 10*time.Second); err != nil || !ok {
		t.Fatalf("node-a acquire: ok=%v err=%v", ok, err)
	}

	// Before TTL expiry, node-b cannot acquire.
	if ok, _ := store.AcquireJob(context.Background(), "j-recover", "node-b", 10*time.Second); ok {
		t.Fatalf("node-b acquired before TTL expiry; claim isolation broken")
	}

	// Fast-forward past the TTL.
	wallClock = wallClock.Add(15 * time.Second)

	// Now node-b can recover the job.
	if ok, err := store.AcquireJob(context.Background(), "j-recover", "node-b", 10*time.Second); err != nil || !ok {
		t.Fatalf("node-b recover after TTL: ok=%v err=%v", ok, err)
	}

	got, _, err := store.GetJob(context.Background(), "j-recover")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != JobRunning {
		t.Fatalf("post-recover state = %q, want JobRunning", got.State)
	}
}

// TestFleetOrchestrator_HeartbeatPreventsExpiry asserts the
// other side of the claim-recovery coin: a node that keeps
// heartbeating MUST retain its claim even past the original
// TTL. Without this, a job that takes longer than the TTL
// would be silently double-executed by a competing node mid-flight.
func TestFleetOrchestrator_HeartbeatPreventsExpiry(t *testing.T) {
	wallClock := time.Now()
	clock := func() time.Time { return wallClock }
	store := NewInMemoryJobStore(clock)

	if err := store.PutJob(context.Background(), MigrationJob{
		JobID: "j-long", TenantID: "T", Bucket: "b",
		DestCellID: "cell-a", SourceBackend: "wasabi", DestBackend: "ceph",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	if ok, _ := store.AcquireJob(context.Background(), "j-long", "node-a", 10*time.Second); !ok {
		t.Fatal("node-a acquire failed")
	}

	// Advance 8s (inside TTL); heartbeat refreshes claim.
	wallClock = wallClock.Add(8 * time.Second)
	if err := store.HeartbeatJob(context.Background(), "j-long", "node-a", 10*time.Second); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	// Advance another 8s (past original TTL but inside the
	// heartbeat-extended one).
	wallClock = wallClock.Add(8 * time.Second)

	// Node-b must not be able to acquire.
	if ok, _ := store.AcquireJob(context.Background(), "j-long", "node-b", 10*time.Second); ok {
		t.Fatal("node-b acquired despite live heartbeat; split-brain risk")
	}
}

// TestFleetOrchestrator_HeartbeatRejectsFencedNode asserts the
// split-brain guard: once node A has lost its claim (TTL
// expired, node B took over) a stale HeartbeatJob from A must
// return ErrClaimNotHeld. This is what makes the heartbeatLoop
// safe — without the fence, a slow node could keep extending
// a claim it no longer owns and stomp on B's writes.
func TestFleetOrchestrator_HeartbeatRejectsFencedNode(t *testing.T) {
	wallClock := time.Now()
	clock := func() time.Time { return wallClock }
	store := NewInMemoryJobStore(clock)

	_ = store.PutJob(context.Background(), MigrationJob{
		JobID: "j-fence", TenantID: "T", DestCellID: "c",
	})
	_, _ = store.AcquireJob(context.Background(), "j-fence", "node-a", 5*time.Second)
	wallClock = wallClock.Add(6 * time.Second) // expire A's claim
	if ok, _ := store.AcquireJob(context.Background(), "j-fence", "node-b", 5*time.Second); !ok {
		t.Fatal("node-b takeover failed")
	}

	if err := store.HeartbeatJob(context.Background(), "j-fence", "node-a", 5*time.Second); !errors.Is(err, ErrClaimNotHeld) {
		t.Fatalf("stale heartbeat err = %v, want ErrClaimNotHeld", err)
	}

	if err := store.ReleaseJob(context.Background(), "j-fence", "node-a", JobDone, 0, 0, nil); !errors.Is(err, ErrClaimNotHeld) {
		t.Fatalf("stale release err = %v, want ErrClaimNotHeld", err)
	}
}

// TestFleetOrchestrator_NodeIDRequired asserts that the
// WithStore constructor rejects an empty NodeID — without a
// distinct identifier the claim-ownership fence is meaningless
// (every gateway looks like every other gateway to the store).
func TestFleetOrchestrator_NodeIDRequired(t *testing.T) {
	store := NewInMemoryJobStore(nil)
	_, err := NewFleetOrchestratorWithStore(FleetOrchestratorConfig{
		Store:  store,
		NodeID: "",
	})
	if err == nil {
		t.Fatal("WithStore must reject empty NodeID")
	}

	_, err = NewFleetOrchestratorWithStore(FleetOrchestratorConfig{
		Store:  nil,
		NodeID: "node-a",
	})
	if err == nil {
		t.Fatal("WithStore must reject nil Store")
	}
}

// mustOrch constructs an orchestrator and fails the test on
// error. Used by the distributed-coordination tests where the
// per-test boilerplate is otherwise four lines of identical
// FleetOrchestratorConfig setup.
func mustOrch(t *testing.T, store JobStore, nodeID string, runner JobRunner) *FleetOrchestrator {
	t.Helper()
	o, err := NewFleetOrchestratorWithStore(FleetOrchestratorConfig{
		Store:  store,
		NodeID: nodeID,
		Limits: []CellLimits{{CellID: "cell-a", MaxConcurrentJobs: 4}},
		Runner: runner,
	})
	if err != nil {
		t.Fatalf("orchestrator: %v", err)
	}
	return o
}
