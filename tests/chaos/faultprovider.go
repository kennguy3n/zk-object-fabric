package chaos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// FaultMode describes how a FaultProvider should fail a single
// operation. The zero value (ModeNone) means "delegate to the
// underlying provider untouched".
type FaultMode int

const (
	// ModeNone passes the call through to the wrapped provider
	// unchanged.
	ModeNone FaultMode = iota

	// ModeAlwaysFail returns the configured error on every call.
	// Used to model "this backend is gone" — Wasabi rate-limit
	// trip, full origin outage, NVMe-mount unmounted.
	ModeAlwaysFail

	// ModeFailEveryNth returns the configured error on every Nth
	// call (1-indexed: N=2 → calls 2, 4, 6, ... fail). Used to
	// model intermittent 5xx storms where retries SHOULD succeed.
	ModeFailEveryNth

	// ModeFailFirstN fails the first N calls and passes through
	// after that. Used to model a transient outage that resolves
	// by itself.
	ModeFailFirstN

	// ModeFailUntilTime fails until the configured wall-clock
	// time, then passes through. Used to simulate a partition that
	// heals at a known instant.
	ModeFailUntilTime

	// ModeTruncatedRead returns the wrapped provider's reader but
	// truncated to the first N bytes, followed by a read error.
	// Used to model a network partition mid-GET.
	ModeTruncatedRead

	// ModeSlowResponse delegates to the wrapped provider but adds
	// the configured Latency before returning. Used to model a
	// degraded backend that drives the gateway to its timeout
	// budget without actually erroring.
	ModeSlowResponse
)

// String renders the mode for log lines and test failure messages.
func (m FaultMode) String() string {
	switch m {
	case ModeNone:
		return "none"
	case ModeAlwaysFail:
		return "always_fail"
	case ModeFailEveryNth:
		return "fail_every_nth"
	case ModeFailFirstN:
		return "fail_first_n"
	case ModeFailUntilTime:
		return "fail_until_time"
	case ModeTruncatedRead:
		return "truncated_read"
	case ModeSlowResponse:
		return "slow_response"
	default:
		return fmt.Sprintf("unknown(%d)", int(m))
	}
}

// FaultConfig describes the fault to inject into one of the
// StorageProvider methods. The zero value injects nothing.
type FaultConfig struct {
	// Mode is the fault to apply. ModeNone passes the call
	// through.
	Mode FaultMode

	// Err is the error returned to the caller on a faulted call.
	// Required when Mode is ModeAlwaysFail, ModeFailEveryNth,
	// ModeFailFirstN, ModeFailUntilTime, or ModeTruncatedRead. If
	// nil, a default ErrInjectedFault is returned so tests that
	// only care that "something failed" still get a clear signal.
	Err error

	// EveryNth is the call cadence for ModeFailEveryNth.
	// A value <= 1 fails every call (equivalent to ModeAlwaysFail
	// with the same Err).
	EveryNth int

	// FirstN is the prefix length for ModeFailFirstN. Calls 1..N
	// fail; calls N+1.. pass through. A value <= 0 is a no-op.
	FirstN int

	// FailUntil is the deadline for ModeFailUntilTime. Calls made
	// strictly before FailUntil fail; calls at or after it pass
	// through.
	FailUntil time.Time

	// TruncateAfterBytes is the byte budget for ModeTruncatedRead.
	// A value <= 0 falls back to the io.EOF behavior of the
	// underlying reader (i.e. no actual truncation).
	TruncateAfterBytes int64

	// Latency is the artificial latency for ModeSlowResponse, and
	// is also injected for every faulted call regardless of mode
	// when non-zero. Used to model a degraded backend.
	Latency time.Duration
}

// ErrInjectedFault is the default error returned when a FaultConfig
// triggers without an explicit Err. Tests should use errors.Is to
// detect it rather than string comparison.
var ErrInjectedFault = errors.New("chaos: injected fault")

