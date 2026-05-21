package pieceintegrity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/zeebo/blake3"

	"github.com/kennguy3n/zk-object-fabric/metadata"
)

func blake3Hex(b []byte) string {
	h := blake3.New()
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestVerify_AllHashForms(t *testing.T) {
	body := []byte("zkof-shared-piece-bytes")

	// wantSentinel == nil means Verify should return nil (verified
	// or skipped). Otherwise the returned error must wrap the named
	// sentinel; this distinguishes content-mismatch (502-class) from
	// unrecognised-format (legacy-class) at the API contract level.
	cases := []struct {
		name         string
		hash         string
		body         []byte
		wantSentinel error
	}{
		{"blake3 matches", "blake3:" + blake3Hex(body), body, nil},
		{"blake3 mismatch", "blake3:" + blake3Hex([]byte("expected")), []byte("tampered"), ErrIntegrityCheckFailed},
		{"legacy sha256 matches", sha256Hex(body), body, nil},
		{"legacy sha256 mismatch", sha256Hex([]byte("expected")), []byte("tampered"), ErrIntegrityCheckFailed},
		{"legacy quoted etag matches", `"` + sha256Hex(body) + `"`, body, nil},
		{"legacy quoted etag mismatch", `"` + sha256Hex([]byte("expected")) + `"`, []byte("tampered"), ErrIntegrityCheckFailed},
		{"empty hash skips verification", "", body, nil},
		{"empty hash skips even with empty body", "", []byte{}, nil},
		// Opaque ETags written into the Hash slot are reported as
		// an unrecognised-format observation, not as a content
		// mismatch. The legacy multipart / copy / dedup write
		// paths in this repo stamped a backend ETag into Hash for
		// some manifests; treating those as 502 on every GET
		// would turn legitimate legacy reads into hard failures
		// on upgrade. Callers log + emit a separate observability
		// counter and serve.
		{"opaque etag flagged unrecognised", "etag-xyz", body, ErrIntegrityClaimUnrecognized},
		{"short hex flagged unrecognised", "deadbeef", body, ErrIntegrityClaimUnrecognized},
		{"non-hex same length flagged unrecognised", strings.Repeat("g", sha256HexLen), body, ErrIntegrityClaimUnrecognized},
		{"uppercase hex flagged unrecognised", strings.ToUpper(sha256Hex(body)), body, ErrIntegrityClaimUnrecognized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.body, metadata.Piece{PieceID: "p1", Hash: tc.hash})
			switch {
			case tc.wantSentinel == nil:
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
			default:
				if err == nil {
					t.Fatalf("Verify: want error wrapping %v, got nil", tc.wantSentinel)
				}
				if !errors.Is(err, tc.wantSentinel) {
					t.Fatalf("Verify: error %v not wrapping %v", err, tc.wantSentinel)
				}
				// Make sure the two sentinels don't accidentally
				// alias each other — the whole point of the split
				// is that callers can distinguish them.
				other := ErrIntegrityCheckFailed
				if tc.wantSentinel == ErrIntegrityCheckFailed {
					other = ErrIntegrityClaimUnrecognized
				}
				if errors.Is(err, other) {
					t.Fatalf("Verify: error %v wraps both sentinels", err)
				}
			}
		})
	}
}

func TestHasher_StreamingMatchesVerify(t *testing.T) {
	body := []byte("0123456789abcdef" + strings.Repeat("x", 4096))

	t.Run("blake3 streaming verify matches", func(t *testing.T) {
		piece := metadata.Piece{PieceID: "p1", Hash: "blake3:" + blake3Hex(body)}
		w, check := Hasher(piece)
		// Write in small chunks to exercise the streaming path.
		if _, err := io.Copy(w, splitReader(body, 17)); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if err := check(); err != nil {
			t.Fatalf("check: %v", err)
		}
	})

	t.Run("blake3 streaming rejects tamper after the fact", func(t *testing.T) {
		piece := metadata.Piece{PieceID: "p1", Hash: "blake3:" + blake3Hex([]byte("not-this"))}
		w, check := Hasher(piece)
		if _, err := io.Copy(w, splitReader(body, 17)); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if err := check(); !errors.Is(err, ErrIntegrityCheckFailed) {
			t.Fatalf("check: want ErrIntegrityCheckFailed, got %v", err)
		}
	})

	t.Run("legacy sha256 streaming verify matches", func(t *testing.T) {
		piece := metadata.Piece{PieceID: "p1", Hash: sha256Hex(body)}
		w, check := Hasher(piece)
		if _, err := io.Copy(w, splitReader(body, 41)); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if err := check(); err != nil {
			t.Fatalf("check: %v", err)
		}
	})

	t.Run("legacy quoted etag streaming verify matches", func(t *testing.T) {
		piece := metadata.Piece{PieceID: "p1", Hash: `"` + sha256Hex(body) + `"`}
		w, check := Hasher(piece)
		if _, err := io.Copy(w, splitReader(body, 41)); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if err := check(); err != nil {
			t.Fatalf("check: %v", err)
		}
	})

	t.Run("empty hash is a no-op", func(t *testing.T) {
		piece := metadata.Piece{PieceID: "p1", Hash: ""}
		w, check := Hasher(piece)
		if _, err := io.Copy(w, splitReader(body, 41)); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if err := check(); err != nil {
			t.Fatalf("check: %v", err)
		}
	})

	t.Run("opaque etag surfaces unrecognised format after stream", func(t *testing.T) {
		// The streaming hasher writes to io.Discard while the
		// stream is flowing (so the caller's Tee still feeds
		// its destination), then Check returns the structural
		// observation. The caller decides whether to log it as
		// a legacy-manifest signal (zkof_integrity_claim_unrecognized_total)
		// or treat it as a failure; the verifier itself does
		// not impose a 502 on unrecognised formats.
		piece := metadata.Piece{PieceID: "p1", Hash: "etag-xyz"}
		w, check := Hasher(piece)
		if _, err := io.Copy(w, splitReader(body, 17)); err != nil {
			t.Fatalf("copy: %v", err)
		}
		err := check()
		if !errors.Is(err, ErrIntegrityClaimUnrecognized) {
			t.Fatalf("check: want ErrIntegrityClaimUnrecognized, got %v", err)
		}
		if errors.Is(err, ErrIntegrityCheckFailed) {
			t.Fatalf("check: unrecognised-format error must not also wrap ErrIntegrityCheckFailed: %v", err)
		}
	})
}

// splitReader returns an io.Reader that yields b in chunks of n
// bytes so io.Copy exercises the multi-write streaming path on
// the hasher.
func splitReader(b []byte, n int) io.Reader {
	return &chunkR{buf: b, n: n}
}

type chunkR struct {
	buf []byte
	n   int
}

func (c *chunkR) Read(p []byte) (int, error) {
	if len(c.buf) == 0 {
		return 0, io.EOF
	}
	w := c.n
	if w > len(p) {
		w = len(p)
	}
	if w > len(c.buf) {
		w = len(c.buf)
	}
	copy(p, c.buf[:w])
	c.buf = c.buf[w:]
	return w, nil
}
