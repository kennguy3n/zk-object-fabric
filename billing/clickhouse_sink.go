package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ClickHouseConfig tunes a ClickHouseSink.
type ClickHouseConfig struct {
	// Endpoint is the base URL of the ClickHouse HTTP interface
	// (e.g. "https://clickhouse.internal:8443"). Required.
	Endpoint string

	// Database is the ClickHouse database that owns the usage
	// events table. Required.
	Database string

	// Table is the target table. Defaults to "usage_events".
	Table string

	// Username / Password are optional basic-auth credentials. When
	// both are empty the sink issues unauthenticated requests
	// (suitable for private networks where ClickHouse sits behind
	// the service mesh).
	Username string
	Password string

	// BatchSize is the number of events buffered before a flush.
	// Defaults to 500. A flush is also forced every FlushInterval.
	BatchSize int

	// FlushInterval is the maximum age of the oldest buffered event
	// before a flush is forced. Defaults to 5 seconds.
	FlushInterval time.Duration

	// RequestTimeout bounds each HTTP request. Defaults to 10
	// seconds.
	RequestTimeout time.Duration

	// MaxRetries bounds retry attempts on transient failures.
	// Defaults to 3. Each retry backs off exponentially starting
	// at RetryBase.
	MaxRetries int
	RetryBase  time.Duration

	// HTTPClient, if set, replaces the default *http.Client. Tests
	// use this to point at an httptest server.
	HTTPClient *http.Client

	// Logger, when non-nil, receives flush failures.
	Logger *log.Logger

	// Clock, if set, returns the current time. Tests override it.
	Clock func() time.Time
}

// ClickHouseSink is a production BillingSink that ingests
// UsageEvent rows into ClickHouse over the HTTP interface using the
// JSONEachRow input format. Events are buffered in memory and
// flushed in batches to amortize connection + parsing cost.
//
// Deployments should front this with the standard ClickHouse
// replicated table pattern (MergeTree → ReplicatedMergeTree with a
// Distributed table in front) so a single-node sink failure does
// not lose events.
type ClickHouseSink struct {
	cfg ClickHouseConfig

	mu      sync.Mutex
	buf     []UsageEvent
	timer   *time.Timer
	closed  bool
	running sync.WaitGroup

	failures uint64
	flushed  uint64
}

// NewClickHouseSink validates the config and returns a sink ready
// for use.
func NewClickHouseSink(cfg ClickHouseConfig) (*ClickHouseSink, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("billing: clickhouse endpoint is required")
	}
	if _, err := url.Parse(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("billing: invalid clickhouse endpoint: %w", err)
	}
	if cfg.Database == "" {
		return nil, errors.New("billing: clickhouse database is required")
	}
	if cfg.Table == "" {
		cfg.Table = "usage_events"
	}
	if !strings.HasSuffix(cfg.Table, "_events") {
		return nil, fmt.Errorf("billing: clickhouse table %q must end in \"_events\" (the companion counters table is derived from this suffix)", cfg.Table)
	}
	if !validIdent(cfg.Table) {
		return nil, fmt.Errorf("billing: clickhouse table name %q contains invalid characters", cfg.Table)
	}
	if !validIdent(cfg.Database) {
		return nil, fmt.Errorf("billing: clickhouse database name %q contains invalid characters", cfg.Database)
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 500
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	} else if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 200 * time.Millisecond
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.RequestTimeout}
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &ClickHouseSink{cfg: cfg}, nil
}

