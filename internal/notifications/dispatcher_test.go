package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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
	if rec.S3.Object.Size == nil || *rec.S3.Object.Size != 1234 || rec.S3.Object.ETag != "etag-abc" {
		t.Errorf("size/etag = %v/%q", rec.S3.Object.Size, rec.S3.Object.ETag)
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

// firstBlockDoer blocks the very first Do call until release is closed,
// signalling via started, and answers every call (including the first,
// once released) with status. It lets a test occupy the single worker
// so subsequent Notify calls land in the queue, then exercise the
// shutdown drain.
type firstBlockDoer struct {
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
	status  int
}

func (d *firstBlockDoer) Do(req *http.Request) (*http.Response, error) {
	_, _ = io.ReadAll(req.Body)
	if d.calls.Add(1) == 1 {
		close(d.started)
		<-d.release
	}
	s := d.status
	if s == 0 {
		s = http.StatusOK
	}
	return &http.Response{StatusCode: s, Body: io.NopCloser(http.NoBody)}, nil
}

// errorBlockDoer is firstBlockDoer that always fails the delivery, so a
// test can assert queued events are dead-lettered (not silently dropped)
// when the dispatcher shuts down mid-drain.
type errorBlockDoer struct {
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
}

func (d *errorBlockDoer) Do(req *http.Request) (*http.Response, error) {
	_, _ = io.ReadAll(req.Body)
	if d.calls.Add(1) == 1 {
		close(d.started)
		<-d.release
	}
	return nil, errors.New("connection refused")
}

// TestShutdownDeliversQueuedEvents proves Close drains already-queued
// events instead of abandoning them: events enqueued while a worker is
// busy are still delivered during the graceful drain.
func TestShutdownDeliversQueuedEvents(t *testing.T) {
	doer := &firstBlockDoer{started: make(chan struct{}), release: make(chan struct{})}
	d, err := New(Config{
		Source:      stubSource{cfg: putRule("https://hook.example/x")},
		HTTPClient:  doer,
		Workers:     1,
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// First event occupies the sole worker.
	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "a"})
	<-doer.started
	// Two more queue up behind the blocked worker.
	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "b"})
	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "c"})

	close(doer.release)
	d.Close() // must drain b and c, not abandon them

	if s := d.Stats(); s.Delivered != 3 {
		t.Errorf("delivered = %d, want 3 (queued events must drain on shutdown); stats=%+v", s.Delivered, s)
	}
	if s := d.Stats(); s.Dropped != 0 || s.Failed != 0 {
		t.Errorf("unexpected drop/fail on graceful drain: %+v", s)
	}
}

// TestShutdownDeadLettersUndeliverableQueuedEvents proves that when a
// queued event cannot be delivered during shutdown it is dead-lettered
// rather than silently dropped (the WS8.6 review's #7 finding).
func TestShutdownDeadLettersUndeliverableQueuedEvents(t *testing.T) {
	doer := &errorBlockDoer{started: make(chan struct{}), release: make(chan struct{})}
	dead := &recordingDeadLetters{}
	d, err := New(Config{
		Source:      stubSource{cfg: putRule("https://hook.example/x")},
		HTTPClient:  doer,
		Workers:     1,
		MaxAttempts: 4,
		BackoffBase: time.Millisecond,
		DeadLetters: dead,
	})
	if err != nil {
		t.Fatal(err)
	}

	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "a"})
	<-doer.started
	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "b"})

	go func() {
		time.Sleep(10 * time.Millisecond)
		close(doer.release)
	}()
	d.Close()

	if got := dead.len(); got != 2 {
		t.Errorf("dead letters = %d, want 2 (queued events must be dead-lettered, never silently dropped)", got)
	}
	if s := d.Stats(); s.Dropped != 0 {
		t.Errorf("dropped = %d, want 0 (the drain dead-letters; it never silently drops)", s.Dropped)
	}
}

