// JobStore is the distributed-coordination primitive the
// FleetOrchestrator uses to claim, heartbeat, and release
// migration jobs across many gateway nodes. The in-process
// orchestrator that previously held all state in a
// sync.Mutex-guarded map kept correctness only because a single
// gateway process owned the universe — once two or more
// gateways started running side by side (the v0.1.0 production
// shape) the same job_id could be picked up by both because
// nothing arbitrated across the process boundary. JobStore is
// that arbitration: every claim, heartbeat, and release is a
// single atomic write the chosen backend (in-memory for tests
// and single-node deployments, Postgres for production)
// serialises across all callers.
package migration

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrJobNotFound is returned by JobStore.GetJob and the
// claim/heartbeat methods when the given job_id does not exist.
// Distinguishing "no such job" from "claim conflict" matters
// because the recovery paths diverge: a vanished job is a bug
// (the queue lost a row); a claim conflict is the expected
// outcome of healthy contention between gateway nodes.
var ErrJobNotFound = errors.New("migration: job not found")

// ErrClaimNotHeld is returned by HeartbeatJob and ReleaseJob
// when nodeID does not currently own the claim on jobID.
// Callers treat this as a hard signal to abandon the in-flight
// runner: another node has re-acquired the job after a missed
// heartbeat, and continuing to write to manifests under the old
// claim risks double-execution.
var ErrClaimNotHeld = errors.New("migration: claim not held by caller")

// ErrDuplicateJob is returned by PutJob when a job with the
// same ID already exists. The orchestrator surfaces this as a
// duplicate-enqueue error to the management console; idempotent
// re-enqueues must use a different JobID.
var ErrDuplicateJob = errors.New("migration: duplicate job id")

// JobStore is the persistence + coordination surface the
// FleetOrchestrator depends on. Implementations MUST guarantee
// the following invariants:
//
//   - AcquireJob is atomic: two nodes calling it concurrently
//     for the same jobID see exactly one (true, nil) and one
//     (false, nil). Postgres-backed implementations use a
//     conditional UPDATE on (claimed_by IS NULL OR claimed_until
//     < now()); the in-memory implementation uses a mutex.
//
//   - HeartbeatJob is fenced by nodeID: a heartbeat from a node
//     that does NOT own the claim returns ErrClaimNotHeld and
//     does NOT extend the TTL. This is the split-brain
//     prevention primitive — the old owner of a re-acquired job
//     must lose its writes.
//
//   - ReleaseJob is fenced by nodeID: a release from a node
//     that does NOT own the claim returns ErrClaimNotHeld and
//     does NOT transition state. A worker that loses its claim
//     mid-flight discovers it via a failed heartbeat first;
//     ReleaseJob is the final gate.
//
//   - TTL is the recovery primitive: a node that crashes with
//     a job claimed cannot block the queue forever. Once
//     claimed_until passes the wall clock, another node's
//     AcquireJob succeeds. There is no separate "expire" RPC.
type JobStore interface {
	// PutJob persists a new job in JobPending state. Returns
	// ErrDuplicateJob if job.JobID already exists; callers
	// treat that as a hard error (the queue's enqueue contract
	// is strict-unique).
	PutJob(ctx context.Context, job MigrationJob) error

	// AcquireJob attempts to take ownership of jobID for
	// nodeID with a TTL of ttl. Returns (true, nil) on
	// success; the row's state advances to JobRunning, its
	// StartedAt is stamped, and its claim metadata is updated.
	// Returns (false, nil) when another live claim already
	// owns the job. Returns (false, ErrJobNotFound) when
	// jobID does not exist.
	AcquireJob(ctx context.Context, jobID, nodeID string, ttl time.Duration) (bool, error)

	// HeartbeatJob extends the claim's TTL by ttl. Returns
	// ErrClaimNotHeld when nodeID does not currently own the
	// claim, ErrJobNotFound when jobID does not exist.
	HeartbeatJob(ctx context.Context, jobID, nodeID string, ttl time.Duration) error

	// ReleaseJob finalises jobID with the supplied terminal
	// state (JobDone or JobFailed), byte/piece counts, and
	// optional error. Clears the claim columns so the row is
	// in a terminal state and not eligible for re-acquisition.
	// Returns ErrClaimNotHeld when nodeID is not the owner
	// and ErrJobNotFound when jobID does not exist.
	ReleaseJob(
		ctx context.Context,
		jobID, nodeID string,
		terminal JobState,
		bytesCopied int64,
		piecesCopied int,
		jobErr error,
	) error

	// ListActiveJobs returns the current snapshot of jobs in
	// JobPending or JobRunning state. The orchestrator's
	// RunOnce loop calls this to discover what is claimable;
	// the (state, claimed_until) index in the Postgres schema
	// keeps it cheap. Returned slices are sorted by JobID for
	// determinism.
	ListActiveJobs(ctx context.Context) ([]MigrationJob, error)

	// GetJob returns one job by JobID. ok=false + nil error
	// when not found; nil error + non-empty value otherwise.
	GetJob(ctx context.Context, jobID string) (job MigrationJob, ok bool, err error)

	// Jobs returns every job the store has seen. The
	// management console calls this to render the migration
	// queue; production implementations should paginate, but
	// the v0.1.0 surface is small enough that a single SELECT
	// is acceptable.
	Jobs(ctx context.Context) ([]MigrationJob, error)
}

