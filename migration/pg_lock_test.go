package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// pgStoreOrSkip is the Postgres test gate. It opens
// METADATA_DSN (the env var the rest of the codebase's
// Postgres tests use), creates a unique per-test table, runs
// the schema DDL, and returns a PgJobStore wired to it. The
// caller's t.Cleanup drops the table on exit.
//
// When METADATA_DSN is unset, every Postgres test in this file
// skips. CI runs the rest of the suite against an in-process
// SQLite (or other fast mocks) and gets Postgres coverage
// from the integration job; engineers running `go test` on a
// machine without a database get a fast skip rather than a
// confusing connection error.
func pgStoreOrSkip(t *testing.T) (*PgJobStore, func(), *sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("METADATA_DSN")
	if dsn == "" {
		t.Skip("METADATA_DSN not set; skipping PgJobStore tests")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	// Unique per-test table prevents cross-test interference
	// when multiple tests run against the same database.
	tableName := fmt.Sprintf("mig_jobs_%d_%s",
		time.Now().UnixNano(),
		sanitizeIdent(t.Name()),
	)
	if !isSafePgIdent(tableName) {
		t.Fatalf("internal: tableName %q is not safe", tableName)
	}
	schema := strings.ReplaceAll(coordinationSchemaSQL, "migration_jobs", tableName)
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	cleanup := func() {
		if _, err := db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", tableName)); err != nil {
			t.Logf("drop %s: %v", tableName, err)
		}
		_ = db.Close()
	}
	store, err := NewPgJobStore(PgConfig{DB: db, Table: tableName})
	if err != nil {
		cleanup()
		t.Fatalf("NewPgJobStore: %v", err)
	}
	return store, cleanup, db, tableName
}