// Emit buffers event. It never blocks the caller on a network IO:
// flushes run in a goroutine driven by the batch-size and interval
// triggers.
func (s *ClickHouseSink) Emit(event UsageEvent) {
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

// Close drains any buffered events and stops the sink. After Close
// returns, subsequent Emit calls are no-ops.
func (s *ClickHouseSink) Close(ctx context.Context) error {
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
	batch := s.takeBufLocked()
	s.mu.Unlock()

	s.running.Wait()
	if len(batch) == 0 {
		return nil
	}
	return s.flushBatch(ctx, batch)
}

// Stats returns lifetime counters for the sink. Primarily useful
// for tests and operator dashboards.
func (s *ClickHouseSink) Stats() (flushed, failed uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushed, s.failures
}

// intervalFlush runs when the batch interval fires. It forwards to
// scheduleFlush but wipes the timer first so the next Emit can arm
// a fresh one.
func (s *ClickHouseSink) intervalFlush() {
	s.mu.Lock()
	s.timer = nil
	s.mu.Unlock()
	s.scheduleFlush()
}

// scheduleFlush snapshots the current buffer and triggers a
// background flush. It is safe to call multiple times concurrently.
func (s *ClickHouseSink) scheduleFlush() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	batch := s.takeBufLocked()
	if len(batch) == 0 {
		s.mu.Unlock()
		return
	}
	s.running.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.running.Done()
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.RequestTimeout*time.Duration(s.cfg.MaxRetries+1))
		defer cancel()
		if err := s.flushBatch(ctx, batch); err != nil {
			s.logf("billing: clickhouse flush failed after %d retries: %v", s.cfg.MaxRetries, err)
		}
	}()
}

func (s *ClickHouseSink) takeBufLocked() []UsageEvent {
	if len(s.buf) == 0 {
		return nil
	}
	batch := s.buf
	s.buf = nil
	return batch
}

// flushBatch serialises events as JSONEachRow and POSTs them to
// ClickHouse. The request is retried with exponential backoff on
// transient (5xx / network) failures.
func (s *ClickHouseSink) flushBatch(ctx context.Context, batch []UsageEvent) error {
	if len(batch) == 0 {
		return nil
	}
	body, err := encodeBatch(batch)
	if err != nil {
		s.recordFailure(uint64(len(batch)))
		return err
	}
	insertURL, err := s.insertURL()
	if err != nil {
		s.recordFailure(uint64(len(batch)))
		return err
	}

	var lastErr error
	attempt := 0
	for attempt <= s.cfg.MaxRetries {
		if err := ctx.Err(); err != nil {
			s.recordFailure(uint64(len(batch)))
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, insertURL, bytes.NewReader(body))
		if err != nil {
			s.recordFailure(uint64(len(batch)))
			return fmt.Errorf("billing: build clickhouse request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		if s.cfg.Username != "" || s.cfg.Password != "" {
			req.SetBasicAuth(s.cfg.Username, s.cfg.Password)
		}
		resp, err := s.cfg.HTTPClient.Do(req)
		if err != nil {
			lastErr = err
		} else {
			discardAndClose(resp.Body)
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				s.recordSuccess(uint64(len(batch)))
				return nil
			}
			if resp.StatusCode < 500 {
				lastErr = fmt.Errorf("clickhouse returned %s", resp.Status)
				s.recordFailure(uint64(len(batch)))
				return lastErr
			}
			lastErr = fmt.Errorf("clickhouse 5xx: %s", resp.Status)
		}
		attempt++
		if attempt > s.cfg.MaxRetries {
			break
		}
		backoff := s.cfg.RetryBase * (1 << (attempt - 1))
		select {
		case <-ctx.Done():
			s.recordFailure(uint64(len(batch)))
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	s.recordFailure(uint64(len(batch)))
	return lastErr
}

func (s *ClickHouseSink) recordSuccess(n uint64) {
	s.mu.Lock()
	s.flushed += n
	s.mu.Unlock()
}

func (s *ClickHouseSink) recordFailure(n uint64) {
	s.mu.Lock()
	s.failures += n
	s.mu.Unlock()
}

// insertURL builds the ClickHouse HTTP URL with the INSERT ... FORMAT
// JSONEachRow query inlined. ClickHouse requires the query as a
// URL parameter; the body carries rows.
func (s *ClickHouseSink) insertURL() (string, error) {
	u, err := url.Parse(s.cfg.Endpoint)
	if err != nil {
		return "", fmt.Errorf("billing: parse clickhouse endpoint: %w", err)
	}
	q := u.Query()
	q.Set("database", s.cfg.Database)
	// The table name is validated by NewClickHouseSink to be a
	// simple [A-Za-z0-9_]+ identifier, but backtick-quote it
	// anyway so this query survives future loosening of that
	// check (e.g. to allow a database-qualified `db`.`table`).
	q.Set("query", fmt.Sprintf("INSERT INTO `%s` FORMAT JSONEachRow", s.cfg.Table))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validIdent accepts a conservative subset of SQL identifiers —
// letters, digits, and underscore. ClickHouse is more permissive
// (backticks allow arbitrary strings) but restricting the allowed
// set keeps the INSERT query safe from injection by misconfiguration
// and makes the SchemaDDL output predictable.
func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_':
		default:
			return false
		}
	}
	return true
}

