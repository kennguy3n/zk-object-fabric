// Package migration's fleet_orchestrator coordinates many
// concurrent background_rebalancer.Rebalancer instances to drain
// tenants off a legacy backend (typically Wasabi) and onto a
// per-cell local primary.
//
// The orchestrator does NOT implement the rebalance loop itself
// — that lives in migration/background_rebalancer. It only owns
// the queueing, per-cell concurrency caps, claim coordination
// across gateway nodes, and progress reporting across many
// active migrations.
//
// Distributed coordination: every claim/heartbeat/release goes
// through a JobStore (in-memory for single-node deployments,
// Postgres-backed for multi-gateway production). The store is
// the canonical source of truth; the orchestrator carries no
// in-process state besides the per-cell concurrency tracker
// (an ephemeral counter derived from the store's running set).
package migration

import (
	"context"
	"errors"
	"log"
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
	JobPending JobState = "pending"
	JobRunning JobState = "running"
	JobDone    JobState = "done"
	JobFailed  JobState = "failed"
)

// CellLimits caps how much concurrent work a single dest-cell
// can absorb. The orchestrator never schedules more than
// MaxConcurrentJobs against the same DestCellID at the same
// instant, ACROSS the entire fleet — the JobStore's
// ListActiveJobs is the source of truth for the running set,
// which is what makes the cap a global rather than per-node
// guarantee.
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

// defaultClaimTTL is the wall-clock window the orchestrator
// installs on every AcquireJob. Picked so a crashed node's
// jobs are recoverable within a minute without flooding the
// JobStore with heartbeat traffic on busy fleets. Configurable
// via FleetOrchestratorConfig.ClaimTTL; the worker heartbeats
// at half this interval.
const defaultClaimTTL = 30 * time.Second

// FleetOrchestratorConfig wires a FleetOrchestrator. Store and
// NodeID are required; the rest are optional with sensible
// defaults so existing callers (the single-node in-process
// shape) keep working with one extra struct field instead of a
// large refactor.
type FleetOrchestratorConfig struct {
	// Store is the JobStore backing the queue. Use
	// InMemoryJobStore for tests and single-node deployments,
	// PgJobStore for multi-gateway production.
	Store JobStore

	// NodeID identifies this gateway instance for claim
	// ownership. Two gateways MUST NOT share a NodeID;
	// cmd/gateway defaults to the OS hostname when the
	// config field is empty.
	NodeID string

	// Limits caps per-cell concurrency. Cells absent from the
	// list default to 1; explicit MaxConcurrentJobs <= 0
	// collapses to 1.
	Limits []CellLimits

	// Runner is the per-job dispatch function. Required.
	Runner JobRunner

	// ClaimTTL is the wall-clock window installed on every
	// AcquireJob. Zero defaults to defaultClaimTTL (30s).
	// Heartbeats fire at ClaimTTL/2.
	ClaimTTL time.Duration

	// Logger receives structured progress lines. Nil suppresses.
	Logger *log.Logger
}

// FleetOrchestrator owns the migration queue and per-cell
// concurrency caps. State lives in the JobStore; this struct
// carries only the per-process dispatch loop.
type FleetOrchestrator struct {
	store    JobStore
	nodeID   string
	limits   map[string]int
	runner   JobRunner
	claimTTL time.Duration
	logger   *log.Logger

	// mu guards inflight (the set of jobs this node is
	// currently running a goroutine for). It is decoupled
	// from JobStore's atomicity guarantees: inflight is a
	// per-process bookkeeping aid that lets RunOnce skip jobs
	// it has already dispatched without paying a round trip
	// to the store on every pick.
	mu       sync.Mutex
	inflight map[string]struct{}
}

