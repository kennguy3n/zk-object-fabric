package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// SQLiteSinkConfig tunes a SQLiteSink.
type SQLiteSinkConfig struct {
	// DB is the SQLite database the sink writes to. Required.
	// Callers open it via internal/embeddeddb so it shares the
	// single-connection pool with the other embedded stores.
	DB *sql.DB

	// BatchSize is the number of events buffered before a flush.
	// Defaults to 256. A flush is also forced every FlushInterval.
	BatchSize int

	// FlushInterval is the maximum age of the oldest buffered event
	// before a flush is forced. Defaults to 2 seconds.
	FlushInterval time.Duration

	// Logger, when non-nil, receives flush failures.
	Logger *log.Logger

	// Clock, if set, returns the current time. Tests override it.
	Clock func() time.Time
}

// SQLiteSink is the BillingSink for the embedded / single-node
// deployment profile (docker compose up with no ClickHouse). It
// persists raw UsageEvent rows to a local SQLite table and maintains
// an aggregated usage_counters table (summed Delta per tenant,
// bucket, dimension) so quota checks and the console can read
// usage-to-date with a single indexed lookup.
//
// Like ClickHouseSink, Emit never blocks on IO: events are buffered
// in memory and flushed in batches by a background goroutine driven
// by the batch-size and interval triggers. Call Close to drain the
// buffer on shutdown.
type SQLiteSink struct {
	cfg SQLiteSinkConfig

	mu      sync.Mutex
	buf     []UsageEvent
	timer   *time.Timer
	closed  bool
	running sync.WaitGroup

	failures uint64
	flushed  uint64
}

