// Package conformance is the reusable StorageProvider conformance
// suite. Any backend that implements providers.StorageProvider must
// pass Run.
//
// The suite is parameterised by a factory so the same tests can be
// pointed at local_fs_dev, wasabi (via an in-memory S3 fake in CI or
// real credentials offline), and any other adapter added later.
package conformance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// Factory constructs a fresh StorageProvider for a single subtest.
// Implementations should return a provider with an empty namespace so
// tests can assume the backend starts clean.
type Factory func(t *testing.T) providers.StorageProvider

// Options tune the suite for backends with adapter-specific quirks.
// The defaults target the strictest interpretation of the interface
// (local_fs_dev); S3-backed adapters widen the envelope where S3's
// own semantics differ.
type Options struct {
	// SkipUnsafePieceIDs skips the filesystem-traversal rejection
	// test. S3-compatible backends accept arbitrary keys (including
	// "/" and "..") so the defense-in-depth check only applies to
	// filesystem providers.
	SkipUnsafePieceIDs bool

	// SkipDeleteMissingError skips the assertion that
	// DeletePiece(missing) returns an error. S3 DeleteObject is
	// idempotent by spec and does not distinguish delete-of-missing
	// from delete-of-existing.
	SkipDeleteMissingError bool
}

// Run executes the full conformance suite. It MUST be called from a
// test function; every assertion uses t.Fatalf.
func Run(t *testing.T, factory Factory, opts Options) {
	t.Helper()
	t.Run("PutGetRoundTrip", func(t *testing.T) { testPutGetRoundTrip(t, factory) })
	t.Run("HeadPiece", func(t *testing.T) { testHeadPiece(t, factory) })
	t.Run("DeletePiece", func(t *testing.T) { testDeletePiece(t, factory, opts) })
	t.Run("ListPieces", func(t *testing.T) { testListPieces(t, factory) })
	t.Run("ByteRange", func(t *testing.T) { testByteRange(t, factory) })
	t.Run("IfNoneMatch", func(t *testing.T) { testIfNoneMatch(t, factory) })
	t.Run("MissingPieceErrors", func(t *testing.T) { testMissingPieceErrors(t, factory) })
	if !opts.SkipUnsafePieceIDs {
		t.Run("RejectsUnsafePieceIDs", func(t *testing.T) { testRejectsUnsafePieceIDs(t, factory) })
	}
	t.Run("DescriptiveMethods", func(t *testing.T) { testDescriptiveMethods(t, factory) })
	t.Run("MaxBytesErrorPreserved", func(t *testing.T) { testMaxBytesErrorPreserved(t, factory) })
}

