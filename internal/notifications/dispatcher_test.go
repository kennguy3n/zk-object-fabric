package notifications

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/notification"
)

// stubSource returns a fixed config for any (tenant, bucket).
type stubSource struct {
	cfg notification.Config
	err error
}

func (s stubSource) GetNotification(_ context.Context, _, _ string) (notification.Config, error) {
	return s.cfg, s.err
}

// capturingDoer records every request body it receives and replies
// with a configurable status. It is safe for concurrent use.
type capturingDoer struct {
	mu      sync.Mutex
	bodies  [][]byte
	urls    []string
	headers []http.Header
	status  int
	err     error
	calls   atomic.Int64
}

func (c *capturingDoer) Do(req *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	body, _ := io.ReadAll(req.Body)
	c.mu.Lock()
	c.bodies = append(c.bodies, body)
	c.urls = append(c.urls, req.URL.String())
	c.headers = append(c.headers, req.Header.Clone())
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(http.NoBody)}, nil
}

// recordingDeadLetters captures dead letters for assertions.
type recordingDeadLetters struct {
	mu   sync.Mutex
	dead []DeadLetter
}

func (r *recordingDeadLetters) Dead(dl DeadLetter) {
	r.mu.Lock()
	r.dead = append(r.dead, dl)
	r.mu.Unlock()
}

func (r *recordingDeadLetters) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.dead)
}

func putRule(endpoint string) notification.Config {
	return notification.Config{Rules: []notification.Rule{{
		ID:       "r1",
		Events:   []notification.EventType{notification.ObjectCreatedAll},
		Endpoint: endpoint,
	}}}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestDispatcherDeliversMatchingEvent(t *testing.T) {
	doer := &capturingDoer{}
	d, err := New(Config{
		Source:     stubSource{cfg: putRule("https://hook.example/test")},
		HTTPClient: doer,
		Workers:    2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	d.Notify(Event{
		TenantID:  "t1",
		Bucket:    "b1",
		Name:      notification.ObjectCreatedPut,
		ObjectKey: "photos/cat.jpg",
		SizeBytes: 1234,
		ETag:      "etag-abc",
	})

	waitFor(t, func() bool { return doer.calls.Load() == 1 })

	doer.mu.Lock()
	defer doer.mu.Unlock()
	if got := doer.urls[0]; got != "https://hook.example/test" {
		t.Errorf("endpoint = %q", got)
	}
	var env s3EventEnvelope
	if err := json.Unmarshal(doer.bodies[0], &env); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if len(env.Records) != 1 {
		t.Fatalf("Records len = %d", len(env.Records))
	}
	rec := env.Records[0]
	if rec.EventName != "s3:ObjectCreated:Put" {
		t.Errorf("eventName = %q", rec.EventName)
	}
	if rec.S3.Bucket.Name != "b1" {
		t.Errorf("bucket = %q", rec.S3.Bucket.Name)
	}
	if rec.S3.Object.Key != "photos/cat.jpg" {
		t.Errorf("key = %q", rec.S3.Object.Key)
	}
	if rec.S3.Object.Size != 1234 || rec.S3.Object.ETag != "etag-abc" {
		t.Errorf("size/etag = %d/%q", rec.S3.Object.Size, rec.S3.Object.ETag)
	}
	if rec.S3.Object.Sequencer == "" {
		t.Error("sequencer not filled in")
	}
	if rec.S3.ConfigurationID != "r1" {
		t.Errorf("configurationId = %q", rec.S3.ConfigurationID)
	}
	if got := doer.headers[0].Get("X-Zkof-Event"); got != "s3:ObjectCreated:Put" {
		t.Errorf("X-Zkof-Event = %q", got)
	}
}

func TestDispatcherSkipsNonMatchingPrefix(t *testing.T) {
	doer := &capturingDoer{}
	cfg := notification.Config{Rules: []notification.Rule{{
		Events:   []notification.EventType{notification.ObjectCreatedAll},
		Endpoint: "https://hook.example/test",
		Prefix:   "logs/",
	}}}
	d, err := New(Config{Source: stubSource{cfg: cfg}, HTTPClient: doer, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "photos/cat.jpg"})
	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "logs/app.log"})

	waitFor(t, func() bool { return doer.calls.Load() == 1 })
	// Give any erroneous second delivery a chance to land.
	time.Sleep(20 * time.Millisecond)
	if got := doer.calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (only the logs/ key matches)", got)
	}
}