// FaultProvider wraps a providers.StorageProvider with per-operation
// fault injection. The zero value passes every call through.
//
// FaultProvider is safe for concurrent use. The call counter used by
// ModeFailEveryNth and ModeFailFirstN is shared across all goroutines
// for a given operation kind, which matches the operational reality
// of an upstream rate limiter or shared circuit breaker.
type FaultProvider struct {
	// Inner is the wrapped provider. Required.
	Inner providers.StorageProvider

	// Now is the clock used by ModeFailUntilTime and for any
	// Latency injection. Defaults to time.Now if nil. Tests
	// override this to keep behaviour deterministic.
	Now func() time.Time

	// Sleep is the time-skip primitive used by ModeSlowResponse
	// and any Latency injection. Defaults to time.Sleep if nil.
	// Tests override this to avoid real sleeps.
	Sleep func(time.Duration)

	// Per-operation faults. Each method consults exactly one of
	// these. Embedding by-value keeps the zero-value usage clean.
	PutFault    FaultConfig
	GetFault    FaultConfig
	HeadFault   FaultConfig
	DeleteFault FaultConfig
	ListFault   FaultConfig

	mu       sync.Mutex
	counters map[string]int64

	// Calls is the total number of operations dispatched to this
	// provider, success or failure. Useful for assertions like
	// "the gateway retried at least 3 times".
	Calls atomic.Int64

	// Failures is the number of operations the fault injector
	// turned into an error. Always <= Calls.
	Failures atomic.Int64
}

// NewFaultProvider returns a FaultProvider wrapping inner. inner must
// be non-nil; passing nil is a programmer error and is caught by every
// downstream call.
func NewFaultProvider(inner providers.StorageProvider) *FaultProvider {
	return &FaultProvider{Inner: inner}
}

func (p *FaultProvider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *FaultProvider) sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	if p.Sleep != nil {
		p.Sleep(d)
		return
	}
	time.Sleep(d)
}

// shouldFail consults the FaultConfig and decides whether this call
// should be faulted. It is the single source of truth for fault
// triggering so the semantics stay consistent across the five
// StorageProvider methods.
func (p *FaultProvider) shouldFail(op string, cfg FaultConfig) (bool, error) {
	if p.Inner == nil {
		return true, errors.New("chaos: FaultProvider.Inner is nil")
	}
	switch cfg.Mode {
	case ModeNone:
		return false, nil
	case ModeAlwaysFail:
		return true, faultErr(cfg)
	case ModeFailEveryNth:
		n := cfg.EveryNth
		if n <= 1 {
			return true, faultErr(cfg)
		}
		c := p.bumpCounter(op)
		if c%int64(n) == 0 {
			return true, faultErr(cfg)
		}
		return false, nil
	case ModeFailFirstN:
		if cfg.FirstN <= 0 {
			return false, nil
		}
		c := p.bumpCounter(op)
		if c <= int64(cfg.FirstN) {
			return true, faultErr(cfg)
		}
		return false, nil
	case ModeFailUntilTime:
		if p.now().Before(cfg.FailUntil) {
			return true, faultErr(cfg)
		}
		return false, nil
	case ModeTruncatedRead:
		// Handled inside the GET path; PUT/HEAD/DELETE/LIST treat
		// it as ModeNone because there is no read stream to
		// truncate. Tests that misconfigure this on a non-GET
		// operation get the pass-through behaviour rather than a
		// silent panic.
		return false, nil
	case ModeSlowResponse:
		// Pass-through with extra latency; handled by the per-op
		// latency injection below.
		return false, nil
	default:
		return true, fmt.Errorf("chaos: unknown FaultMode %s", cfg.Mode)
	}
}

func faultErr(cfg FaultConfig) error {
	if cfg.Err != nil {
		return cfg.Err
	}
	return ErrInjectedFault
}

func (p *FaultProvider) bumpCounter(op string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.counters == nil {
		p.counters = map[string]int64{}
	}
	p.counters[op]++
	return p.counters[op]
}

// applyLatency injects the configured latency for cfg, either because
// the mode is ModeSlowResponse or because the caller asked for a
// generic Latency budget on top of a different mode.
func (p *FaultProvider) applyLatency(cfg FaultConfig) {
	switch {
	case cfg.Mode == ModeSlowResponse:
		p.sleep(cfg.Latency)
	case cfg.Latency > 0:
		p.sleep(cfg.Latency)
	}
}

// PutPiece dispatches through the PutFault config.
func (p *FaultProvider) PutPiece(ctx context.Context, pieceID string, r io.Reader, opts providers.PutOptions) (providers.PutResult, error) {
	p.Calls.Add(1)
	p.applyLatency(p.PutFault)
	fail, err := p.shouldFail("PUT", p.PutFault)
	if fail {
		p.Failures.Add(1)
		return providers.PutResult{}, err
	}
	return p.Inner.PutPiece(ctx, pieceID, r, opts)
}