func testPutGetRoundTrip(t *testing.T, factory Factory) {
	ctx := context.Background()
	p := factory(t)

	payload := []byte("zk-object-fabric conformance payload")
	res, err := p.PutPiece(ctx, "piece-001", bytes.NewReader(payload), providers.PutOptions{
		ContentLength: int64(len(payload)),
		ContentType:   "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("PutPiece: %v", err)
	}
	if res.SizeBytes != int64(len(payload)) {
		t.Fatalf("PutPiece size = %d, want %d", res.SizeBytes, len(payload))
	}

	rc, err := p.GetPiece(ctx, "piece-001", nil)
	if err != nil {
		t.Fatalf("GetPiece: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read piece: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, payload)
	}
}

func testHeadPiece(t *testing.T, factory Factory) {
	ctx := context.Background()
	p := factory(t)

	payload := []byte("head-me")
	if _, err := p.PutPiece(ctx, "piece-head", bytes.NewReader(payload), providers.PutOptions{
		ContentLength: int64(len(payload)),
		ContentType:   "text/plain",
		StorageClass:  "standard",
		Metadata:      map[string]string{"origin": "test"},
	}); err != nil {
		t.Fatalf("PutPiece: %v", err)
	}

	md, err := p.HeadPiece(ctx, "piece-head")
	if err != nil {
		t.Fatalf("HeadPiece: %v", err)
	}
	if md.SizeBytes != int64(len(payload)) {
		t.Fatalf("HeadPiece size = %d, want %d", md.SizeBytes, len(payload))
	}
	if md.ContentType != "text/plain" {
		t.Fatalf("HeadPiece ContentType = %q, want %q", md.ContentType, "text/plain")
	}
	if md.Metadata["origin"] != "test" {
		t.Fatalf("HeadPiece Metadata[origin] = %q, want %q", md.Metadata["origin"], "test")
	}
}

func testDeletePiece(t *testing.T, factory Factory, opts Options) {
	ctx := context.Background()
	p := factory(t)

	if _, err := p.PutPiece(ctx, "piece-delete", bytes.NewReader([]byte("bye")), providers.PutOptions{}); err != nil {
		t.Fatalf("PutPiece: %v", err)
	}
	if err := p.DeletePiece(ctx, "piece-delete"); err != nil {
		t.Fatalf("DeletePiece: %v", err)
	}
	if _, err := p.HeadPiece(ctx, "piece-delete"); err == nil {
		t.Fatal("HeadPiece after delete: want error, got nil")
	}
	if !opts.SkipDeleteMissingError {
		if err := p.DeletePiece(ctx, "piece-delete"); err == nil {
			t.Fatal("DeletePiece of missing piece: want error, got nil")
		}
	}
}

func testListPieces(t *testing.T, factory Factory) {
	ctx := context.Background()
	p := factory(t)

	ids := []string{"alpha-1", "alpha-2", "beta-1"}
	for _, id := range ids {
		if _, err := p.PutPiece(ctx, id, bytes.NewReader([]byte(id)), providers.PutOptions{}); err != nil {
			t.Fatalf("PutPiece %q: %v", id, err)
		}
	}

	lr, err := p.ListPieces(ctx, "alpha-", "")
	if err != nil {
		t.Fatalf("ListPieces: %v", err)
	}
	if len(lr.Pieces) != 2 {
		t.Fatalf("ListPieces alpha-: got %d pieces, want 2", len(lr.Pieces))
	}
	for _, pm := range lr.Pieces {
		if !strings.HasPrefix(pm.PieceID, "alpha-") {
			t.Fatalf("ListPieces returned out-of-prefix piece %q", pm.PieceID)
		}
	}
}

func testByteRange(t *testing.T, factory Factory) {
	ctx := context.Background()
	p := factory(t)

	payload := []byte("0123456789")
	if _, err := p.PutPiece(ctx, "piece-range", bytes.NewReader(payload), providers.PutOptions{
		ContentLength: int64(len(payload)),
	}); err != nil {
		t.Fatalf("PutPiece: %v", err)
	}

	cases := []struct {
		name  string
		start int64
		end   int64
		want  string
	}{
		{"prefix", 0, 3, "0123"},
		{"middle", 4, 6, "456"},
		{"tail-open", 7, -1, "789"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, err := p.GetPiece(ctx, "piece-range", &providers.ByteRange{Start: tc.start, End: tc.end})
			if err != nil {
				t.Fatalf("GetPiece: %v", err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read range: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("range [%d,%d] = %q, want %q", tc.start, tc.end, got, tc.want)
			}
		})
	}
}

func testIfNoneMatch(t *testing.T, factory Factory) {
	ctx := context.Background()
	p := factory(t)

	if _, err := p.PutPiece(ctx, "piece-once", bytes.NewReader([]byte("x")), providers.PutOptions{}); err != nil {
		t.Fatalf("PutPiece first: %v", err)
	}
	_, err := p.PutPiece(ctx, "piece-once", bytes.NewReader([]byte("x")), providers.PutOptions{IfNoneMatch: true})
	if err == nil {
		t.Fatal("PutPiece IfNoneMatch on existing piece: want error, got nil")
	}
}

func testMissingPieceErrors(t *testing.T, factory Factory) {
	ctx := context.Background()
	p := factory(t)

	if _, err := p.GetPiece(ctx, "does-not-exist", nil); err == nil {
		t.Fatal("GetPiece missing: want error, got nil")
	} else if errors.Is(err, io.EOF) {
		t.Fatal("GetPiece missing returned EOF, want a not-found error")
	}
	if _, err := p.HeadPiece(ctx, "does-not-exist"); err == nil {
		t.Fatal("HeadPiece missing: want error, got nil")
	}
}

func testRejectsUnsafePieceIDs(t *testing.T, factory Factory) {
	ctx := context.Background()
	p := factory(t)

	unsafe := []string{
		"../escape",
		"..",
		".",
		".hidden",
		"nested/id",
		`back\slash`,
		"",
	}
	for _, id := range unsafe {
		t.Run("put/"+id, func(t *testing.T) {
			if _, err := p.PutPiece(ctx, id, bytes.NewReader([]byte("x")), providers.PutOptions{}); err == nil {
				t.Fatalf("PutPiece(%q): want error, got nil", id)
			}
			if _, err := p.GetPiece(ctx, id, nil); err == nil {
				t.Fatalf("GetPiece(%q): want error, got nil", id)
			}
			if _, err := p.HeadPiece(ctx, id); err == nil {
				t.Fatalf("HeadPiece(%q): want error, got nil", id)
			}
			if err := p.DeletePiece(ctx, id); err == nil {
				t.Fatalf("DeletePiece(%q): want error, got nil", id)
			}
		})
	}
}

// testMaxBytesErrorPreserved pins a defense-in-depth contract on
// every StorageProvider: when the gateway hands PutPiece an
// io.Reader that is actually an http.MaxBytesReader-wrapped
// request body, and the client overruns the cap mid-stream, the
// error returned by PutPiece MUST still be unwrappable to a
// *http.MaxBytesError via errors.As. Without this guarantee, the
// s3compat handler's writePutPieceError helper would silently
// degrade a 413 EntityTooLarge into a generic 502 BackendPutFailed
// for any provider that wraps its underlying error with `%v`
// instead of `%w` somewhere in the chain.
//
// The test simulates the production wiring exactly: a fresh
// ResponseRecorder + MaxBytesReader-wrapped reader, the same way
// dispatch wraps r.Body at the top of every PUT/POST. The
// provider then attempts to read from it; the read fails with
// MaxBytesError, and the test asserts the returned error chain
// still resolves to *http.MaxBytesError. This catches a future
// provider that, e.g., does fmt.Errorf("put: %v", err) instead of
// `%w` — the test would fail immediately rather than the
// regression manifesting as a 502 in production.
func testMaxBytesErrorPreserved(t *testing.T, factory Factory) {
	ctx := context.Background()
	p := factory(t)

	const limit int64 = 16
	// Body is intentionally larger than the limit so the
	// MaxBytesReader will fire mid-read. ContentLength is set to
	// -1 below to bypass the stdlib short-circuit that rejects
	// the request before the reader runs.
	payload := bytes.Repeat([]byte("M"), int(limit)*4)
	req := httptest.NewRequest(http.MethodPut, "/conformance", bytes.NewReader(payload))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	wrapped := http.MaxBytesReader(rec, req.Body, limit)

	_, err := p.PutPiece(ctx, "piece-maxbytes", wrapped, providers.PutOptions{
		// ContentLength: -1 instructs the provider to stream
		// without pre-declaring the size; matches the
		// production single-piece Put path that hands the
		// MaxBytesReader directly through.
		ContentLength: -1,
	})
	if err == nil {
		t.Fatal("PutPiece with overflowing MaxBytesReader: want error, got nil")
	}

	var mb *http.MaxBytesError
	if !errors.As(err, &mb) {
		t.Fatalf("errors.As(*http.MaxBytesError) = false; provider must wrap underlying error with %%w so the s3compat handler can surface 413 EntityTooLarge. Got err type %T: %v", err, err)
	}
	if mb.Limit != limit {
		t.Errorf("MaxBytesError.Limit = %d, want %d (provider must not synthesize a fresh MaxBytesError — the original must be preserved through the error chain)", mb.Limit, limit)
	}
}

func testDescriptiveMethods(t *testing.T, factory Factory) {
	p := factory(t)
	caps := p.Capabilities()
	if !caps.SupportsRangeReads {
		t.Fatal("Capabilities.SupportsRangeReads: want true")
	}
	labels := p.PlacementLabels()
	if labels.Provider == "" {
		t.Fatal("PlacementLabels.Provider: want non-empty")
	}
	_ = p.CostModel()
}
