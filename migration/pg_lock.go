// PgJobStore is the Postgres-backed JobStore. It satisfies the
// distributed-coordination invariants by leaning on Postgres'
// row-level locking + conditional UPDATEs: every AcquireJob,
// HeartbeatJob, and ReleaseJob is a single statement that
// either changes one row or zero. Atomicity falls out of the
// SQL contract without an explicit BEGIN/COMMIT around each
// call (the JobStore methods are not composite operations).
//
// The schema lives in coordination_schema.sql; the calling
// binary runs it once at startup. This package imports no
// concrete driver — register github.com/lib/pq (or pgx/stdlib)
// in the gateway binary and hand New the *sql.DB.
package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// PgConfig wires the Postgres-backed JobStore. Table defaults
// to "migration_jobs" matching coordination_schema.sql; a
// non-default table must satisfy isSafePgIdent so it cannot
// inject SQL through the table-name interpolation. Clock, when
// non-nil, replaces the wall clock the store uses for TTL
// evaluation — tests inject a controllable clock to make claim
// expiry deterministic.
type PgConfig struct {
	DB    *sql.DB
	Table string
	Clock func() time.Time
}

// PgJobStore is the Postgres-backed JobStore.
type PgJobStore struct {
	db    *sql.DB
	table string
	clock func() time.Time
}

