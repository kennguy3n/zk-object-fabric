package migration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestInMemoryJobStore_AtomicAcquire is the canonical
// concurrent-acquire stress test. It launches N goroutines all
// attempting to claim the same jobID at the same instant; the
// invariant the JobStore must preserve is exactly-one-winner.
// Without it, two gateway nodes could each spawn a rebalancer
// against the same manifest and produce duplicate PutPiece
// calls — the exact bug the distributed-coordination layer was
// added to prevent.
func TestInMemoryJobStore_AtomicAcquire(t *testing.T) {
	s := NewInMemoryJobStore(nil)
	if err := s.PutJob(context.Background(), MigrationJob{
		JobID: "j-race", TenantID: "T", DestCellID: "cell-a",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	const racers = 32
	var wg sync.WaitGroup
	wg.Add(racers)
	wins := make([]bool, racers)
	for i := 0; i < racers; i++ {
		i := i
		go func() {
			defer wg.Done()
			ok, err := s.AcquireJob(context.Background(), "j-race", "node-"+itoa(i), 10*time.Second)
			if err != nil {
				t.Errorf("racer %d err: %v", i, err)
				return
			}
			wins[i] = ok
		}()
	}
	wg.Wait()

	winners := 0
	for _, w := range wins {
		if w {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("got %d winners, want exactly 1", winners)
	}
}

// TestInMemoryJobStore_TerminalStateIsFinal asserts the
// state-machine invariant: once a job reaches JobDone or
// JobFailed, AcquireJob never picks it up again. This is the
// belt-and-braces protection: the claim-columns reset on
// ReleaseJob and the state guard in AcquireJob both stop a
// terminal row from being re-run.
func TestInMemoryJobStore_TerminalStateIsFinal(t *testing.T) {
	s := NewInMemoryJobStore(nil)
	_ = s.PutJob(context.Background(), MigrationJob{
		JobID: "j-done", TenantID: "T", DestCellID: "cell-a",
	})
	if ok, _ := s.AcquireJob(context.Background(), "j-done", "node-a", time.Second); !ok {
		t.Fatal("initial acquire failed")
	}
	if err := s.ReleaseJob(context.Background(), "j-done", "node-a", JobDone, 1024, 1, nil); err != nil {
		t.Fatalf("release: %v", err)
	}
	ok, err := s.AcquireJob(context.Background(), "j-done", "node-b", time.Second)
	if err != nil {
		t.Fatalf("post-done acquire err: %v", err)
	}
	if ok {
		t.Fatal("acquired terminal job; state guard broken")
	}
}

// TestInMemoryJobStore_FencedHeartbeatAndRelease asserts the
// split-brain guard at the store layer: HeartbeatJob and
// ReleaseJob from a node that does not own the claim return
// ErrClaimNotHeld and do NOT mutate state. The FleetOrchestrator
// relies on this fence to detect a lost claim during a long
// runner pass.
func TestInMemoryJobStore_FencedHeartbeatAndRelease(t *testing.T) {
	clock := time.Now()
	s := NewInMemoryJobStore(func() time.Time { return clock })
	_ = s.PutJob(context.Background(), MigrationJob{JobID: "j-fence", TenantID: "T", DestCellID: "c"})
	_, _ = s.AcquireJob(context.Background(), "j-fence", "node-a", 5*time.Second)
	clock = clock.Add(6 * time.Second)
	_, _ = s.AcquireJob(context.Background(), "j-fence", "node-b", 5*time.Second)

	if err := s.HeartbeatJob(context.Background(), "j-fence", "node-a", 5*time.Second); !errors.Is(err, ErrClaimNotHeld) {
		t.Errorf("stale heartbeat = %v, want ErrClaimNotHeld", err)
	}
	if err := s.ReleaseJob(context.Background(), "j-fence", "node-a", JobDone, 0, 0, nil); !errors.Is(err, ErrClaimNotHeld) {
		t.Errorf("stale release = %v, want ErrClaimNotHeld", err)
	}

	// node-b's claim must still be live.
	got, _, _ := s.GetJob(context.Background(), "j-fence")
	if got.State != JobRunning {
		t.Errorf("node-b state = %q, want JobRunning", got.State)
	}
}

// TestInMemoryJobStore_DuplicateRejected asserts PutJob is
// strict-unique on JobID. Idempotent re-enqueues must construct
// a fresh JobID; the orchestrator's Enqueue surfaces this as
// a hard error.
func TestInMemoryJobStore_DuplicateRejected(t *testing.T) {
	s := NewInMemoryJobStore(nil)
	job := MigrationJob{JobID: "j-dup", TenantID: "T", DestCellID: "c"}
	if err := s.PutJob(context.Background(), job); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := s.PutJob(context.Background(), job); !errors.Is(err, ErrDuplicateJob) {
		t.Fatalf("second put = %v, want ErrDuplicateJob", err)
	}
}

// TestInMemoryJobStore_ListActiveExcludesTerminal asserts the
// active-set returned by ListActiveJobs only contains pending
// + running rows. The orchestrator iterates this set in
// RunOnce; surfacing terminal rows would cause AcquireJob
// calls that always fail and pollute the dispatch loop.
func TestInMemoryJobStore_ListActiveExcludesTerminal(t *testing.T) {
	s := NewInMemoryJobStore(nil)
	_ = s.PutJob(context.Background(), MigrationJob{JobID: "j-pending", TenantID: "T", DestCellID: "c"})
	_ = s.PutJob(context.Background(), MigrationJob{JobID: "j-done", TenantID: "T", DestCellID: "c"})

	_, _ = s.AcquireJob(context.Background(), "j-done", "n", time.Second)
	_ = s.ReleaseJob(context.Background(), "j-done", "n", JobDone, 0, 0, nil)

	active, _ := s.ListActiveJobs(context.Background())
	if len(active) != 1 || active[0].JobID != "j-pending" {
		t.Fatalf("active = %+v, want exactly j-pending", active)
	}
}

// itoa avoids strconv for the very small range the racer test
// produces (0..32). Keeping the test file dependency-light.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := "0123456789"
	out := ""
	for n > 0 {
		out = string(digits[n%10]) + out
		n /= 10
	}
	return out
}