func TestDispatcherRetriesThenDeadLetters(t *testing.T) {
	doer := &capturingDoer{status: http.StatusInternalServerError}
	dead := &recordingDeadLetters{}
	d, err := New(Config{
		Source:      stubSource{cfg: putRule("https://hook.example/test")},
		HTTPClient:  doer,
		Workers:     1,
		MaxAttempts: 3,
		BackoffBase: time.Millisecond,
		DeadLetters: dead,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "k"})

	waitFor(t, func() bool { return dead.len() == 1 })
	if got := doer.calls.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	dead.mu.Lock()
	dl := dead.dead[0]
	dead.mu.Unlock()
	if dl.Attempts != 3 || dl.Endpoint != "https://hook.example/test" {
		t.Errorf("dead letter = %+v", dl)
	}
	if s := d.Stats(); s.Failed != 1 || s.Delivered != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestDispatcherRetrySucceedsSecondAttempt(t *testing.T) {
	// First call 500, subsequent 200.
	var n atomic.Int64
	doer := &togglingDoer{fn: func() int {
		if n.Add(1) == 1 {
			return http.StatusBadGateway
		}
		return http.StatusOK
	}}
	dead := &recordingDeadLetters{}
	d, err := New(Config{
		Source:      stubSource{cfg: putRule("https://hook.example/test")},
		HTTPClient:  doer,
		Workers:     1,
		MaxAttempts: 4,
		BackoffBase: time.Millisecond,
		DeadLetters: dead,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "k"})
	waitFor(t, func() bool { return d.Stats().Delivered == 1 })
	if dead.len() != 0 {
		t.Errorf("unexpected dead letters: %d", dead.len())
	}
}

type togglingDoer struct{ fn func() int }

func (d *togglingDoer) Do(req *http.Request) (*http.Response, error) {
	_, _ = io.ReadAll(req.Body)
	return &http.Response{StatusCode: d.fn(), Body: io.NopCloser(http.NoBody)}, nil
}

func TestDispatcherEmptyConfigNoDelivery(t *testing.T) {
	doer := &capturingDoer{}
	d, err := New(Config{Source: stubSource{cfg: notification.Config{}}, HTTPClient: doer, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "k"})
	time.Sleep(30 * time.Millisecond)
	if got := doer.calls.Load(); got != 0 {
		t.Errorf("calls = %d, want 0", got)
	}
}

func TestDispatcherQueueFullDropsAndDeadLetters(t *testing.T) {
	// A blocking doer keeps the single worker busy so the queue fills.
	release := make(chan struct{})
	doer := &blockingDoer{release: release}
	dead := &recordingDeadLetters{}
	d, err := New(Config{
		Source:      stubSource{cfg: putRule("https://hook.example/test")},
		HTTPClient:  doer,
		Workers:     1,
		QueueSize:   1,
		DeadLetters: dead,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(release)
		d.Close()
	}()

	// Flood far beyond worker+queue capacity; at least one must drop.
	for i := 0; i < 50; i++ {
		d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "k"})
	}
	waitFor(t, func() bool { return d.Stats().Dropped > 0 && dead.len() > 0 })
}

type blockingDoer struct{ release chan struct{} }

func (b *blockingDoer) Do(req *http.Request) (*http.Response, error) {
	_, _ = io.ReadAll(req.Body)
	<-b.release
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(http.NoBody)}, nil
}

func TestNilDispatcherNotifyIsNoOp(t *testing.T) {
	var d *Dispatcher
	d.Notify(Event{}) // must not panic
	d.Close()         // must not panic
	if s := d.Stats(); s != (Stats{}) {
		t.Errorf("nil stats = %+v", s)
	}
}

func TestNewRequiresSource(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error when Source is nil")
	}
}

// TestNotifyDuringCloseNoPanic exercises the send-after-close race:
// concurrent Notify calls while Close runs must never panic with "send
// on closed channel". Run with -race to also catch data races.
func TestNotifyDuringCloseNoPanic(t *testing.T) {
	doer := &capturingDoer{}
	d, err := New(Config{Source: stubSource{cfg: putRule("https://hook.example/test")}, HTTPClient: doer, Workers: 4})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "k"})
			}
		}()
	}
	// Close concurrently with the in-flight Notify storm.
	time.Sleep(time.Millisecond)
	d.Close()
	wg.Wait()
	// Notify after Close must also be a safe no-op (drops).
	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "k"})
}
