// Package migration's fleet_orchestrator coordinates many
// concurrent background_rebalancer.Rebalancer instances to drain
// tenants off a legacy backend (typically Wasabi) and onto a
// per-cell local primary.
//
// The orchestrator does NOT implement the rebalance loop itself
// — that lives in migration/background_rebalancer. It only owns
// the queueing, per-cell concurrency caps, and progress reporting
// across many active migrations.
package migration

import (
	"context"
	"errors"
	"sync"
	"time"
)

// MigrationJob is one (tenant, bucket) drain queued against a
// destination cell. The orchestrator's RunOnce method picks up
// pending jobs and dispatches them to a pool of worker
// goroutines bounded by the cell's MaxConcurrentJobs.
type MigrationJob struct {
	JobID          string    `json:"job_id"`
	TenantID       string    `json:"tenant_id"`
	Bucket         string    `json:"bucket"`
	SourceBackend  string    `json:"source_backend"`
	DestCellID     string    `json:"dest_cell_id"`
	DestBackend    string    `json:"dest_backend"`
	BytesPerSecond int64     `json:"bytes_per_second"`
	State          JobState  `json:"state"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	CompletedAt    time.Time `json:"completed_at,omitempty"`
	Error          string    `json:"error,omitempty"`
	BytesCopied    int64     `json:"bytes_copied"`
	PiecesCopied   int       `json:"pieces_copied"`
}

// JobState is the high-level state of a MigrationJob.
type JobState string

const (
	JobPending  JobState = "pending"
	JobRunning  JobState = "running"
	JobDone     JobState = "done"
	JobFailed   JobState = "failed"
)

// CellLimits caps how much concurrent work a single dest-cell
// can absorb. The orchestrator never schedules more than
// MaxConcurrentJobs against the same DestCellID at the same
// instant.
type CellLimits struct {
	CellID            string
	MaxConcurrentJobs int
}

// JobRunner is the function the orchestrator invokes for each
// pending job. The production wiring constructs a
// background_rebalancer.Rebalancer for the (tenant, bucket,
// source, dest) tuple, runs one pass, and returns aggregate
// stats; tests inject a stub.
type JobRunner func(ctx context.Context, job MigrationJob) (bytesCopied int64, piecesCopied int, err error)

// FleetOrchestrator owns the migration queue and per-cell
// concurrency caps.
type FleetOrchestrator struct {
	limits map[string]int
	runner JobRunner

	mu     sync.Mutex
	jobs   map[string]*MigrationJob
	queue  []string // pending job IDs in submission order
}

// NewFleetOrchestrator returns a ready orchestrator.
//
// limits maps DestCellID → MaxConcurrentJobs. Cells absent from
// the map default to 1 concurrent job; setting MaxConcurrentJobs
// to 0 also collapses to 1.
func NewFleetOrchestrator(limits []CellLimits, runner JobRunner) *FleetOrchestrator {
	if runner == nil {
		runner = noopRunner
	}
	o := &FleetOrchestrator{
		limits: make(map[string]int, len(limits)),
		runner: runner,
		jobs:   map[string]*MigrationJob{},
	}
	for _, lim := range limits {
		max := lim.MaxConcurrentJobs
		if max <= 0 {
			max = 1
		}
		o.limits[lim.CellID] = max
	}
	return o
}

func noopRunner(_ context.Context, _ MigrationJob) (int64, int, error) {
	return 0, 0, nil
}

// Enqueue registers a new pending job. JobID must be unique;
// duplicates return an error so callers can detect requeues.
func (o *FleetOrchestrator) Enqueue(job MigrationJob) error {
	if job.JobID == "" || job.TenantID == "" || job.DestCellID == "" {
		return errors.New("fleet_orchestrator: job_id, tenant_id, and dest_cell_id are required")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.jobs[job.JobID]; ok {
		return errors.New("fleet_orchestrator: duplicate job_id")
	}
	job.State = JobPending
	o.jobs[job.JobID] = &job
	o.queue = append(o.queue, job.JobID)
	return nil
}

// Jobs returns a stable snapshot of every job the orchestrator
// has seen, ordered by submission time.
func (o *FleetOrchestrator) Jobs() []MigrationJob {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]MigrationJob, 0, len(o.jobs))
	for _, id := range o.queue {
		if j, ok := o.jobs[id]; ok {
			out = append(out, *j)
		}
	}
	return out
}

// Job returns the current state of a single job.
func (o *FleetOrchestrator) Job(id string) (MigrationJob, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	j, ok := o.jobs[id]
	if !ok {
		return MigrationJob{}, false
	}
	return *j, true
}

// RunOnce drains as many pending jobs as the per-cell limits
// allow. Returns the count of jobs started in this call. Pass
// RunOnce on a ticker to drive sustained progress.
func (o *FleetOrchestrator) RunOnce(ctx context.Context) int {
	picked := o.pickRunnable()
	for _, j := range picked {
		go o.run(ctx, j.JobID)
	}
	return len(picked)
}

// pickRunnable selects the next batch of pending jobs while
// respecting per-cell concurrency caps. It marks them Running
// and updates StartedAt before returning so a follow-up RunOnce
// won't double-pick them.
func (o *FleetOrchestrator) pickRunnable() []MigrationJob {
	o.mu.Lock()
	defer o.mu.Unlock()
	running := map[string]int{}
	for _, j := range o.jobs {
		if j.State == JobRunning {
			running[j.DestCellID]++
		}
	}
	var picked []MigrationJob
	now := time.Now()
	for _, id := range o.queue {
		j, ok := o.jobs[id]
		if !ok || j.State != JobPending {
			continue
		}
		max := o.limits[j.DestCellID]
		if max == 0 {
			max = 1
		}
		if running[j.DestCellID] >= max {
			continue
		}
		j.State = JobRunning
		j.StartedAt = now
		running[j.DestCellID]++
		picked = append(picked, *j)
	}
	return picked
}

// run is the per-job worker.
func (o *FleetOrchestrator) run(ctx context.Context, id string) {
	o.mu.Lock()
	jobCopy, ok := o.jobs[id]
	if !ok {
		o.mu.Unlock()
		return
	}
	snap := *jobCopy
	o.mu.Unlock()

	bytes, pieces, err := o.runner(ctx, snap)

	o.mu.Lock()
	defer o.mu.Unlock()
	j := o.jobs[id]
	j.BytesCopied = bytes
	j.PiecesCopied = pieces
	j.CompletedAt = time.Now()
	if err != nil {
		j.State = JobFailed
		j.Error = err.Error()
	} else {
		j.State = JobDone
	}
}