// NewFleetOrchestratorWithStore is the v0.1.0-and-later
// constructor. The earlier NewFleetOrchestrator signature is
// preserved (in a thin wrapper) so existing single-node code
// keeps compiling, but new wiring should call this one.
func NewFleetOrchestratorWithStore(cfg FleetOrchestratorConfig) (*FleetOrchestrator, error) {
	if cfg.Store == nil {
		return nil, errors.New("fleet_orchestrator: FleetOrchestratorConfig.Store is required")
	}
	if cfg.NodeID == "" {
		return nil, errors.New("fleet_orchestrator: FleetOrchestratorConfig.NodeID is required")
	}
	runner := cfg.Runner
	if runner == nil {
		runner = noopRunner
	}
	ttl := cfg.ClaimTTL
	if ttl <= 0 {
		ttl = defaultClaimTTL
	}
	limits := make(map[string]int, len(cfg.Limits))
	for _, lim := range cfg.Limits {
		max := lim.MaxConcurrentJobs
		if max <= 0 {
			max = 1
		}
		limits[lim.CellID] = max
	}
	return &FleetOrchestrator{
		store:    cfg.Store,
		nodeID:   cfg.NodeID,
		limits:   limits,
		runner:   runner,
		claimTTL: ttl,
		logger:   cfg.Logger,
		inflight: map[string]struct{}{},
	}, nil
}

// NewFleetOrchestrator preserves the v0.0.x signature. It
// constructs an InMemoryJobStore-backed orchestrator with a
// fixed nodeID of "single-node" so single-gateway deployments
// keep building without a config change. New callers should
// use NewFleetOrchestratorWithStore.
func NewFleetOrchestrator(limits []CellLimits, runner JobRunner) *FleetOrchestrator {
	o, err := NewFleetOrchestratorWithStore(FleetOrchestratorConfig{
		Store:  NewInMemoryJobStore(nil),
		NodeID: "single-node",
		Limits: limits,
		Runner: runner,
	})
	if err != nil {
		// Construction can only fail when Store or NodeID
		// are missing; the inlined values above mean this
		// path is unreachable. Panic-on-impossible keeps
		// the legacy signature error-free.
		panic("fleet_orchestrator: legacy constructor invariant broken: " + err.Error())
	}
	return o
}

func noopRunner(_ context.Context, _ MigrationJob) (int64, int, error) {
	return 0, 0, nil
}

// Enqueue registers a new pending job. JobID must be unique;
// duplicates return ErrDuplicateJob so callers can detect
// requeues.
func (o *FleetOrchestrator) Enqueue(job MigrationJob) error {
	if job.JobID == "" || job.TenantID == "" || job.DestCellID == "" {
		return errors.New("fleet_orchestrator: job_id, tenant_id, and dest_cell_id are required")
	}
	return o.store.PutJob(context.Background(), job)
}

// Jobs returns a snapshot of every job the orchestrator has
// seen, ordered by submission time. Used by the management
// console's GET /api/v1/migrations endpoint.
func (o *FleetOrchestrator) Jobs() []MigrationJob {
	jobs, err := o.store.Jobs(context.Background())
	if err != nil {
		if o.logger != nil {
			o.logger.Printf("fleet_orchestrator: Jobs list failed: %v", err)
		}
		return nil
	}
	return jobs
}

// Job returns the current state of a single job.
func (o *FleetOrchestrator) Job(id string) (MigrationJob, bool) {
	job, ok, err := o.store.GetJob(context.Background(), id)
	if err != nil {
		if o.logger != nil {
			o.logger.Printf("fleet_orchestrator: GetJob(%q) failed: %v", id, err)
		}
		return MigrationJob{}, false
	}
	return job, ok
}

// RunOnce drains as many runnable jobs as the per-cell limits
// allow. Returns the count of jobs newly dispatched in this
// call. Pass RunOnce on a ticker to drive sustained progress.
//
// The implementation queries the JobStore for active jobs,
// counts how many of them this node has currently in flight
// against each cell, and AcquireJob's the remaining pending
// jobs in submission order. Successful acquisitions spawn a
// worker goroutine via run().
func (o *FleetOrchestrator) RunOnce(ctx context.Context) int {
	active, err := o.store.ListActiveJobs(ctx)
	if err != nil {
		if o.logger != nil {
			o.logger.Printf("fleet_orchestrator: ListActiveJobs failed: %v", err)
		}
		return 0
	}
	o.mu.Lock()
	running := map[string]int{}
	for _, j := range active {
		if j.State != JobRunning {
			continue
		}
		// Only this node's inflight jobs contribute to the
		// per-cell counter from the dispatch loop's
		// perspective; jobs claimed by other nodes count
		// too because the CellLimits are a global cap, not
		// a per-node cap.
		running[j.DestCellID]++
	}
	o.mu.Unlock()

	dispatched := 0
	for _, j := range active {
		if j.State != JobPending {
			continue
		}
		max := o.cellLimit(j.DestCellID)
		if running[j.DestCellID] >= max {
			continue
		}
		ok, err := o.store.AcquireJob(ctx, j.JobID, o.nodeID, o.claimTTL)
		if err != nil {
			if o.logger != nil {
				o.logger.Printf("fleet_orchestrator: AcquireJob(%q): %v", j.JobID, err)
			}
			continue
		}
		if !ok {
			continue // another node grabbed it first
		}
		running[j.DestCellID]++
		dispatched++
		o.mu.Lock()
		o.inflight[j.JobID] = struct{}{}
		o.mu.Unlock()
		go o.run(ctx, j.JobID)
	}
	return dispatched
}

