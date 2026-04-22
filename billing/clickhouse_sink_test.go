package billing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type captured struct {
	mu       sync.Mutex
	requests []string
	status   int
	fail5xx  int // force N 5xx responses before succeeding
}

func (c *captured) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		if c.fail5xx > 0 {
			c.fail5xx--
			http.Error(w, "simulated failure", http.StatusInternalServerError)
			return
		}
		c.requests = append(c.requests, string(body))
		if c.status == 0 {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(c.status)
		}
	})
}

func (c *captured) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func TestClickHouseSink_FlushOnBatchSize(t *testing.T) {
	cap := &captured{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	sink, err := NewClickHouseSink(ClickHouseConfig{
		Endpoint:  srv.URL,
		Database:  "obj",
		BatchSize: 3,
	})
	if err != nil {
		t.Fatalf("NewClickHouseSink: %v", err)
	}
	for i := 0; i < 3; i++ {
		sink.Emit(UsageEvent{TenantID: "t1", Bucket: "b1", Dimension: PutRequests, Delta: 1, ObservedAt: time.Unix(1, 0)})
	}
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if cap.total() != 1 {
		t.Fatalf("captured %d requests, want 1", cap.total())
	}
	if !strings.Contains(cap.requests[0], `"tenant_id":"t1"`) {
		t.Fatalf("request body missing tenant_id: %s", cap.requests[0])
	}
}

func TestClickHouseSink_FlushOnClose(t *testing.T) {
	cap := &captured{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	sink, err := NewClickHouseSink(ClickHouseConfig{
		Endpoint:      srv.URL,
		Database:      "obj",
		BatchSize:     1000,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewClickHouseSink: %v", err)
	}
	sink.Emit(UsageEvent{TenantID: "t", Bucket: "b", Dimension: GetRequests, Delta: 7, ObservedAt: time.Unix(2, 0)})
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if cap.total() != 1 {
		t.Fatalf("captured %d requests, want 1", cap.total())
	}
}

func TestClickHouseSink_RetriesOn5xx(t *testing.T) {
	cap := &captured{fail5xx: 2}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	sink, err := NewClickHouseSink(ClickHouseConfig{
		Endpoint:   srv.URL,
		Database:   "obj",
		BatchSize:  1,
		MaxRetries: 5,
		RetryBase:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClickHouseSink: %v", err)
	}
	sink.Emit(UsageEvent{TenantID: "t", Bucket: "b", Dimension: EgressBytes, Delta: 42, ObservedAt: time.Unix(3, 0)})
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if cap.total() != 1 {
		t.Fatalf("captured %d requests after retry, want 1", cap.total())
	}
	flushed, failed := sink.Stats()
	if flushed != 1 {
		t.Fatalf("flushed = %d, want 1", flushed)
	}
	if failed != 0 {
		t.Fatalf("failed = %d, want 0 after success on retry", failed)
	}
}

func TestClickHouseSink_RejectsInvalidConfig(t *testing.T) {
	if _, err := NewClickHouseSink(ClickHouseConfig{Database: "x"}); err == nil {
		t.Fatalf("missing endpoint: want error, got nil")
	}
	if _, err := NewClickHouseSink(ClickHouseConfig{Endpoint: "http://x"}); err == nil {
		t.Fatalf("missing database: want error, got nil")
	}
}

func TestSchemaDDL_ContainsTables(t *testing.T) {
	ddl := SchemaDDL("obj", "usage_events")
	for _, want := range []string{"obj.usage_events", "obj.usage_counters", "MergeTree", "SummingMergeTree"} {
		if !strings.Contains(ddl, want) {
			t.Fatalf("schema missing %q", want)
		}
	}
}