// NewSQLiteSink validates the config, creates the backing tables if
// needed, and returns a sink ready for use.
func NewSQLiteSink(cfg SQLiteSinkConfig) (*SQLiteSink, error) {
	if cfg.DB == nil {
		return nil, errors.New("billing: sqlite sink requires a non-nil *sql.DB")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 256
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 2 * time.Second
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	s := &SQLiteSink{cfg: cfg}
	if err := s.ensureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteSink) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS usage_events (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			tenant_id     TEXT    NOT NULL,
			bucket        TEXT    NOT NULL,
			dimension     TEXT    NOT NULL,
			delta         INTEGER NOT NULL,
			observed_at   TIMESTAMP NOT NULL,
			source_node_id TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_events_tenant
			ON usage_events (tenant_id, bucket, dimension)`,
		`CREATE TABLE IF NOT EXISTS usage_counters (
			tenant_id  TEXT    NOT NULL,
			bucket     TEXT    NOT NULL,
			dimension  TEXT    NOT NULL,
			total      INTEGER NOT NULL DEFAULT 0,
			updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY (tenant_id, bucket, dimension)
		)`,
	}
	for _, q := range stmts {
		if _, err := s.cfg.DB.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("billing: ensure sqlite sink schema: %w", err)
		}
	}
	return nil
}

// Emit buffers event. It never blocks the caller on disk IO: flushes
// run in a goroutine driven by the batch-size and interval triggers.
func (s *SQLiteSink) Emit(event UsageEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if event.ObservedAt.IsZero() {
		event.ObservedAt = s.cfg.Clock()
	}
	s.buf = append(s.buf, event)
	needFlush := len(s.buf) >= s.cfg.BatchSize
	if s.timer == nil && !needFlush {
		s.timer = time.AfterFunc(s.cfg.FlushInterval, s.intervalFlush)
	}
	s.mu.Unlock()
	if needFlush {
		s.scheduleFlush()
	}
}

// intervalFlush is the time-driven flush trigger.
func (s *SQLiteSink) intervalFlush() {
	s.scheduleFlush()
}

// scheduleFlush detaches the current buffer and writes it in a
// background goroutine so the caller (Emit or the interval timer) is
// never blocked on disk IO.
func (s *SQLiteSink) scheduleFlush() {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if len(s.buf) == 0 {
		s.mu.Unlock()
		return
	}
	batch := s.buf
	s.buf = nil
	s.running.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.running.Done()
		s.writeBatch(context.Background(), batch)
	}()
}

// writeBatch persists batch and records flush/failure counters. It
// does not hold s.mu during the disk write, so it is safe to call
// from a background goroutine (scheduleFlush) or synchronously (Close
// / Flush).
func (s *SQLiteSink) writeBatch(ctx context.Context, batch []UsageEvent) {
	if len(batch) == 0 {
		return
	}
	if err := s.persist(ctx, batch); err != nil {
		s.mu.Lock()
		s.failures++
		s.mu.Unlock()
		if s.cfg.Logger != nil {
			s.cfg.Logger.Printf("billing: sqlite sink flush failed (%d events): %v", len(batch), err)
		}
		return
	}
	s.mu.Lock()
	s.flushed += uint64(len(batch))
	s.mu.Unlock()
}

// persist writes the raw events and updates the aggregated counters
// in a single transaction so a reader never sees a counter that is
// out of step with the events that produced it.
func (s *SQLiteSink) persist(ctx context.Context, batch []UsageEvent) error {
	tx, err := s.cfg.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const insertEvent = `
		INSERT INTO usage_events (tenant_id, bucket, dimension, delta, observed_at, source_node_id)
		VALUES (?, ?, ?, ?, ?, NULLIF(?, ''))`
	const upsertCounter = `
		INSERT INTO usage_counters (tenant_id, bucket, dimension, total, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, bucket, dimension)
		DO UPDATE SET total = total + excluded.total, updated_at = excluded.updated_at`

	for _, e := range batch {
		if _, err := tx.ExecContext(ctx, insertEvent, e.TenantID, e.Bucket, string(e.Dimension), e.Delta, e.ObservedAt, e.SourceNodeID); err != nil {
			return fmt.Errorf("insert event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, upsertCounter, e.TenantID, e.Bucket, string(e.Dimension), e.Delta, e.ObservedAt); err != nil {
			return fmt.Errorf("upsert counter: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Total returns the summed Delta for (tenantID, bucket, dimension)
// from the aggregated counters table. A dimension with no events
// returns (0, nil). Used by quota enforcement and console usage
// displays.
func (s *SQLiteSink) Total(ctx context.Context, tenantID, bucket string, dimension Dimension) (uint64, error) {
	const q = `SELECT total FROM usage_counters WHERE tenant_id = ? AND bucket = ? AND dimension = ?`
	var total int64
	err := s.cfg.DB.QueryRowContext(ctx, q, tenantID, bucket, string(dimension)).Scan(&total)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("billing: read counter: %w", err)
	}
	if total < 0 {
		total = 0
	}
	return uint64(total), nil
}

// TenantTotal returns the summed Delta for (tenantID, dimension)
// across every bucket. Used for tenant-wide quota checks (e.g. total
// storage bytes).
func (s *SQLiteSink) TenantTotal(ctx context.Context, tenantID string, dimension Dimension) (uint64, error) {
	const q = `SELECT COALESCE(SUM(total), 0) FROM usage_counters WHERE tenant_id = ? AND dimension = ?`
	var total int64
	if err := s.cfg.DB.QueryRowContext(ctx, q, tenantID, string(dimension)).Scan(&total); err != nil {
		return 0, fmt.Errorf("billing: read tenant counter: %w", err)
	}
	if total < 0 {
		total = 0
	}
	return uint64(total), nil
}

// TenantUsage returns the summed Delta per dimension for tenantID
// over the half-open period [start, end), read from the raw
// usage_events rows. The returned map is keyed by the dimension
// string. It satisfies the console's UsageQuery interface so the
// embedded profile can drive the console usage page without
// ClickHouse.
//
// Callers that need an up-to-the-event total should Flush first;
// otherwise events still buffered in memory are not yet reflected.
func (s *SQLiteSink) TenantUsage(ctx context.Context, tenantID string, start, end time.Time) (map[string]uint64, error) {
	const q = `
		SELECT dimension, COALESCE(SUM(delta), 0)
		FROM usage_events
		WHERE tenant_id = ? AND observed_at >= ? AND observed_at < ?
		GROUP BY dimension`
	rows, err := s.cfg.DB.QueryContext(ctx, q, tenantID, start, end)
	if err != nil {
		return nil, fmt.Errorf("billing: tenant usage: %w", err)
	}
	defer rows.Close()
	out := make(map[string]uint64)
	for rows.Next() {
		var dim string
		var total int64
		if err := rows.Scan(&dim, &total); err != nil {
			return nil, fmt.Errorf("billing: tenant usage scan: %w", err)
		}
		if total < 0 {
			total = 0
		}
		out[dim] = uint64(total)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("billing: tenant usage iter: %w", err)
	}
	return out, nil
}

// Flush forces a synchronous flush of any buffered events and waits
// for in-flight background flushes to finish. Mainly used by tests
// that need to read counters immediately after Emit.
func (s *SQLiteSink) Flush(ctx context.Context) {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	batch := s.buf
	s.buf = nil
	s.mu.Unlock()
	s.writeBatch(ctx, batch)
	s.running.Wait()
}

// Close drains any buffered events and stops the sink. After Close
// returns, subsequent Emit calls are no-ops. It does not close the
// underlying *sql.DB, which is owned by the caller.
func (s *SQLiteSink) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.running.Wait()
		return nil
	}
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	batch := s.buf
	s.buf = nil
	s.mu.Unlock()

	s.writeBatch(ctx, batch)
	s.running.Wait()
	return nil
}

var _ BillingSink = (*SQLiteSink)(nil)
