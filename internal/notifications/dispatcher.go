// Package notifications delivers S3 bucket event notifications (WS8.6)
// to the per-bucket webhook destinations configured through
// metadata/bucket_config. The gateway's S3 handler hands the dispatcher
// a fully-populated Event on the hot path via Notify, which is
// non-blocking: the actual configuration lookup, rule matching, webhook
// POST, retry, and dead-lettering all happen on the dispatcher's own
// worker goroutines so a slow or unreachable destination never adds
// latency to (or fails) the originating S3 request.
package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/notification"
)

// ConfigSource supplies the per-bucket notification configuration the
// dispatcher matches events against. It is satisfied by
// bucket_config.Store.
type ConfigSource interface {
	GetNotification(ctx context.Context, tenantID, bucket string) (notification.Config, error)
}

// Doer is the subset of *http.Client the dispatcher uses; tests stub
// the transport through it.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// DeadLetter is one (event, destination) delivery that exhausted its
// retries, handed to the configured DeadLetterSink.
type DeadLetter struct {
	Event    Event
	Endpoint string
	Attempts int
	LastErr  string
}

// DeadLetterSink receives deliveries that could not be completed after
// all retries, and events dropped because the queue was full. A nil
// sink logs at WARN.
type DeadLetterSink interface {
	Dead(dl DeadLetter)
}

// Stats is a snapshot of dispatcher counters for observability and
// tests.
type Stats struct {
	Enqueued  uint64
	Dropped   uint64
	Delivered uint64
	Failed    uint64
}

// Config wires a Dispatcher. Only Source is required.
type Config struct {
	// Source resolves a bucket's notification configuration. Required.
	Source ConfigSource

	// HTTPClient delivers webhook POSTs. Defaults to an *http.Client
	// with DeliveryTimeout.
	HTTPClient Doer

	// Workers is the number of delivery goroutines. Defaults to 4.
	Workers int

	// QueueSize bounds the in-flight event buffer. When full, Notify
	// drops the event (and reports it to the DeadLetterSink) rather
	// than blocking the S3 hot path. Defaults to 1024.
	QueueSize int

	// MaxAttempts is the total number of delivery attempts per
	// destination (the first try plus retries). Defaults to 4.
	MaxAttempts int

	// BackoffBase is the base of the exponential backoff between
	// attempts. Defaults to 200ms.
	BackoffBase time.Duration

	// DeliveryTimeout bounds a single webhook POST. Defaults to 5s.
	DeliveryTimeout time.Duration

	// ShutdownGrace bounds how long Close spends draining already-queued
	// events. Events still queued when the grace elapses are
	// dead-lettered rather than delivered, so a backlog of unreachable
	// destinations cannot stall shutdown. Defaults to 10s (and is never
	// shorter than DeliveryTimeout).
	ShutdownGrace time.Duration

	// AllowPrivateDestinations disables the SSRF guard that, by default,
	// refuses to deliver to loopback, link-local, and private (RFC 1918 /
	// ULA) addresses. It only takes effect when HTTPClient is nil (the
	// dispatcher builds its own guarded client); an injected HTTPClient
	// is used as-is. Leave false in production; set true for local
	// development against localhost webhooks.
	AllowPrivateDestinations bool

	// DeadLetters receives exhausted/dropped deliveries. Defaults to a
	// sink that logs at WARN.
	DeadLetters DeadLetterSink

	// Now supplies the current time; defaults to time.Now. Injected
	// for deterministic tests.
	Now func() time.Time
}

// Dispatcher fans matched object events out to configured webhook
// destinations on background workers.
type Dispatcher struct {
	source      ConfigSource
	client      Doer
	queue       chan Event
	maxAttempts int
	backoffBase time.Duration
	timeout     time.Duration
	grace       time.Duration
	dead        DeadLetterSink
	now         func() time.Time

	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    chan struct{}
	// drainDeadline bounds the post-Close drain. It is written once in
	// Close before closed is closed, and only read by workers after they
	// observe closed, so the channel close provides the happens-before.
	drainDeadline time.Time
	seq           atomic.Uint64

	// mu guards the queue against the send-after-close race: Notify
	// takes it for reading around the channel send, Close takes it for
	// writing before closing the queue, so a send can never race the
	// close. isClosed lets a late Notify drop instead of panicking.
	mu       sync.RWMutex
	isClosed bool

	enqueued  atomic.Uint64
	dropped   atomic.Uint64
	delivered atomic.Uint64
	failed    atomic.Uint64
}

// logSink is the default DeadLetterSink: it logs each dead letter at
// WARN using the standard logger, matching the rest of the gateway.
type logSink struct{}