// cellLimit returns the configured max-concurrent for the
// given cell, falling back to 1 when the cell is not in the
// limits map.
func (o *FleetOrchestrator) cellLimit(cellID string) int {
	max := o.limits[cellID]
	if max == 0 {
		max = 1
	}
	return max
}

// run is the per-job worker. It starts a heartbeat goroutine
// that keeps the claim alive, invokes the runner, and writes
// the terminal state through ReleaseJob.
func (o *FleetOrchestrator) run(ctx context.Context, id string) {
	defer func() {
		o.mu.Lock()
		delete(o.inflight, id)
		o.mu.Unlock()
	}()

	job, ok, err := o.store.GetJob(ctx, id)
	if err != nil {
		if o.logger != nil {
			o.logger.Printf("fleet_orchestrator: run GetJob(%q): %v", id, err)
		}
		return
	}
	if !ok {
		if o.logger != nil {
			o.logger.Printf("fleet_orchestrator: run job(%q) vanished", id)
		}
		return
	}

	// Heartbeat at half the claim TTL so a single missed
	// tick does not lose the claim. The goroutine exits when
	// the parent context cancels OR the runner returns and
	// signals doneCh.
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	doneCh := make(chan struct{})
	go o.heartbeatLoop(heartbeatCtx, id, doneCh)

	bytes, pieces, runErr := o.runner(ctx, job)
	close(doneCh)
	cancelHeartbeat()

	terminal := JobDone
	if runErr != nil {
		terminal = JobFailed
	}
	if err := o.store.ReleaseJob(ctx, id, o.nodeID, terminal, bytes, pieces, runErr); err != nil {
		if errors.Is(err, ErrClaimNotHeld) && o.logger != nil {
			// The claim was lost mid-flight (another node
			// already re-acquired and possibly completed).
			// Log loudly — this should not happen on a
			// healthy fleet but the heartbeat backstop is
			// our last line of defence.
			o.logger.Printf("fleet_orchestrator: ReleaseJob(%q) found claim lost; another node owns the row", id)
		} else if o.logger != nil {
			o.logger.Printf("fleet_orchestrator: ReleaseJob(%q): %v", id, err)
		}
	}
}

// heartbeatLoop extends the claim's TTL at half-period. It
// exits on context cancellation OR when doneCh closes (the
// runner has finished and is about to call ReleaseJob, which
// resets claim_until anyway). A HeartbeatJob that fails with
// ErrClaimNotHeld means the claim has been re-acquired by
// another node; the loop exits early so we do not extend a
// claim we no longer own.
func (o *FleetOrchestrator) heartbeatLoop(ctx context.Context, id string, doneCh <-chan struct{}) {
	interval := o.claimTTL / 2
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-doneCh:
			return
		case <-ticker.C:
			if err := o.store.HeartbeatJob(ctx, id, o.nodeID, o.claimTTL); err != nil {
				if errors.Is(err, ErrClaimNotHeld) {
					if o.logger != nil {
						o.logger.Printf("fleet_orchestrator: heartbeat lost claim on %q; abandoning", id)
					}
					return
				}
				if errors.Is(err, ErrJobNotFound) {
					return
				}
				if o.logger != nil {
					o.logger.Printf("fleet_orchestrator: heartbeat(%q) error: %v", id, err)
				}
			}
		}
	}
}
