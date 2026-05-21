// Bandwidth-throttled io.Reader for the streaming rebalance path.
// See doc comment on throttledReader below.
package background_rebalancer

import (
	"context"
	"io"

	"golang.org/x/time/rate"
)

// throttledReader wraps an io.Reader and applies a token-bucket
// byte-rate cap inline (per Read call). It exists so the streaming
// rebalancer can pipe a piece source→primary through io.Copy
// without first buffering it in memory; the previous design
// io.ReadAll'd the body and then slept once for size /
// BytesPerSecond, which (a) forced the entire piece into the
// gateway's heap and (b) defeated the point of a streaming copy
// because the destination's PutPiece could not start until the
// source GetPiece had fully drained.
//
// A nil limiter (constructed when BytesPerSecond <= 0) makes
// throttledReader a passthrough: Read returns the underlying
// reader's bytes immediately with no rate work. Otherwise, after
// each underlying Read returns n > 0 bytes the wrapper waits for n
// tokens before returning to the caller. WaitN honours the
// supplied context so a SIGTERM-driven cancel unblocks the
// rebalancer promptly instead of holding it for
// piece_size / BytesPerSecond seconds (the failure mode the
// pre-streaming throttle could exhibit on a large piece against a
// low rate cap).
//
// The burst capacity is sized to one second of throughput with a
// 64 KiB floor: with the default io.Copy buffer of 32 KiB a single
// Read can produce up to 32 KiB of bytes per call, so the burst
// must be at least that many tokens or WaitN would always reject
// the request as oversized. Read also caps each underlying read at
// the limiter's burst so even if a caller passes an unusually
// large buffer the token-bucket request stays within bounds.
type throttledReader struct {
	r       io.Reader
	limiter *rate.Limiter
	ctx     context.Context
}

// minBurstBytes is the floor on the rate limiter's burst capacity.
// It must be at least as large as the io.Copy default buffer
// (32 KiB on go1.x) so the limiter does not reject every
// underlying Read as oversized when BytesPerSecond is small.
const minBurstBytes = 64 * 1024

// newThrottledReader builds a per-Read rate-limited wrapper around
// r. A bytesPerSecond <= 0 disables rate limiting entirely — the
// returned reader is a thin passthrough so the streaming copy
// path stays allocation-light when the operator has not opted
// into a cap.
func newThrottledReader(ctx context.Context, r io.Reader, bytesPerSecond int64) *throttledReader {
	if bytesPerSecond <= 0 {
		return &throttledReader{r: r, ctx: ctx}
	}
	burst := int(bytesPerSecond)
	if burst < minBurstBytes {
		burst = minBurstBytes
	}
	return &throttledReader{
		r:       r,
		ctx:     ctx,
		limiter: rate.NewLimiter(rate.Limit(bytesPerSecond), burst),
	}
}

// Read reads from the underlying source, then blocks (on the
// limiter) until enough tokens have been replenished to cover the
// bytes that were just produced. Returning the read count
// alongside any context-cancellation error mirrors the io.Reader
// contract — callers (chiefly io.Copy) see exactly the bytes that
// landed in p and can decide whether to forward them downstream.
func (t *throttledReader) Read(p []byte) (int, error) {
	if t.limiter == nil {
		return t.r.Read(p)
	}
	// Cap each underlying Read at one burst's worth of bytes so
	// WaitN never returns an "n > burst" error. A caller that
	// hands us a 1 MiB buffer at 100 KiB/s would otherwise see
	// every read fail because the limiter can never grant a
	// 1 MiB request.
	if len(p) > t.limiter.Burst() {
		p = p[:t.limiter.Burst()]
	}
	n, err := t.r.Read(p)
	if n > 0 {
		if waitErr := t.limiter.WaitN(t.ctx, n); waitErr != nil {
			// Return the bytes the underlying reader
			// produced along with the wait error so the
			// caller can flush them downstream if it wants
			// to, but terminate the stream immediately
			// (matches the context-cancellation contract).
			return n, waitErr
		}
	}
	return n, err
}