func (logSink) Dead(dl DeadLetter) {
	log.Printf("notifications: WARN dead_letter: tenant=%s bucket=%s key=%s event=%s endpoint=%s attempts=%d err=%v",
		dl.Event.TenantID, dl.Event.Bucket, dl.Event.ObjectKey, dl.Event.Name, dl.Endpoint, dl.Attempts, dl.LastErr)
}

// New constructs and starts a Dispatcher. The returned dispatcher's
// workers run until Close is called.
func New(cfg Config) (*Dispatcher, error) {
	if cfg.Source == nil {
		return nil, errors.New("notifications: Config.Source is required")
	}
	workers := cfg.Workers
	if workers <= 0 {
		workers = 4
	}
	queueSize := cfg.QueueSize
	if queueSize <= 0 {
		queueSize = 1024
	}
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 4
	}
	backoff := cfg.BackoffBase
	if backoff <= 0 {
		backoff = 200 * time.Millisecond
	}
	timeout := cfg.DeliveryTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	grace := cfg.ShutdownGrace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	if grace < timeout {
		grace = timeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = newGuardedClient(timeout, cfg.AllowPrivateDestinations)
	}
	dead := cfg.DeadLetters
	if dead == nil {
		dead = logSink{}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	d := &Dispatcher{
		source:      cfg.Source,
		client:      client,
		queue:       make(chan Event, queueSize),
		maxAttempts: maxAttempts,
		backoffBase: backoff,
		timeout:     timeout,
		grace:       grace,
		dead:        dead,
		now:         now,
		closed:      make(chan struct{}),
	}
	d.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go d.worker()
	}
	return d, nil
}

// Notify enqueues an event for asynchronous delivery. It never blocks:
// if the queue is full the event is dropped and reported to the
// DeadLetterSink, so a slow destination can never stall the S3 request
// path. A nil dispatcher is a no-op, so callers can hold an optional
// *Dispatcher without nil checks.
func (d *Dispatcher) Notify(evt Event) {
	if d == nil {
		return
	}
	if evt.Time.IsZero() {
		evt.Time = d.now().UTC()
	}
	if evt.Sequencer == "" {
		evt.Sequencer = strconv.FormatUint(d.seq.Add(1), 16)
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.isClosed {
		// Dispatcher is shutting down; drop without dead-lettering
		// (the drain in Close handles already-queued events).
		d.dropped.Add(1)
		return
	}
	select {
	case d.queue <- evt:
		d.enqueued.Add(1)
	default:
		d.dropped.Add(1)
		d.dead.Dead(DeadLetter{Event: evt, Attempts: 0, LastErr: "dispatcher queue full"})
	}
}

// Close stops accepting events and drains the queue within ShutdownGrace.
// Already-queued events are still delivered (one attempt each, no retry
// backoff); any that cannot be delivered before the grace deadline are
// dead-lettered rather than silently dropped. It is safe to call
// multiple times.
func (d *Dispatcher) Close() {
	if d == nil {
		return
	}
	d.closeOnce.Do(func() {
		d.mu.Lock()
		// drainDeadline must be set before closing closed: workers only
		// read it after observing the close, so the close orders the write.
		d.drainDeadline = time.Now().Add(d.grace)
		close(d.closed)
		d.isClosed = true
		close(d.queue)
		d.mu.Unlock()
	})
	d.wg.Wait()
}

// Stats returns a snapshot of the dispatcher's counters.
func (d *Dispatcher) Stats() Stats {
	if d == nil {
		return Stats{}
	}
	return Stats{
		Enqueued:  d.enqueued.Load(),
		Dropped:   d.dropped.Load(),
		Delivered: d.delivered.Load(),
		Failed:    d.failed.Load(),
	}
}

func (d *Dispatcher) worker() {
	defer d.wg.Done()
	for evt := range d.queue {
		d.process(evt)
	}
}

// process resolves the bucket's notification configuration, matches the
// event against its rules, and delivers the rendered payload to each
// matching destination. The config lookup uses opContext, which is
// bounded by DeliveryTimeout (and the drain deadline during shutdown)
// but is NOT tied to the shutdown signal itself, so an already-queued
// event still resolves its rules and is delivered during a graceful
// drain instead of being abandoned.
func (d *Dispatcher) process(evt Event) {
	ctx, cancel := d.opContext()
	cfg, err := d.source.GetNotification(ctx, evt.TenantID, evt.Bucket)
	cancel()
	if err != nil {
		// The rules are unknown, so there is no destination endpoint to
		// target; record the loss through the dead-letter sink (with an
		// empty Endpoint) rather than only logging, so a graceful restart
		// or a backend blip never silently drops a queued event.
		d.failed.Add(1)
		d.dead.Dead(DeadLetter{Event: evt, Attempts: 0, LastErr: fmt.Sprintf("config lookup failed: %v", err)})
		return
	}
	if cfg.Empty() {
		return
	}
	for _, rule := range cfg.Match(evt.Name, evt.ObjectKey) {
		d.deliver(evt, rule)
	}
}

// deliver POSTs the event to one destination, retrying with
// exponential backoff up to maxAttempts before dead-lettering. During
// shutdown the backoff is short-circuited, so an in-flight delivery gets
// at most one more attempt and is then dead-lettered with the actual
// number of attempts made.
func (d *Dispatcher) deliver(evt Event, rule notification.Rule) {
	body, err := json.Marshal(evt.render(rule.ID))
	if err != nil {
		d.failed.Add(1)
		d.dead.Dead(DeadLetter{Event: evt, Endpoint: rule.Endpoint, Attempts: 0, LastErr: fmt.Sprintf("render: %v", err)})
		return
	}
	var lastErr string
	completed := 0
	for attempt := 1; attempt <= d.maxAttempts; attempt++ {
		if attempt > 1 {
			if !d.sleep(d.backoff(attempt - 1)) {
				lastErr = "dispatcher shutting down"
				break
			}
		}
		completed = attempt
		lastErr = d.attempt(rule.Endpoint, evt.Name, body)
		if lastErr == "" {
			d.delivered.Add(1)
			return
		}
	}
	d.failed.Add(1)
	d.dead.Dead(DeadLetter{Event: evt, Endpoint: rule.Endpoint, Attempts: completed, LastErr: lastErr})
}

// maxDrainBytes caps how many bytes of a webhook response body we read
// back before closing the connection. We only drain to enable keep-alive
// reuse; the body content is never inspected, so 32 KiB is ample for any
// well-behaved endpoint while denying a hostile one the ability to stream
// unbounded data into the discard copy.
const maxDrainBytes = 32 << 10

// attempt performs a single webhook POST. It returns an empty string on
// success (2xx) or a description of the failure otherwise.
func (d *Dispatcher) attempt(endpoint string, name notification.EventType, body []byte) string {
	ctx, cancel := d.opContext()
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Sprintf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "zkof-notifications/1")
	req.Header.Set("X-Zkof-Event", string(name))
	resp, err := d.client.Do(req)
	if err != nil {
		return err.Error()
	}
	// Drain the body before closing so the underlying transport can
	// reuse the keep-alive connection for the next POST to this
	// destination instead of opening a fresh TCP (and TLS) connection.
	// The drain is bounded by maxDrainBytes: webhook destinations are
	// tenant-configured and may be malicious or misconfigured, so we cap
	// how much of a response body we read back purely to free the
	// connection (the response content is irrelevant — only the status
	// code matters). opContext's timeout still bounds total time; this
	// bounds bytes so a hostile endpoint cannot stream unbounded data
	// through the discard copy.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Sprintf("status %d", resp.StatusCode)
	}
	return ""
}