// TestProcessConfigLookupFailureDeadLetters proves a config-lookup error
// surfaces as a dead letter instead of a silent drop + misleading WARN.
func TestProcessConfigLookupFailureDeadLetters(t *testing.T) {
	dead := &recordingDeadLetters{}
	d, err := New(Config{
		Source:      stubSource{err: errors.New("backend down")},
		HTTPClient:  &capturingDoer{},
		Workers:     1,
		DeadLetters: dead,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "k"})
	waitFor(t, func() bool { return dead.len() == 1 })
	dead.mu.Lock()
	dl := dead.dead[0]
	dead.mu.Unlock()
	if dl.Endpoint != "" {
		t.Errorf("config-lookup dead letter Endpoint = %q, want empty (destination unknown)", dl.Endpoint)
	}
	if s := d.Stats(); s.Failed != 1 {
		t.Errorf("failed = %d, want 1", s.Failed)
	}
}

// countingBody is a ReadCloser that yields an effectively unbounded
// stream of bytes and records how many were read, so a test can prove
// the dispatcher's keep-alive drain is bounded rather than reading the
// whole (hostile) body.
type countingBody struct {
	read   atomic.Int64
	closed atomic.Bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	b.read.Add(int64(len(p)))
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func (b *countingBody) Close() error {
	b.closed.Store(true)
	return nil
}

// floodDoer replies 200 with a body that never ends, to exercise the
// bounded drain on the success path.
type floodDoer struct {
	body  *countingBody
	calls atomic.Int64
}

func (d *floodDoer) Do(_ *http.Request) (*http.Response, error) {
	d.calls.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Body: d.body}, nil
}

func TestAttemptBoundsResponseBodyDrain(t *testing.T) {
	body := &countingBody{}
	doer := &floodDoer{body: body}
	d, err := New(Config{
		Source:     stubSource{cfg: putRule("https://hook.example/test")},
		HTTPClient: doer,
		Workers:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "k"})
	waitFor(t, func() bool { return doer.calls.Load() == 1 && body.closed.Load() })

	// io.LimitReader stops the discard copy at maxDrainBytes; the final
	// Read may overshoot by at most one buffer (io.Copy's 32 KiB default),
	// so allow a single buffer of slack. The key property: the drain does
	// NOT read the unbounded body to exhaustion.
	if got := body.read.Load(); got > maxDrainBytes+32<<10 {
		t.Errorf("drained %d bytes, want <= %d (bounded drain)", got, maxDrainBytes+32<<10)
	}
}

// TestRenderObjectSizePresence pins the AWS-compatible rule that the
// s3.object.size field is present on every ObjectCreated record (even a
// 0-byte object, where it must serialise as "size":0) and absent on
// ObjectRemoved records. It marshals the rendered envelope and inspects
// the raw JSON so the presence/absence of the key is asserted directly,
// not via the *int64 round-trip.
func TestRenderObjectSizePresence(t *testing.T) {
	objectJSON := func(e Event) string {
		raw, err := json.Marshal(e.render(""))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var env struct {
			Records []struct {
				S3 struct {
					Object json.RawMessage `json:"object"`
				} `json:"s3"`
			} `json:"Records"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(env.Records) != 1 {
			t.Fatalf("Records len = %d", len(env.Records))
		}
		return string(env.Records[0].S3.Object)
	}

	cases := []struct {
		name      string
		event     Event
		wantSize  string // substring that must be present
		omitsSize bool
	}{
		{
			name:     "create nonzero carries size",
			event:    Event{Name: notification.ObjectCreatedPut, ObjectKey: "k", SizeBytes: 1234},
			wantSize: `"size":1234`,
		},
		{
			name:     "create zero-byte still carries size:0",
			event:    Event{Name: notification.ObjectCreatedCompleteMultipartUpload, ObjectKey: "k", SizeBytes: 0},
			wantSize: `"size":0`,
		},
		{
			name:      "remove omits size entirely",
			event:     Event{Name: notification.ObjectRemovedDelete, ObjectKey: "k", SizeBytes: 0},
			omitsSize: true,
		},
		{
			name:      "delete-marker omits size entirely",
			event:     Event{Name: notification.ObjectRemovedDeleteMarkerCreated, ObjectKey: "k"},
			omitsSize: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := objectJSON(tc.event)
			if tc.omitsSize {
				if strings.Contains(obj, `"size"`) {
					t.Errorf("object %s unexpectedly contains size", obj)
				}
				return
			}
			if !strings.Contains(obj, tc.wantSize) {
				t.Errorf("object %s missing %s", obj, tc.wantSize)
			}
		})
	}
}
