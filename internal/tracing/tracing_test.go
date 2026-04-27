package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTracer_StartEnd_RecordsSpan(t *testing.T) {
	exp := &MemoryExporter{}
	tr := New("svc", exp)
	_, span := tr.Start(context.Background(), "test.op")
	span.SetAttribute("k", "v")
	span.AddEvent("retry", map[string]string{"n": "1"})
	span.End()
	if len(exp.Spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(exp.Spans))
	}
	got := exp.Spans[0]
	if got.Name != "test.op" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.Attributes["k"] != "v" {
		t.Errorf("attribute k = %q", got.Attributes["k"])
	}
	if len(got.Events) != 1 || got.Events[0].Name != "retry" {
		t.Errorf("events = %+v", got.Events)
	}
	if got.TraceID == "" || got.SpanID == "" {
		t.Errorf("missing IDs: trace=%q span=%q", got.TraceID, got.SpanID)
	}
}

func TestTracer_NestedSpans_ShareTraceID(t *testing.T) {
	exp := &MemoryExporter{}
	tr := New("svc", exp)
	ctx, parent := tr.Start(context.Background(), "parent")
	_, child := tr.Start(ctx, "child")
	child.End()
	parent.End()
	if len(exp.Spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(exp.Spans))
	}
	if exp.Spans[0].TraceID != exp.Spans[1].TraceID {
		t.Errorf("trace IDs differ: %q vs %q", exp.Spans[0].TraceID, exp.Spans[1].TraceID)
	}
	if exp.Spans[0].ParentID != exp.Spans[1].SpanID {
		t.Errorf("expected child.ParentID == parent.SpanID")
	}
}

func TestTracer_Middleware_AnnotatesRequest(t *testing.T) {
	exp := &MemoryExporter{}
	tr := New("svc", exp)
	handler := tr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if SpanFromContext(r.Context()) == nil {
			t.Error("middleware did not inject span into ctx")
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x/y", nil)
	handler.ServeHTTP(rec, req)
	if len(exp.Spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(exp.Spans))
	}
	got := exp.Spans[0]
	if got.Attributes["http.method"] != "GET" {
		t.Errorf("http.method = %q", got.Attributes["http.method"])
	}
	if got.Attributes["http.path"] != "/x/y" {
		t.Errorf("http.path = %q", got.Attributes["http.path"])
	}
	if got.Attributes["http.status"] != "4xx" {
		t.Errorf("http.status = %q", got.Attributes["http.status"])
	}
}

func TestTracer_Nil_NoOp(t *testing.T) {
	var tr *Tracer
	ctx, span := tr.Start(context.Background(), "noop")
	span.SetAttribute("k", "v") // must not panic
	span.AddEvent("e", nil)
	span.End()
	if SpanFromContext(ctx) != nil {
		t.Errorf("nil tracer should not place span in ctx")
	}
}