// clickHouseRow is the on-the-wire shape ClickHouse ingests. Column
// names match billing/schema.sql.
type clickHouseRow struct {
	TenantID   string `json:"tenant_id"`
	Bucket     string `json:"bucket"`
	Dimension  string `json:"dimension"`
	Delta      uint64 `json:"delta"`
	ObservedAt string `json:"observed_at"`
	SourceNode string `json:"source_node_id,omitempty"`
}

func encodeBatch(batch []UsageEvent) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, e := range batch {
		row := clickHouseRow{
			TenantID:   e.TenantID,
			Bucket:     e.Bucket,
			Dimension:  string(e.Dimension),
			Delta:      e.Delta,
			ObservedAt: e.ObservedAt.UTC().Format("2006-01-02 15:04:05.000"),
			SourceNode: e.SourceNodeID,
		}
		if err := enc.Encode(&row); err != nil {
			return nil, fmt.Errorf("billing: encode clickhouse row: %w", err)
		}
	}
	// ClickHouse JSONEachRow tolerates both newline- and
	// concatenation-delimited rows; the encoder writes the newline
	// terminator already.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func (s *ClickHouseSink) logf(format string, args ...any) {
	if s.cfg.Logger == nil {
		return
	}
	s.cfg.Logger.Printf(format, args...)
}

// SchemaDDL returns the DDL statements that create the events
// and counters tables. Operators can apply these out-of-band or
// through their migration tool of choice.
//
// The table argument is the events table name; the counters table
// name is derived by replacing the trailing "_events" suffix with
// "_counters". The table name must therefore end in "_events";
// this mirrors the same invariant NewClickHouseSink enforces on
// its config. An empty table name defaults to "usage_events".
func SchemaDDL(database, table string) string {
	if table == "" {
		table = "usage_events"
	}
	if !strings.HasSuffix(table, "_events") {
		// Fall back to appending rather than producing a
		// nonsensical "my_tablecounters" name. This path is
		// unreachable through NewClickHouseSink, which rejects
		// table names that don't end in "_events", but keeps
		// direct callers of SchemaDDL honest.
		return fmt.Sprintf("-- billing: table %q does not end in \"_events\"; expected suffix required to derive counters table\n", table)
	}
	counterTable := strings.TrimSuffix(table, "_events") + "_counters"
	return fmt.Sprintf(`-- billing schema for ClickHouseSink.
CREATE TABLE IF NOT EXISTS %[1]s.%[2]s (
    tenant_id      LowCardinality(String),
    bucket         LowCardinality(String),
    dimension      LowCardinality(String),
    delta          UInt64,
    observed_at    DateTime64(3, 'UTC'),
    source_node_id LowCardinality(String)
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(observed_at)
ORDER BY (tenant_id, bucket, dimension, observed_at);

CREATE TABLE IF NOT EXISTS %[1]s.%[3]s (
    tenant_id     LowCardinality(String),
    bucket        LowCardinality(String),
    dimension     LowCardinality(String),
    value         UInt64,
    period_start  DateTime,
    period_end    DateTime
) ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(period_start)
ORDER BY (tenant_id, bucket, dimension, period_start);
`, database, table, counterTable)
}

func discardAndClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}

// static check
var _ BillingSink = (*ClickHouseSink)(nil)

// BillingSink is the interface implemented by billing sinks. It
// mirrors api/s3compat.BillingSink so this package can be imported
// standalone (e.g. by a separate metering service).
type BillingSink interface {
	Emit(event UsageEvent)
}