func sanitizeIdent(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
			out = append(out, byte(r))
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// coordinationSchemaSQL is the embedded DDL the tests run
// against their per-test table. Kept in sync with the
// coordination_schema.sql file by hand because the file is
// short and the embed package is otherwise unused in this
// dir.
const coordinationSchemaSQL = `
CREATE TABLE IF NOT EXISTS migration_jobs (
    job_id            TEXT        NOT NULL PRIMARY KEY,
    tenant_id         TEXT        NOT NULL,
    bucket            TEXT        NOT NULL,
    source_backend    TEXT        NOT NULL,
    dest_cell_id      TEXT        NOT NULL,
    dest_backend      TEXT        NOT NULL,
    bytes_per_second  BIGINT      NOT NULL DEFAULT 0,
    state             TEXT        NOT NULL DEFAULT 'pending',
    claimed_by        TEXT        NULL,
    claimed_until     TIMESTAMPTZ NULL,
    bytes_copied      BIGINT      NOT NULL DEFAULT 0,
    pieces_copied     INT         NOT NULL DEFAULT 0,
    started_at        TIMESTAMPTZ NULL,
    completed_at      TIMESTAMPTZ NULL,
    error             TEXT        NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// TestPgJobStore_AtomicAcquire is the multi-row Postgres
// counterpart of the in-memory racer test. The exactly-one
// invariant must hold even when the contending callers are in
// separate goroutines posting their UPDATEs to the same row
// concurrently. Postgres' row-level lock plus the conditional
// WHERE clause is what makes the SQL contract enforce it.
func TestPgJobStore_AtomicAcquire(t *testing.T) {
	store, cleanup, _, _ := pgStoreOrSkip(t)
	defer cleanup()

	if err := store.PutJob(context.Background(), MigrationJob{
		JobID: "j-race", TenantID: "T", DestCellID: "cell-a",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	const racers = 16
	var wg sync.WaitGroup
	wg.Add(racers)
	wins := make([]bool, racers)
	for i := 0; i < racers; i++ {
		i := i
		go func() {
			defer wg.Done()
			ok, err := store.AcquireJob(context.Background(), "j-race",
				fmt.Sprintf("node-%d", i), 10*time.Second)
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
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

// TestPgJobStore_ClaimExpiryRecovery asserts the
// crash-recovery invariant against a live Postgres. A node
// claims and stops heartbeating; once the claimed_until column
// passes now() another node successfully re-acquires. This is
// the property that lets the orchestrator survive gateway
// process crashes without operator intervention.
func TestPgJobStore_ClaimExpiryRecovery(t *testing.T) {
	store, cleanup, _, _ := pgStoreOrSkip(t)
	defer cleanup()

	_ = store.PutJob(context.Background(), MigrationJob{
		JobID: "j-recover", TenantID: "T", DestCellID: "c",
	})

	// Short TTL so the test does not have to wait long.
	if ok, _ := store.AcquireJob(context.Background(), "j-recover", "node-a", 250*time.Millisecond); !ok {
		t.Fatal("node-a acquire failed")
	}
	if ok, _ := store.AcquireJob(context.Background(), "j-recover", "node-b", 250*time.Millisecond); ok {
		t.Fatal("node-b acquired during live claim")
	}
	// Wait out the TTL.
	time.Sleep(400 * time.Millisecond)
	if ok, err := store.AcquireJob(context.Background(), "j-recover", "node-b", 5*time.Second); err != nil || !ok {
		t.Fatalf("node-b recover: ok=%v err=%v", ok, err)
	}
}

// TestPgJobStore_FencedHeartbeatAndRelease asserts the
// split-brain guard at the Postgres layer. The WHERE clause on
// claimed_by = $2 in HeartbeatJob and ReleaseJob is what makes
// this work; without it a slow node could keep extending a
// claim it has lost.
func TestPgJobStore_FencedHeartbeatAndRelease(t *testing.T) {
	store, cleanup, _, _ := pgStoreOrSkip(t)
	defer cleanup()

	_ = store.PutJob(context.Background(), MigrationJob{JobID: "j-fence", TenantID: "T", DestCellID: "c"})
	_, _ = store.AcquireJob(context.Background(), "j-fence", "node-a", 200*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	_, _ = store.AcquireJob(context.Background(), "j-fence", "node-b", 5*time.Second)

	if err := store.HeartbeatJob(context.Background(), "j-fence", "node-a", 5*time.Second); !errors.Is(err, ErrClaimNotHeld) {
		t.Errorf("stale heartbeat = %v, want ErrClaimNotHeld", err)
	}
	if err := store.ReleaseJob(context.Background(), "j-fence", "node-a", JobDone, 0, 0, nil); !errors.Is(err, ErrClaimNotHeld) {
		t.Errorf("stale release = %v, want ErrClaimNotHeld", err)
	}

	got, _, _ := store.GetJob(context.Background(), "j-fence")
	if got.State != JobRunning {
		t.Errorf("post-fence state = %q, want JobRunning", got.State)
	}
}

// TestPgJobStore_PutJobRejectsDuplicate asserts ON CONFLICT DO
// NOTHING semantics — a second PutJob with the same JobID
// returns ErrDuplicateJob, NOT an opaque pg error.
func TestPgJobStore_PutJobRejectsDuplicate(t *testing.T) {
	store, cleanup, _, _ := pgStoreOrSkip(t)
	defer cleanup()

	job := MigrationJob{JobID: "j-dup", TenantID: "T", DestCellID: "c"}
	if err := store.PutJob(context.Background(), job); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := store.PutJob(context.Background(), job); !errors.Is(err, ErrDuplicateJob) {
		t.Fatalf("second put = %v, want ErrDuplicateJob", err)
	}
}

// TestPgJobStore_TerminalRowExcludedFromActive asserts that
// once a job has been Released the (state IN
// ('pending','running')) filter in ListActiveJobs excludes it.
// The orchestrator's dispatch loop iterates the active set; a
// done row appearing there would cause AcquireJob attempts that
// always fail and pollute the loop.
func TestPgJobStore_TerminalRowExcludedFromActive(t *testing.T) {
	store, cleanup, _, _ := pgStoreOrSkip(t)
	defer cleanup()

	_ = store.PutJob(context.Background(), MigrationJob{JobID: "j-pending", TenantID: "T", DestCellID: "c"})
	_ = store.PutJob(context.Background(), MigrationJob{JobID: "j-done", TenantID: "T", DestCellID: "c"})
	_, _ = store.AcquireJob(context.Background(), "j-done", "n", time.Second)
	_ = store.ReleaseJob(context.Background(), "j-done", "n", JobDone, 100, 1, nil)

	active, err := store.ListActiveJobs(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(active) != 1 || active[0].JobID != "j-pending" {
		t.Fatalf("active = %+v, want exactly j-pending", active)
	}
}

// TestPgJobStore_GetJobAfterRelease asserts that ReleaseJob
// writes the terminal stats (bytes_copied, pieces_copied,
// completed_at, error) and they survive a round trip. Without
// this, the management console would render every completed
// job with zeroes.
func TestPgJobStore_GetJobAfterRelease(t *testing.T) {
	store, cleanup, _, _ := pgStoreOrSkip(t)
	defer cleanup()

	_ = store.PutJob(context.Background(), MigrationJob{JobID: "j-stats", TenantID: "T", DestCellID: "c"})
	_, _ = store.AcquireJob(context.Background(), "j-stats", "n", time.Second)
	if err := store.ReleaseJob(context.Background(), "j-stats", "n", JobFailed, 4096, 2, errors.New("disk full")); err != nil {
		t.Fatalf("release: %v", err)
	}
	got, ok, err := store.GetJob(context.Background(), "j-stats")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.State != JobFailed || got.BytesCopied != 4096 || got.PiecesCopied != 2 || got.Error != "disk full" {
		t.Fatalf("post-release row = %+v", got)
	}
}

// TestPgJobStore_InvalidTableRejected asserts the SQL-injection
// guard rejects table names that would otherwise be string-
// interpolated into every statement.
func TestPgJobStore_InvalidTableRejected(t *testing.T) {
	// No DB required for this test; the validation happens in
	// NewPgJobStore before any query runs, so the rejection
	// suite runs even on machines without METADATA_DSN.
	//
	// Empty string is intentionally NOT in the rejection
	// list — NewPgJobStore treats it as "use the default
	// table name" and falls back to "migration_jobs".
	cases := []string{
		"migration_jobs; DROP TABLE foo",
		"migration jobs",   // space
		"migration_jobs--", // SQL comment
		"123leading_digit",
	}
	for _, name := range cases {
		_, err := NewPgJobStore(PgConfig{DB: &sql.DB{}, Table: name})
		if err == nil {
			t.Errorf("NewPgJobStore accepted unsafe table %q", name)
		}
	}
}