// backoff returns the delay before retry n (n>=1), capped at 30s.
func (d *Dispatcher) backoff(n int) time.Duration {
	delay := d.backoffBase << (n - 1)
	const max = 30 * time.Second
	// The delay <= 0 arm is load-bearing, not just a floor: with an
	// operator-configured high MaxAttempts the shift count (n-1) can
	// reach values where backoffBase << k overflows int64 to a negative
	// duration (k >= ~36) or shifts every bit out to 0 (k >= 64). Both
	// collapse to <= 0 and are clamped to the 30s cap here, so a large
	// MaxAttempts can never produce a negative or zero sleep.
	if delay > max || delay <= 0 {
		return max
	}
	return delay
}

// sleep waits for the given delay, returning false if the dispatcher is
// shutting down before the delay elapses. It selects on the long-lived
// closed channel directly, so it adds no per-event goroutine.
func (d *Dispatcher) sleep(delay time.Duration) bool {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-d.closed:
		return false
	}
}

// opContext returns the context for one config lookup or webhook POST.
// It is bounded by DeliveryTimeout in steady state. Once Close has run,
// it is additionally capped by the drain deadline so a backlog of
// unreachable destinations cannot stall shutdown past ShutdownGrace;
// when the grace is already exhausted the returned context is
// already-expired, so the operation fails fast and the event is
// dead-lettered.
func (d *Dispatcher) opContext() (context.Context, context.CancelFunc) {
	timeout := d.timeout
	select {
	case <-d.closed:
		if remaining := time.Until(d.drainDeadline); remaining < timeout {
			timeout = remaining
		}
	default:
	}
	if timeout < 0 {
		timeout = 0
	}
	return context.WithTimeout(context.Background(), timeout)
}
