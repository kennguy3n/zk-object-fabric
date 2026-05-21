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

	cases := []struct {
		name    string
		hash    string
		body    []byte
		wantErr bool
	}{
		{"blake3 matches", "blake3:" + blake3Hex(body), body, false},
		{"blake3 mismatch", "blake3:" + blake3Hex([]byte("expected")), []byte("tampered"), true},
		{"legacy sha256 matches", sha256Hex(body), body, false},
		{"legacy sha256 mismatch", sha256Hex([]byte("expected")), []byte("tampered"), true},
		{"legacy quoted etag matches", `"` + sha256Hex(body) + `"`, body, false},
		{"legacy quoted etag mismatch", `"` + sha256Hex([]byte("expected")) + `"`, []byte("tampered"), true},
		{"empty hash skips verification", "", body, false},
		{"empty hash skips even with empty body", "", []byte{}, false},
		// Opaque ETags written into the Hash slot are rejected
		// rather than silently treated as legacy SHA-256. Before
		// this rule landed, a manifest with an opaque ETag would
		// silently fail to verify (length mismatch with the
		// real SHA-256 produced "different" hashes and returned
		// a confusing error). Failing closed with a structural
		// message points operators at the actual bug - the
		// write path is stamping ETags into Hash, which the
		// gateway never does on any current path.
		{"opaque etag rejected as structural error", "etag-xyz", body, true},
		{"short hex rejected", "deadbeef", body, true},
		{"non-hex same length rejected", strings.Repeat("g", sha256HexLen), body, true},
		{"uppercase hex rejected", strings.ToUpper(sha256Hex(body)), body, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.body, metadata.Piece{PieceID: "p1", Hash: tc.hash})
			if tc.wantErr {
				if err == nil {
					t.Fatal("Verify: want error, got nil")
				}
				if !errors.Is(err, ErrIntegrityCheckFailed) {
					t.Fatalf("Verify: error %v not wrapping ErrIntegrityCheckFailed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify: %v", err)
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

	t.Run("opaque etag rejected after stream", func(t *testing.T) {
		// The streaming hasher writes to io.Discard while the
		// stream is flowing, then Check returns the structural
		// rejection. The caller is expected to clean up the
		// destination (cache row, cached file, etc.) on the
		// error path.
		piece := metadata.Piece{PieceID: "p1", Hash: "etag-xyz"}
		w, check := Hasher(piece)
		if _, err := io.Copy(w, splitReader(body, 17)); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if err := check(); !errors.Is(err, ErrIntegrityCheckFailed) {
			t.Fatalf("check: want ErrIntegrityCheckFailed, got %v", err)
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
