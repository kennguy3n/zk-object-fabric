package chaos

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
)

// newBackingProvider returns a fresh local_fs_dev.Provider rooted at
// a fresh temp dir. Used as the "honest" backend that the fault
// wrappers sit in front of so the tests exercise real
// StorageProvider semantics (not a hand-rolled mock).
func newBackingProvider(t *testing.T) providers.StorageProvider {
	t.Helper()
	root := filepath.Join(t.TempDir(), "backing")
	p, err := local_fs_dev.New(root)
	if err != nil {
		t.Fatalf("local_fs_dev.New: %v", err)
	}
	return p
}

func putBytes(t *testing.T, p providers.StorageProvider, pieceID string, body []byte) {
	t.Helper()
	_, err := p.PutPiece(context.Background(), pieceID, bytes.NewReader(body), providers.PutOptions{
		ContentLength: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("seed PutPiece(%s): %v", pieceID, err)
	}
}

func TestFaultProvider_NoneIsPassThrough(t *testing.T) {
	t.Parallel()
	inner := newBackingProvider(t)
	fp := NewFaultProvider(inner)
	putBytes(t, fp, "p1", []byte("hello"))
	rc, err := fp.GetPiece(context.Background(), "p1", nil)
	if err != nil {
		t.Fatalf("GetPiece: %v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "hello" {
		t.Errorf("body = %q, want %q", string(body), "hello")
	}
	if fp.Failures.Load() != 0 {
		t.Errorf("Failures = %d, want 0", fp.Failures.Load())
	}
	if fp.Calls.Load() != 2 {
		t.Errorf("Calls = %d, want 2", fp.Calls.Load())
	}
}

func TestFaultProvider_AlwaysFailUsesConfiguredErr(t *testing.T) {
	t.Parallel()
	inner := newBackingProvider(t)
	sentinel := errors.New("backend exploded")
	fp := NewFaultProvider(inner)
	fp.PutFault = FaultConfig{Mode: ModeAlwaysFail, Err: sentinel}

	for i := 0; i < 3; i++ {
		_, err := fp.PutPiece(context.Background(), "p1", bytes.NewReader([]byte("x")), providers.PutOptions{})
		if !errors.Is(err, sentinel) {
			t.Fatalf("PutPiece[%d] = %v, want %v", i, err, sentinel)
		}
	}
	if fp.Failures.Load() != 3 {
		t.Errorf("Failures = %d, want 3", fp.Failures.Load())
	}
}

func TestFaultProvider_AlwaysFailDefaultsToErrInjectedFault(t *testing.T) {
	t.Parallel()
	inner := newBackingProvider(t)
	fp := NewFaultProvider(inner)
	fp.GetFault = FaultConfig{Mode: ModeAlwaysFail}

	_, err := fp.GetPiece(context.Background(), "any", nil)
	if !errors.Is(err, ErrInjectedFault) {
		t.Errorf("GetPiece err = %v, want ErrInjectedFault", err)
	}
}

func TestFaultProvider_FailEveryNthBurstsAtCadence(t *testing.T) {
	t.Parallel()
	inner := newBackingProvider(t)
	// Seed one piece so GETs have something to read.
	putBytes(t, inner, "p1", []byte("data"))

	fp := NewFaultProvider(inner)
	fp.GetFault = FaultConfig{Mode: ModeFailEveryNth, EveryNth: 3}

	// Calls 1..6: 3 and 6 should fail, the rest pass.
	want := []bool{false, false, true, false, false, true}
	for i, wantFail := range want {
		_, err := fp.GetPiece(context.Background(), "p1", nil)
		gotFail := err != nil
		if gotFail != wantFail {
			t.Errorf("call %d: failed=%v, want %v (err=%v)", i+1, gotFail, wantFail, err)
		}
	}
	if got, want := fp.Failures.Load(), int64(2); got != want {
		t.Errorf("Failures = %d, want %d", got, want)
	}
}

func TestFaultProvider_FailFirstNRecoversAfter(t *testing.T) {
	t.Parallel()
	inner := newBackingProvider(t)
	putBytes(t, inner, "p1", []byte("data"))

	fp := NewFaultProvider(inner)
	fp.GetFault = FaultConfig{Mode: ModeFailFirstN, FirstN: 4}

	for i := 1; i <= 4; i++ {
		_, err := fp.GetPiece(context.Background(), "p1", nil)
		if err == nil {
			t.Errorf("call %d: want failure during first-N window", i)
		}
	}
	for i := 5; i <= 8; i++ {
		rc, err := fp.GetPiece(context.Background(), "p1", nil)
		if err != nil {
			t.Errorf("call %d: want recovered, got %v", i, err)
			continue
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
}

func TestFaultProvider_FailUntilTimeHealsAtDeadline(t *testing.T) {
	t.Parallel()
	inner := newBackingProvider(t)
	putBytes(t, inner, "p1", []byte("data"))

	var now atomic.Int64
	now.Store(int64(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()))
	fp := NewFaultProvider(inner)
	fp.Now = func() time.Time { return time.Unix(0, now.Load()) }
	healTime := time.Unix(0, now.Load()).Add(5 * time.Second)
	fp.GetFault = FaultConfig{Mode: ModeFailUntilTime, FailUntil: healTime}

	// Pre-heal: faults.
	if _, err := fp.GetPiece(context.Background(), "p1", nil); err == nil {
		t.Error("pre-heal GetPiece: want failure")
	}
	// Advance the clock past the heal time.
	now.Store(healTime.UnixNano())
	rc, err := fp.GetPiece(context.Background(), "p1", nil)
	if err != nil {
		t.Fatalf("post-heal GetPiece: %v", err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}

func TestFaultProvider_TruncatedReadDeliversPartialBodyThenFails(t *testing.T) {
	t.Parallel()
	inner := newBackingProvider(t)
	body := bytes.Repeat([]byte("abcdef"), 64) // 384 bytes
	putBytes(t, inner, "p1", body)

	fp := NewFaultProvider(inner)
	fp.GetFault = FaultConfig{
		Mode:               ModeTruncatedRead,
		TruncateAfterBytes: 100,
		Err:                errors.New("connection reset"),
	}
	rc, err := fp.GetPiece(context.Background(), "p1", nil)
	if err != nil {
		t.Fatalf("GetPiece: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err == nil || err.Error() != "connection reset" {
		t.Errorf("ReadAll err = %v, want connection reset", err)
	}
	if int64(len(got)) != 100 {
		t.Errorf("got %d bytes, want exactly 100 before truncation", len(got))
	}
	if !bytes.Equal(got, body[:100]) {
		t.Errorf("truncated prefix mismatches the real bytes")
	}
}

// TestFaultProvider_TruncatedReadWithZeroBudgetPassesThrough is the
// regression for Devin Review finding BUG_pr-review-job-…_0001:
// a FaultConfig with Mode=ModeTruncatedRead but TruncateAfterBytes
// unset (or <= 0) previously hard-errored on the very first Read()
// with 0 bytes delivered, contradicting the documented "no
// truncation" semantics. The fix returns the inner reader
// unwrapped in that case so the read completes against the
// provider's natural EOF.
func TestFaultProvider_TruncatedReadWithZeroBudgetPassesThrough(t *testing.T) {
	t.Parallel()
	inner := newBackingProvider(t)
	body := []byte("zero-budget-pass-through")
	putBytes(t, inner, "p1", body)

	// Subtest 1: explicit Mode but TruncateAfterBytes left at the
	// zero value — must NOT error, must deliver the full body.
	t.Run("zero_budget", func(t *testing.T) {
		fp := NewFaultProvider(inner)
		fp.GetFault = FaultConfig{
			Mode: ModeTruncatedRead,
			Err:  errors.New("should never surface"),
		}
		rc, err := fp.GetPiece(context.Background(), "p1", nil)
		if err != nil {
			t.Fatalf("GetPiece returned err=%v; with TruncateAfterBytes<=0 "+
				"the inner reader must be returned unwrapped", err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("ReadAll err=%v; want nil (no truncation budget = "+
				"natural EOF, not synthesized failure)", err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("got %q want %q — pass-through must yield the full body", got, body)
		}
		if fp.Failures.Load() != 0 {
			t.Errorf("Failures = %d; want 0 (no fault was actually "+
				"injected because the budget was zero)", fp.Failures.Load())
		}
	})

	// Subtest 2: negative budget — same pass-through behaviour.
	t.Run("negative_budget", func(t *testing.T) {
		fp := NewFaultProvider(inner)
		fp.GetFault = FaultConfig{
			Mode:               ModeTruncatedRead,
			TruncateAfterBytes: -42,
			Err:                errors.New("should never surface"),
		}
		rc, err := fp.GetPiece(context.Background(), "p1", nil)
		if err != nil {
			t.Fatalf("GetPiece err=%v with negative budget", err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Errorf("ReadAll err=%v with negative budget", err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("got %q want %q", got, body)
		}
	})
}

func TestFaultProvider_SlowResponseInjectsLatency(t *testing.T) {
	t.Parallel()
	inner := newBackingProvider(t)
	putBytes(t, inner, "p1", []byte("data"))

	var slept time.Duration
	fp := NewFaultProvider(inner)
	fp.Sleep = func(d time.Duration) { slept += d }
	fp.GetFault = FaultConfig{Mode: ModeSlowResponse, Latency: 250 * time.Millisecond}

	rc, err := fp.GetPiece(context.Background(), "p1", nil)
	if err != nil {
		t.Fatalf("GetPiece: %v", err)
	}
	_ = rc.Close()
	if slept != 250*time.Millisecond {
		t.Errorf("slept = %s, want 250ms", slept)
	}
}

func TestFaultProvider_NilInnerYieldsClearError(t *testing.T) {
	t.Parallel()
	fp := &FaultProvider{}
	fp.PutFault = FaultConfig{Mode: ModeAlwaysFail}
	// Even without ModeAlwaysFail the nil-Inner check fires before
	// the inner call is attempted.
	_, err := fp.PutPiece(context.Background(), "p1", bytes.NewReader([]byte("x")), providers.PutOptions{})
	if err == nil {
		t.Fatal("PutPiece on nil Inner: want error, got nil")
	}
}

func TestFaultProvider_ConcurrentCallsShareCounter(t *testing.T) {
	t.Parallel()
	inner := newBackingProvider(t)
	putBytes(t, inner, "p1", []byte("data"))

	fp := NewFaultProvider(inner)
	fp.GetFault = FaultConfig{Mode: ModeFailEveryNth, EveryNth: 10}

	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			rc, err := fp.GetPiece(context.Background(), "p1", nil)
			if err == nil {
				_, _ = io.Copy(io.Discard, rc)
				_ = rc.Close()
			}
		}()
	}
	wg.Wait()
	// EveryNth=10 over 200 calls => exactly 20 failures, regardless
	// of interleaving, because the counter is shared and monotonic.
	if got, want := fp.Failures.Load(), int64(20); got != want {
		t.Errorf("Failures = %d, want %d", got, want)
	}
	if got, want := fp.Calls.Load(), int64(N); got != want {
		t.Errorf("Calls = %d, want %d", got, want)
	}
}

func TestFaultProvider_DelegatesMetadataCalls(t *testing.T) {
	t.Parallel()
	inner := newBackingProvider(t)
	fp := NewFaultProvider(inner)
	// All non-data calls should pass through even when every data
	// call is faulted — metadata should still be readable for the
	// gateway to decide what to do.
	fp.PutFault = FaultConfig{Mode: ModeAlwaysFail}
	fp.GetFault = FaultConfig{Mode: ModeAlwaysFail}
	if got := fp.Capabilities(); got != inner.Capabilities() {
		t.Errorf("Capabilities mismatch: %#v vs %#v", got, inner.Capabilities())
	}
	if got := fp.CostModel(); got != inner.CostModel() {
		t.Errorf("CostModel mismatch: %#v vs %#v", got, inner.CostModel())
	}
	if got := fp.PlacementLabels(); got.Provider != inner.PlacementLabels().Provider {
		t.Errorf("PlacementLabels.Provider mismatch")
	}
}
