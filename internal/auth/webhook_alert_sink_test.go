package auth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
)

func TestWebhookAlertSink_DeliverPostsJSON(t *testing.T) {
	var got webhookPayload
	var contentType string
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		contentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	sink := NewWebhookAlertSink(srv.URL)
	ev := billing.UsageEvent{
		TenantID:     "tenant-a",
		Bucket:       "b1",
		Dimension:    billing.AbuseAnomalyAlert,
		Delta:        7,
		ObservedAt:   time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC),
		SourceNodeID: "gw-1",
	}
	if err := sink.deliver(ev); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("server calls = %d", calls)
	}
	if contentType != "application/json" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	if got.TenantID != "tenant-a" || got.Bucket != "b1" || got.Dimension != string(billing.AbuseAnomalyAlert) {
		t.Fatalf("payload mismatch: %+v", got)
	}
	if got.Delta != 7 || got.SourceNodeID != "gw-1" {
		t.Fatalf("payload mismatch: %+v", got)
	}
}

func TestWebhookAlertSink_NonOKStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("nope"))
	}))
	t.Cleanup(srv.Close)
	if err := NewWebhookAlertSink(srv.URL).deliver(billing.UsageEvent{TenantID: "t"}); err == nil {
		t.Fatalf("expected error from non-2xx response")
	}
}

func TestWebhookAlertSink_EmitDispatchesGoroutine(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	sink := NewWebhookAlertSink(srv.URL)
	sink.Emit(billing.UsageEvent{TenantID: "t1", Dimension: billing.AbuseBudgetExhausted, Delta: 1})

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("webhook did not fire within 2s")
	}
}

func TestWebhookAlertSink_EmptyURLNoOp(t *testing.T) {
	// Should not panic and should not block.
	(&WebhookAlertSink{}).Emit(billing.UsageEvent{TenantID: "t"})
}

// fanOutSink records every event for use in MultiAlertSink tests.
type fanOutSink struct {
	mu     sync.Mutex
	events []billing.UsageEvent
}

func (f *fanOutSink) Emit(ev billing.UsageEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func TestMultiAlertSink_FansOut(t *testing.T) {
	a, b := &fanOutSink{}, &fanOutSink{}
	multi := NewMultiAlertSink(a, b)
	multi.Emit(billing.UsageEvent{TenantID: "t1", Dimension: billing.AbuseAnomalyAlert, Delta: 1})
	multi.Emit(billing.UsageEvent{TenantID: "t2", Dimension: billing.AbuseBudgetExhausted, Delta: 2})
	if len(a.events) != 2 || len(b.events) != 2 {
		t.Fatalf("fan-out lengths: a=%d b=%d", len(a.events), len(b.events))
	}
	if a.events[0].TenantID != "t1" || b.events[1].TenantID != "t2" {
		t.Fatalf("fan-out ordering mismatch")
	}
}

func TestMultiAlertSink_NilSinksCompacted(t *testing.T) {
	if got := NewMultiAlertSink(nil, nil); got != nil {
		t.Fatalf("expected nil for all-nil inputs, got %T", got)
	}
	a := &fanOutSink{}
	if got := NewMultiAlertSink(nil, a, nil); got != a {
		t.Fatalf("expected single-sink unwrap, got %T", got)
	}
}
