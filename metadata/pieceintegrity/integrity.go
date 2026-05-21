// Package pieceintegrity centralises content-hash verification for
// stored pieces so every read path (cache-miss GET, lazy
// read-repair, background rebalancer) uses the same matcher. The
// function recognises the three hash forms the gateway has written
// over time:
//
//  1. "blake3:<hex>"   — the canonical Phase 4 content-derived hash
//                        written by api/s3compat/handler.go's PUT
//                        path after the BLAKE3 piece-integrity work
//                        landed.
//  2. raw SHA-256 hex   — the legacy Phase 2 form (no prefix). Kept
//                        so manifests written before the BLAKE3
//                        cut-over still verify cleanly. The string
//                        must be exactly 64 lowercase hex chars.
//  3. quoted SHA-256    — also legacy; the surrounding quotes are
//                        stripped before the SHA-256 comparison.
//                        Must also be 64 hex chars once stripped.
//
// An empty Piece.Hash skips verification (returns nil) so manifests
// that predate the integrity field continue to serve. A hash that
// is neither blake3-prefixed nor a recognised SHA-256 hex string
// fails closed with ErrIntegrityCheckFailed wrapped in a structural
// error — a manifest with an opaque ETag in the Hash slot is a bug
// (the gateway never writes ETags into Hash on any current
// write-path), so silently accepting one would mask the bug and
// trick operators into trusting an integrity check that never ran.
//
// The package lives under metadata/ rather than api/s3compat or
// migration/lazy_read_repair to keep the import graph one-way:
// every caller already imports metadata, and pulling in
// pieceintegrity does not drag in the HTTP surface or the
// rebalancer machinery.
package pieceintegrity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"

	"github.com/zeebo/blake3"

	"github.com/kennguy3n/zk-object-fabric/metadata"
)

// sha256HexLen is the canonical lowercase hex length of a SHA-256
// digest. Used to reject opaque ETags being misrouted into the
// Hash field.
const sha256HexLen = 64

// isSHA256Hex reports whether s is a 64-character lowercase hex
// string (the legacy SHA-256 form). Used to keep the verifier from
// silently accepting opaque ETags via the legacy fall-through.
func isSHA256Hex(s string) bool {
	if len(s) != sha256HexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// stripQuotes returns s without surrounding ASCII double quotes,
// matching the S3 ETag rendering convention.
func stripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// ErrIntegrityCheckFailed is returned by Verify when a piece body
// does not match its recorded Hash. Callers detect it via
// errors.Is so they can emit metrics / return 502 IntegrityCheckFailed
// without string-matching the message.
var ErrIntegrityCheckFailed = errors.New("pieceintegrity: piece content hash mismatch")

// Verify recomputes the content hash of body and compares it
// against piece.Hash. Returns nil on a match (or when the manifest
// records no hash) and an error wrapping ErrIntegrityCheckFailed
// on a mismatch.
//
// Verify is goroutine-safe and allocation-light: only the per-call
// hasher state is allocated.
func Verify(body []byte, piece metadata.Piece) error {
	if piece.Hash == "" {
		return nil
	}
	if strings.HasPrefix(piece.Hash, "blake3:") {
		expected := piece.Hash[len("blake3:"):]
		h := blake3.New()
		_, _ = h.Write(body)
		got := hex.EncodeToString(h.Sum(nil))
		if got == expected {
			return nil
		}
		return fmt.Errorf("%w: piece %s blake3 mismatch: expected %q got %q",
			ErrIntegrityCheckFailed, piece.PieceID, expected, got)
	}
	expected := stripQuotes(piece.Hash)
	if !isSHA256Hex(expected) {
		return fmt.Errorf("%w: piece %s has unrecognised hash format %q (expected blake3:<hex> or 64-char SHA-256 hex)",
			ErrIntegrityCheckFailed, piece.PieceID, piece.Hash)
	}
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if got == expected {
		return nil
	}
	return fmt.Errorf("%w: piece %s sha256 mismatch: expected %q got %q",
		ErrIntegrityCheckFailed, piece.PieceID, expected, got)
}

// Hasher is the streaming counterpart of Verify. It returns an
// io.Writer that the caller can tee a piece body through (via
// io.TeeReader or io.MultiWriter) while the body is being written
// to its destination, plus a Check() closure that the caller
// invokes after the stream is fully drained.
//
// The returned writer is concrete state, not an interface, so the
// caller pays only one allocation per piece even on the streaming
// path. Hasher selects the algorithm by inspecting piece.Hash:
//
//   - blake3:<hex>  → BLAKE3 hasher.
//   - everything else → SHA-256 hasher (matches the legacy form).
//   - empty Hash → a no-op hasher whose Check always returns nil
//     so the caller does not need to branch.
//
// Check returns an error wrapping ErrIntegrityCheckFailed when the
// streamed bytes do not match piece.Hash. Streaming integrity
// failures are inherently detection-only — the caller has already
// forwarded the bytes to its destination by the time Check fires
// — so the caller is responsible for cleaning up (deleting cached
// data, closing the response uncleanly, emitting metrics).
func Hasher(piece metadata.Piece) (io.Writer, func() error) {
	if piece.Hash == "" {
		return io.Discard, func() error { return nil }
	}
	if strings.HasPrefix(piece.Hash, "blake3:") {
		expected := piece.Hash[len("blake3:"):]
		h := blake3.New()
		return h, func() error {
			got := hex.EncodeToString(h.Sum(nil))
			if got == expected {
				return nil
			}
			return fmt.Errorf("%w: piece %s blake3 mismatch: expected %q got %q",
				ErrIntegrityCheckFailed, piece.PieceID, expected, got)
		}
	}
	expected := stripQuotes(piece.Hash)
	if !isSHA256Hex(expected) {
		// Fail closed: return a hasher that discards the body
		// (so the caller's Tee still works) and a Check that
		// reports the structural error. Streaming callers learn
		// about the misformed hash after the stream completes,
		// matching the post-stream detection-only contract.
		pieceID := piece.PieceID
		rawHash := piece.Hash
		return io.Discard, func() error {
			return fmt.Errorf("%w: piece %s has unrecognised hash format %q (expected blake3:<hex> or 64-char SHA-256 hex)",
				ErrIntegrityCheckFailed, pieceID, rawHash)
		}
	}
	var sha hash.Hash = sha256.New()
	return sha, func() error {
		got := hex.EncodeToString(sha.Sum(nil))
		if got == expected {
			return nil
		}
		return fmt.Errorf("%w: piece %s sha256 mismatch: expected %q got %q",
			ErrIntegrityCheckFailed, piece.PieceID, expected, got)
	}
}