// InMemoryJobStore is the process-local JobStore. It exists
// because (a) the existing single-gateway deployments must keep
// working without a Postgres dependency, and (b) the
// FleetOrchestrator's unit tests want a fast, deterministic
// store they do not have to spin up Postgres for.
//
// The implementation is intentionally simple: a sync.Mutex
// guards a map[jobID]*claimedJob. All four invariants in the
// JobStore docstring are satisfied by holding the mutex across
// the claim check + state update for AcquireJob, HeartbeatJob,
// and ReleaseJob.
//
// Use NewInMemoryJobStore to construct a value; the zero value
// is not usable (the mutex is fine but the map is nil).
type InMemoryJobStore struct {
	mu     sync.Mutex
	jobs   map[string]*claimedJob
	clock  func() time.Time
	queue  []string // submission order for Jobs() / ListActiveJobs()
}

type claimedJob struct {
	job           MigrationJob
	claimedBy     string
	claimedUntil  time.Time
}

// NewInMemoryJobStore returns a ready store. clock, if non-nil,
// is the time source the store uses to evaluate claim TTLs;
// tests pass a controllable clock to make claim-expiry
// transitions deterministic. A nil clock falls back to time.Now.
func NewInMemoryJobStore(clock func() time.Time) *InMemoryJobStore {
	if clock == nil {
		clock = time.Now
	}
	return &InMemoryJobStore{
		jobs:  map[string]*claimedJob{},
		clock: clock,
	}
}