// GetPiece dispatches through the GetFault config. ModeTruncatedRead
// is handled here because it requires wrapping the returned
// ReadCloser; every other mode is handled by shouldFail.
func (p *FaultProvider) GetPiece(ctx context.Context, pieceID string, byteRange *providers.ByteRange) (io.ReadCloser, error) {
	p.Calls.Add(1)
	p.applyLatency(p.GetFault)
	if p.GetFault.Mode == ModeTruncatedRead {
		rc, err := p.Inner.GetPiece(ctx, pieceID, byteRange)
		if err != nil {
			return nil, err
		}
		p.Failures.Add(1)
		return &truncatedReader{
			inner:     rc,
			remaining: p.GetFault.TruncateAfterBytes,
			tripErr:   faultErr(p.GetFault),
		}, nil
	}
	fail, err := p.shouldFail("GET", p.GetFault)
	if fail {
		p.Failures.Add(1)
		return nil, err
	}
	return p.Inner.GetPiece(ctx, pieceID, byteRange)
}

// HeadPiece dispatches through the HeadFault config.
func (p *FaultProvider) HeadPiece(ctx context.Context, pieceID string) (providers.PieceMetadata, error) {
	p.Calls.Add(1)
	p.applyLatency(p.HeadFault)
	fail, err := p.shouldFail("HEAD", p.HeadFault)
	if fail {
		p.Failures.Add(1)
		return providers.PieceMetadata{}, err
	}
	return p.Inner.HeadPiece(ctx, pieceID)
}

// DeletePiece dispatches through the DeleteFault config.
func (p *FaultProvider) DeletePiece(ctx context.Context, pieceID string) error {
	p.Calls.Add(1)
	p.applyLatency(p.DeleteFault)
	fail, err := p.shouldFail("DELETE", p.DeleteFault)
	if fail {
		p.Failures.Add(1)
		return err
	}
	return p.Inner.DeletePiece(ctx, pieceID)
}

// ListPieces dispatches through the ListFault config.
func (p *FaultProvider) ListPieces(ctx context.Context, prefix, cursor string) (providers.ListResult, error) {
	p.Calls.Add(1)
	p.applyLatency(p.ListFault)
	fail, err := p.shouldFail("LIST", p.ListFault)
	if fail {
		p.Failures.Add(1)
		return providers.ListResult{}, err
	}
	return p.Inner.ListPieces(ctx, prefix, cursor)
}

// Capabilities delegates without fault injection. Capability lookups
// are advisory metadata and faulting them would just cause every
// caller to misbehave, not exercise a useful failure mode.
func (p *FaultProvider) Capabilities() providers.ProviderCapabilities {
	if p.Inner == nil {
		return providers.ProviderCapabilities{}
	}
	return p.Inner.Capabilities()
}

// CostModel delegates without fault injection.
func (p *FaultProvider) CostModel() providers.ProviderCostModel {
	if p.Inner == nil {
		return providers.ProviderCostModel{}
	}
	return p.Inner.CostModel()
}

// PlacementLabels delegates without fault injection.
func (p *FaultProvider) PlacementLabels() providers.PlacementLabels {
	if p.Inner == nil {
		return providers.PlacementLabels{}
	}
	return p.Inner.PlacementLabels()
}

// truncatedReader wraps an io.ReadCloser and returns at most remaining
// bytes, then trips with tripErr. Used by ModeTruncatedRead to
// simulate a network partition mid-stream — the caller has already
// received headers and started reading bytes when the connection
// breaks.
type truncatedReader struct {
	inner     io.ReadCloser
	remaining int64
	tripErr   error
}

func (t *truncatedReader) Read(p []byte) (int, error) {
	if t.remaining <= 0 {
		return 0, t.tripErr
	}
	if int64(len(p)) > t.remaining {
		p = p[:t.remaining]
	}
	n, err := t.inner.Read(p)
	t.remaining -= int64(n)
	if t.remaining <= 0 && err == nil {
		err = t.tripErr
	}
	return n, err
}

func (t *truncatedReader) Close() error {
	return t.inner.Close()
}
