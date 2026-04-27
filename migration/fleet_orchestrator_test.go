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