// PutJob persists a new job. The job's State is forced to
// JobPending so callers cannot bypass the claim path by
// submitting a pre-running row.
func (s *InMemoryJobStore) PutJob(_ context.Context, job MigrationJob) error {
	if job.JobID == "" {
		return errors.New("migration: PutJob requires JobID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.jobs[job.JobID]; exists {
		return ErrDuplicateJob
	}
	job.State = JobPending
	s.jobs[job.JobID] = &claimedJob{job: job}
	s.queue = append(s.queue, job.JobID)
	return nil
}

// AcquireJob is the atomic claim primitive. The mutex held
// across the load + condition check + write means two callers
// racing on the same jobID see one true and one false even
// when their AcquireJob invocations interleave.
func (s *InMemoryJobStore) AcquireJob(_ context.Context, jobID, nodeID string, ttl time.Duration) (bool, error) {
	if jobID == "" || nodeID == "" {
		return false, errors.New("migration: AcquireJob requires jobID and nodeID")
	}
	if ttl <= 0 {
		return false, errors.New("migration: AcquireJob requires positive ttl")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.jobs[jobID]
	if !ok {
		return false, ErrJobNotFound
	}
	if entry.job.State != JobPending && entry.job.State != JobRunning {
		return false, nil // already terminal
	}
	now := s.clock()
	if entry.claimedBy != "" && entry.claimedUntil.After(now) && entry.claimedBy != nodeID {
		return false, nil // another live claim
	}
	entry.claimedBy = nodeID
	entry.claimedUntil = now.Add(ttl)
	entry.job.State = JobRunning
	if entry.job.StartedAt.IsZero() {
		entry.job.StartedAt = now
	}
	return true, nil
}

// HeartbeatJob refuses to extend a TTL it does not own. The
// fence on (entry.claimedBy != nodeID) is the split-brain
// prevention primitive: a node whose claim has been re-acquired
// by another node (because its previous heartbeat lapsed past
// the TTL) discovers the loss here and abandons the runner.
func (s *InMemoryJobStore) HeartbeatJob(_ context.Context, jobID, nodeID string, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("migration: HeartbeatJob requires positive ttl")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.jobs[jobID]
	if !ok {
		return ErrJobNotFound
	}
	if entry.claimedBy != nodeID {
		return ErrClaimNotHeld
	}
	entry.claimedUntil = s.clock().Add(ttl)
	return nil
}

// ReleaseJob finalises the job. The claim columns are cleared
// even on JobFailed so a follow-up acquire cannot pick the row
// up again — terminal state is the gate, the claim cleanup is
// belt-and-braces.
func (s *InMemoryJobStore) ReleaseJob(
	_ context.Context,
	jobID, nodeID string,
	terminal JobState,
	bytesCopied int64,
	piecesCopied int,
	jobErr error,
) error {
	if terminal != JobDone && terminal != JobFailed {
		return errors.New("migration: ReleaseJob terminal must be JobDone or JobFailed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.jobs[jobID]
	if !ok {
		return ErrJobNotFound
	}
	if entry.claimedBy != nodeID {
		return ErrClaimNotHeld
	}
	entry.job.State = terminal
	entry.job.BytesCopied = bytesCopied
	entry.job.PiecesCopied = piecesCopied
	entry.job.CompletedAt = s.clock()
	if jobErr != nil {
		entry.job.Error = jobErr.Error()
	}
	entry.claimedBy = ""
	entry.claimedUntil = time.Time{}
	return nil
}

// ListActiveJobs returns the snapshot of pending+running jobs
// in submission order. The orchestrator's RunOnce iterates this
// and tries to acquire each one; expired claims (claimed_until
// in the past) are still listed so any node can attempt to
// reclaim them.
func (s *InMemoryJobStore) ListActiveJobs(_ context.Context) ([]MigrationJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MigrationJob, 0, len(s.jobs))
	for _, id := range s.queue {
		entry, ok := s.jobs[id]
		if !ok {
			continue
		}
		if entry.job.State == JobPending || entry.job.State == JobRunning {
			out = append(out, entry.job)
		}
	}
	return out, nil
}

// GetJob is the single-row read path; the management console
// uses it to render one job's status. The clone keeps callers
// from racing the store's internal map.
func (s *InMemoryJobStore) GetJob(_ context.Context, jobID string) (MigrationJob, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.jobs[jobID]
	if !ok {
		return MigrationJob{}, false, nil
	}
	return entry.job, true, nil
}

// Jobs returns the full submission-ordered snapshot. The
// console renders this for the migrations index page.
func (s *InMemoryJobStore) Jobs(_ context.Context) ([]MigrationJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MigrationJob, 0, len(s.queue))
	for _, id := range s.queue {
		if entry, ok := s.jobs[id]; ok {
			out = append(out, entry.job)
		}
	}
	return out, nil
}