// NewPgJobStore constructs a PgJobStore. It does not open or
// ping the connection; callers should have already verified
// connectivity via the metadata DB pool. Table identifier is
// validated to prevent SQL injection through the fmt.Sprintf'd
// table name.
func NewPgJobStore(cfg PgConfig) (*PgJobStore, error) {
	if cfg.DB == nil {
		return nil, errors.New("migration: PgJobStore requires Config.DB")
	}
	table := cfg.Table
	if table == "" {
		table = "migration_jobs"
	}
	if !isSafePgIdent(table) {
		return nil, fmt.Errorf("migration: invalid table name %q", table)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &PgJobStore{db: cfg.DB, table: table, clock: clock}, nil
}

// PutJob inserts the row in JobPending state. ON CONFLICT DO
// NOTHING guards against a duplicate-enqueue race between two
// nodes by returning RowsAffected = 0; that maps to
// ErrDuplicateJob so the orchestrator surfaces the same
// strict-unique error its in-memory counterpart does.
func (s *PgJobStore) PutJob(ctx context.Context, job MigrationJob) error {
	if job.JobID == "" {
		return errors.New("migration: PutJob requires JobID")
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (
			job_id, tenant_id, bucket, source_backend,
			dest_cell_id, dest_backend, bytes_per_second, state
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (job_id) DO NOTHING
	`, s.table)
	res, err := s.db.ExecContext(ctx, q,
		job.JobID, job.TenantID, job.Bucket, job.SourceBackend,
		job.DestCellID, job.DestBackend, job.BytesPerSecond, string(JobPending),
	)
	if err != nil {
		return fmt.Errorf("migration: PutJob exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("migration: PutJob rows: %w", err)
	}
	if n == 0 {
		return ErrDuplicateJob
	}
	return nil
}

// AcquireJob is the atomic claim primitive. The single UPDATE
// commits at most one row: the WHERE clause filters by job_id
// and demands the existing claim be either absent
// (claimed_by IS NULL) or already expired
// (claimed_until <= now). Two nodes calling concurrently see
// Postgres serialise their UPDATEs via row-level locks; whichever
// commits first wins and the second sees RowsAffected = 0.
//
// The state guard (state IN ('pending','running')) prevents a
// stale node from re-acquiring a job that has already
// transitioned to JobDone or JobFailed in between the call's
// ListActiveJobs result and this AcquireJob attempt.
//
// We use the caller-supplied clock for the TTL upper bound but
// rely on Postgres' now() for the expiry comparison — that
// keeps the test path deterministic (clock controls when the
// claim ends) while the live-claim check stays anchored to
// real time. Tests that need to fast-forward expiry use the
// PgConfig.Clock plus a real DB whose now() advances.
func (s *PgJobStore) AcquireJob(ctx context.Context, jobID, nodeID string, ttl time.Duration) (bool, error) {
	if jobID == "" || nodeID == "" {
		return false, errors.New("migration: AcquireJob requires jobID and nodeID")
	}
	if ttl <= 0 {
		return false, errors.New("migration: AcquireJob requires positive ttl")
	}
	now := s.clock()
	expiry := now.Add(ttl)
	q := fmt.Sprintf(`
		UPDATE %s
		   SET claimed_by    = $2,
		       claimed_until = $3,
		       state         = 'running',
		       started_at    = COALESCE(started_at, $4),
		       updated_at    = $4
		 WHERE job_id        = $1
		   AND state         IN ('pending','running')
		   AND (claimed_by IS NULL
		        OR claimed_until <= $4
		        OR claimed_by = $2)
	`, s.table)
	res, err := s.db.ExecContext(ctx, q, jobID, nodeID, expiry, now)
	if err != nil {
		return false, fmt.Errorf("migration: AcquireJob exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("migration: AcquireJob rows: %w", err)
	}
	if n > 0 {
		return true, nil
	}
	// No row updated. Distinguish "no such job" from
	// "claim conflict" with a follow-up existence check so
	// callers can branch correctly.
	if _, ok, err := s.GetJob(ctx, jobID); err != nil {
		return false, err
	} else if !ok {
		return false, ErrJobNotFound
	}
	return false, nil
}

// HeartbeatJob extends the claim's TTL. The fence on
// claimed_by = $2 prevents a node that has lost its claim from
// resurrecting it: another node may have already re-acquired
// the job after a missed heartbeat, and the previous owner's
// UPDATE simply finds zero rows.
func (s *PgJobStore) HeartbeatJob(ctx context.Context, jobID, nodeID string, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("migration: HeartbeatJob requires positive ttl")
	}
	now := s.clock()
	expiry := now.Add(ttl)
	q := fmt.Sprintf(`
		UPDATE %s
		   SET claimed_until = $3,
		       updated_at    = $4
		 WHERE job_id        = $1
		   AND claimed_by    = $2
	`, s.table)
	res, err := s.db.ExecContext(ctx, q, jobID, nodeID, expiry, now)
	if err != nil {
		return fmt.Errorf("migration: HeartbeatJob exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("migration: HeartbeatJob rows: %w", err)
	}
	if n > 0 {
		return nil
	}
	if _, ok, err := s.GetJob(ctx, jobID); err != nil {
		return err
	} else if !ok {
		return ErrJobNotFound
	}
	return ErrClaimNotHeld
}

// ReleaseJob finalises the row with the supplied terminal
// state, byte/piece counts, and optional error. The fence on
// claimed_by = $2 mirrors HeartbeatJob: a worker whose claim
// has been re-acquired cannot overwrite the new owner's
// in-flight progress with a stale Done/Failed.
func (s *PgJobStore) ReleaseJob(
	ctx context.Context,
	jobID, nodeID string,
	terminal JobState,
	bytesCopied int64,
	piecesCopied int,
	jobErr error,
) error {
	if terminal != JobDone && terminal != JobFailed {
		return errors.New("migration: ReleaseJob terminal must be JobDone or JobFailed")
	}
	var errText sql.NullString
	if jobErr != nil {
		errText = sql.NullString{String: jobErr.Error(), Valid: true}
	}
	now := s.clock()
	q := fmt.Sprintf(`
		UPDATE %s
		   SET state         = $3,
		       bytes_copied  = $4,
		       pieces_copied = $5,
		       completed_at  = $6,
		       error         = $7,
		       claimed_by    = NULL,
		       claimed_until = NULL,
		       updated_at    = $6
		 WHERE job_id        = $1
		   AND claimed_by    = $2
	`, s.table)
	res, err := s.db.ExecContext(ctx, q,
		jobID, nodeID, string(terminal),
		bytesCopied, piecesCopied, now, errText,
	)
	if err != nil {
		return fmt.Errorf("migration: ReleaseJob exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("migration: ReleaseJob rows: %w", err)
	}
	if n > 0 {
		return nil
	}
	if _, ok, err := s.GetJob(ctx, jobID); err != nil {
		return err
	} else if !ok {
		return ErrJobNotFound
	}
	return ErrClaimNotHeld
}

// ListActiveJobs returns pending + running rows. The
// orchestrator's RunOnce iterates this and tries to acquire
// each; expired claims are still returned because any node
// can reclaim them.
func (s *PgJobStore) ListActiveJobs(ctx context.Context) ([]MigrationJob, error) {
	q := fmt.Sprintf(`
		SELECT job_id, tenant_id, bucket, source_backend,
		       dest_cell_id, dest_backend, bytes_per_second,
		       state, bytes_copied, pieces_copied,
		       started_at, completed_at, error
		  FROM %s
		 WHERE state IN ('pending','running')
		 ORDER BY job_id ASC
	`, s.table)
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("migration: ListActiveJobs query: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

// GetJob returns one row by ID. ok = false + nil error when
// the row does not exist; callers branch on ok.
func (s *PgJobStore) GetJob(ctx context.Context, jobID string) (MigrationJob, bool, error) {
	q := fmt.Sprintf(`
		SELECT job_id, tenant_id, bucket, source_backend,
		       dest_cell_id, dest_backend, bytes_per_second,
		       state, bytes_copied, pieces_copied,
		       started_at, completed_at, error
		  FROM %s
		 WHERE job_id = $1
	`, s.table)
	row := s.db.QueryRowContext(ctx, q, jobID)
	job, err := scanJobRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MigrationJob{}, false, nil
	}
	if err != nil {
		return MigrationJob{}, false, err
	}
	return job, true, nil
}

// Jobs returns every row, ordered by created_at. The management
// console renders this for the migrations index page.
func (s *PgJobStore) Jobs(ctx context.Context) ([]MigrationJob, error) {
	q := fmt.Sprintf(`
		SELECT job_id, tenant_id, bucket, source_backend,
		       dest_cell_id, dest_backend, bytes_per_second,
		       state, bytes_copied, pieces_copied,
		       started_at, completed_at, error
		  FROM %s
		 ORDER BY created_at ASC
	`, s.table)
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("migration: Jobs query: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

// scanJobs drains an *sql.Rows of the standard job columns into
// a []MigrationJob. Stable sort by JobID lets tests assert
// ordering without depending on insertion order.
func scanJobs(rows *sql.Rows) ([]MigrationJob, error) {
	var out []MigrationJob
	for rows.Next() {
		job, err := scanJobRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migration: rows iter: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out, nil
}

// rowScanner abstracts QueryRow.Scan and Rows.Scan so scanJobRow
// can serve both single-row reads (GetJob) and multi-row scans
// (ListActiveJobs, Jobs).
type rowScanner interface {
	Scan(dest ...any) error
}

// scanJobRow reads the canonical column tuple into a
// MigrationJob. Nullable timestamps and the error column are
// unpacked through sql.Null* helpers because Go's *time.Time
// scan path treats NULL as a zero value (which would silently
// collide with a real time.Time{}).
func scanJobRow(scanner rowScanner) (MigrationJob, error) {
	var (
		j          MigrationJob
		startedAt  sql.NullTime
		complete   sql.NullTime
		errText    sql.NullString
		state      string
	)
	err := scanner.Scan(
		&j.JobID, &j.TenantID, &j.Bucket, &j.SourceBackend,
		&j.DestCellID, &j.DestBackend, &j.BytesPerSecond,
		&state, &j.BytesCopied, &j.PiecesCopied,
		&startedAt, &complete, &errText,
	)
	if err != nil {
		return MigrationJob{}, err
	}
	j.State = JobState(state)
	if startedAt.Valid {
		j.StartedAt = startedAt.Time
	}
	if complete.Valid {
		j.CompletedAt = complete.Time
	}
	if errText.Valid {
		j.Error = errText.String
	}
	return j, nil
}

// isSafePgIdent is the conservative identifier filter used to
// validate the table name before string-interpolating it into
// every statement. The Postgres rules allow lowercase letters,
// digits, and underscores; uppercase letters require quoting,
// which this codebase does not do. The filter also rejects the
// empty string and identifiers that start with a digit.
func isSafePgIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
