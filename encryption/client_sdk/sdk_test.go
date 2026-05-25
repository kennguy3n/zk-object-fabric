package client_sdk

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/kennguy3n/zk-object-fabric/encryption"
)

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		size      int
		chunkSize int
	}{
		{"empty", 0, 1024},
		{"smaller-than-chunk", 512, 1024},
		{"exact-chunk", 1024, 1024},
		{"multi-chunk", 4096, 1024},
		{"odd-tail", 3333, 1024},
		{"default-chunk-small", 1_500_000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dek, err := GenerateDEK()
			if err != nil {
				t.Fatalf("GenerateDEK: %v", err)
			}
			plain := make([]byte, tc.size)
			if _, err := io.ReadFull(rand.Reader, plain); err != nil {
				t.Fatalf("fill plaintext: %v", err)
			}

			encR, err := EncryptObject(bytes.NewReader(plain), dek, Options{ChunkSize: tc.chunkSize})
			if err != nil {
				t.Fatalf("EncryptObject: %v", err)
			}
			ct, err := io.ReadAll(encR)
			if err != nil {
				t.Fatalf("read ciphertext: %v", err)
			}
			if tc.size > 0 && bytes.Equal(ct, plain) {
				t.Fatal("ciphertext equals plaintext")
			}

			decR, err := DecryptObject(bytes.NewReader(ct), dek, Options{ChunkSize: tc.chunkSize})
			if err != nil {
				t.Fatalf("DecryptObject: %v", err)
			}
			got, err := io.ReadAll(decR)
			if err != nil {
				t.Fatalf("read plaintext: %v", err)
			}
			if !bytes.Equal(got, plain) {
				t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(plain))
			}
		})
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	other, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}

	plain := []byte("confidential payload; wrong key must fail")
	encR, err := EncryptObject(bytes.NewReader(plain), dek, Options{ChunkSize: 16})
	if err != nil {
		t.Fatalf("EncryptObject: %v", err)
	}
	ct, err := io.ReadAll(encR)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	decR, err := DecryptObject(bytes.NewReader(ct), other, Options{ChunkSize: 16})
	if err != nil {
		t.Fatalf("DecryptObject: %v", err)
	}
	if _, err := io.ReadAll(decR); err == nil {
		t.Fatal("DecryptObject with wrong key: want auth error, got nil")
	}
}

func TestDecrypt_TamperedCiphertext(t *testing.T) {
	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	plain := []byte("tamper-evident payload")
	encR, err := EncryptObject(bytes.NewReader(plain), dek, Options{ChunkSize: 8})
	if err != nil {
		t.Fatalf("EncryptObject: %v", err)
	}
	ct, err := io.ReadAll(encR)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	// Flip a byte in the body of the first frame (past the header).
	if len(ct) <= chunkHeaderSize {
		t.Fatalf("ciphertext too short to tamper: %d bytes", len(ct))
	}
	ct[chunkHeaderSize] ^= 0xff

	decR, err := DecryptObject(bytes.NewReader(ct), dek, Options{ChunkSize: 8})
	if err != nil {
		t.Fatalf("DecryptObject: %v", err)
	}
	if _, err := io.ReadAll(decR); err == nil {
		t.Fatal("DecryptObject on tampered ciphertext: want auth error, got nil")
	}
}

func TestLocalFileWrapper_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	cmkPath := filepath.Join(dir, "cmk.key")
	master := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(rand.Reader, master); err != nil {
		t.Fatalf("fill master: %v", err)
	}
	if err := os.WriteFile(cmkPath, master, 0o600); err != nil {
		t.Fatalf("write cmk: %v", err)
	}

	w := LocalFileWrapper{Path: cmkPath}
	cmk := encryption.CustomerMasterKeyRef{
		URI:         "cmk://test/primary",
		Version:     1,
		HolderClass: "customer",
	}

	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	wrapped, err := w.WrapDEK(dek, cmk)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if wrapped.WrapAlgorithm != WrapAlgorithm {
		t.Fatalf("WrapAlgorithm = %q, want %q", wrapped.WrapAlgorithm, WrapAlgorithm)
	}
	if bytes.Equal(wrapped.WrappedKey, dek) {
		t.Fatal("wrapped key equals plaintext DEK")
	}

	got, err := w.UnwrapDEK(wrapped, cmk)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("UnwrapDEK returned mismatched plaintext DEK")
	}
}

func TestLocalFileWrapper_WrongCMKRef(t *testing.T) {
	dir := t.TempDir()
	cmkPath := filepath.Join(dir, "cmk.key")
	master := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(rand.Reader, master); err != nil {
		t.Fatalf("fill master: %v", err)
	}
	if err := os.WriteFile(cmkPath, master, 0o600); err != nil {
		t.Fatalf("write cmk: %v", err)
	}
	w := LocalFileWrapper{Path: cmkPath}

	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	wrapped, err := w.WrapDEK(dek, encryption.CustomerMasterKeyRef{URI: "cmk://a", Version: 1, HolderClass: "customer"})
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if _, err := w.UnwrapDEK(wrapped, encryption.CustomerMasterKeyRef{URI: "cmk://b", Version: 1, HolderClass: "customer"}); err == nil {
		t.Fatal("UnwrapDEK with different CMK URI: want error, got nil")
	}
}

func TestEncryptObject_ConvergentNonce_Determinism(t *testing.T) {
	hash := []byte("blake3:cafebabe")
	dek, err := DeriveConvergentDEK(hash, "tnt_a")
	if err != nil {
		t.Fatalf("DeriveConvergentDEK: %v", err)
	}
	plain := bytes.Repeat([]byte("abcd"), 1024) // multi-chunk
	enc1, err := EncryptObject(bytes.NewReader(plain), dek, Options{ChunkSize: 256, ConvergentNonce: true})
	if err != nil {
		t.Fatalf("EncryptObject: %v", err)
	}
	ct1, err := io.ReadAll(enc1)
	if err != nil {
		t.Fatalf("read ct1: %v", err)
	}
	enc2, err := EncryptObject(bytes.NewReader(plain), dek, Options{ChunkSize: 256, ConvergentNonce: true})
	if err != nil {
		t.Fatalf("EncryptObject: %v", err)
	}
	ct2, err := io.ReadAll(enc2)
	if err != nil {
		t.Fatalf("read ct2: %v", err)
	}
	if !bytes.Equal(ct1, ct2) {
		t.Fatal("convergent nonce mode produced non-deterministic ciphertext")
	}

	// Round-trip the deterministic ciphertext to verify decrypt
	// works against the wire format. The decryptor reads nonces
	// from the frame header so it does not need ConvergentNonce.
	dec, err := DecryptObject(bytes.NewReader(ct1), dek, Options{ChunkSize: 256})
	if err != nil {
		t.Fatalf("DecryptObject: %v", err)
	}
	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("read plaintext: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("convergent round-trip mismatch")
	}
}

func TestEncryptObject_ConvergentNonce_DistinctChunkIndicesProduceDistinctNonces(t *testing.T) {
	dek, err := DeriveConvergentDEK([]byte("h"), "tnt")
	if err != nil {
		t.Fatalf("DeriveConvergentDEK: %v", err)
	}
	// Two chunks of distinct plaintext: the on-wire nonces (the
	// first 24 bytes of each frame) must differ even though both
	// are deterministically derived from the same DEK.
	plain := append(bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16)...)
	enc, err := EncryptObject(bytes.NewReader(plain), dek, Options{ChunkSize: 16, ConvergentNonce: true})
	if err != nil {
		t.Fatalf("EncryptObject: %v", err)
	}
	ct, err := io.ReadAll(enc)
	if err != nil {
		t.Fatalf("read ct: %v", err)
	}
	frameLen := chunkHeaderSize + 16 + 16 // header + plaintext + poly1305 tag
	if len(ct) < 2*frameLen {
		t.Fatalf("expected at least 2 frames, got %d bytes", len(ct))
	}
	nonce1 := ct[:24]
	nonce2 := ct[frameLen : frameLen+24]
	if bytes.Equal(nonce1, nonce2) {
		t.Fatal("chunk nonces collided")
	}
}

func TestEncryptObject_ConvergentNonce_MatchesDeriveHelper(t *testing.T) {
	dek, err := DeriveConvergentDEK([]byte("h"), "tnt")
	if err != nil {
		t.Fatalf("DeriveConvergentDEK: %v", err)
	}
	want, err := deriveConvergentNonce(dek, 0, 24)
	if err != nil {
		t.Fatalf("deriveConvergentNonce: %v", err)
	}
	enc, err := EncryptObject(bytes.NewReader([]byte("x")), dek, Options{ChunkSize: 16, ConvergentNonce: true})
	if err != nil {
		t.Fatalf("EncryptObject: %v", err)
	}
	ct, err := io.ReadAll(enc)
	if err != nil {
		t.Fatalf("read ct: %v", err)
	}
	got := ct[:24]
	if !bytes.Equal(got, want) {
		t.Fatalf("first-chunk nonce mismatch: got %x want %x", got, want)
	}
}

func TestEncryptDecrypt_RoundTrip_WithChunkAAD(t *testing.T) {
	dek := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		t.Fatalf("rand dek: %v", err)
	}
	plain := bytes.Repeat([]byte("xyz"), 2048) // forces multiple chunks at chunkSize=256
	aad := []byte("tnt_a|bkt|0xabc|v1")

	encR, err := EncryptObject(bytes.NewReader(plain), dek, Options{ChunkSize: 256, ChunkAAD: aad})
	if err != nil {
		t.Fatalf("EncryptObject: %v", err)
	}
	ct, err := io.ReadAll(encR)
	if err != nil {
		t.Fatalf("read ct: %v", err)
	}

	// Round-trip with matching AAD succeeds.
	decR, err := DecryptObject(bytes.NewReader(ct), dek, Options{ChunkSize: 256, ChunkAAD: aad})
	if err != nil {
		t.Fatalf("DecryptObject: %v", err)
	}
	got, err := io.ReadAll(decR)
	if err != nil {
		t.Fatalf("read pt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("AAD round-trip mismatch")
	}

	// Decrypt with mismatched AAD must fail (open returns auth error).
	mismatched, err := DecryptObject(bytes.NewReader(ct), dek, Options{ChunkSize: 256, ChunkAAD: []byte("different-aad")})
	if err != nil {
		t.Fatalf("DecryptObject: %v", err)
	}
	if _, err := io.ReadAll(mismatched); err == nil {
		t.Fatal("decrypt with mismatched AAD: want auth error, got nil")
	}

	// Decrypt with nil AAD against ciphertext sealed under non-nil AAD also fails.
	nilAAD, err := DecryptObject(bytes.NewReader(ct), dek, Options{ChunkSize: 256})
	if err != nil {
		t.Fatalf("DecryptObject: %v", err)
	}
	if _, err := io.ReadAll(nilAAD); err == nil {
		t.Fatal("decrypt with nil AAD against AAD-sealed ciphertext: want auth error, got nil")
	}
}

func TestEncryptDecrypt_BackwardsCompat_NilAAD(t *testing.T) {
	// Ciphertext sealed with the zero-value Options (no ChunkAAD)
	// MUST decrypt cleanly with the zero-value Options, so existing
	// objects keep working after the AAD field is added.
	dek := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		t.Fatalf("rand dek: %v", err)
	}
	plain := bytes.Repeat([]byte("legacy"), 1024)

	encR, err := EncryptObject(bytes.NewReader(plain), dek, Options{ChunkSize: 256})
	if err != nil {
		t.Fatalf("EncryptObject: %v", err)
	}
	ct, err := io.ReadAll(encR)
	if err != nil {
		t.Fatalf("read ct: %v", err)
	}
	decR, err := DecryptObject(bytes.NewReader(ct), dek, Options{ChunkSize: 256})
	if err != nil {
		t.Fatalf("DecryptObject: %v", err)
	}
	got, err := io.ReadAll(decR)
	if err != nil {
		t.Fatalf("read pt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("nil-AAD round-trip mismatch")
	}
}

func TestChunkAADBytes_CanonicalFormat(t *testing.T) {
	got := chunkAADBytes([]byte("tnt|bkt|h"), 42)
	wantPrefix := []byte("tnt|bkt|h|")
	if !bytes.HasPrefix(got, wantPrefix) {
		t.Errorf("chunkAADBytes prefix = %q, want %q", got[:len(wantPrefix)], wantPrefix)
	}
	if len(got) != len(wantPrefix)+8 {
		t.Errorf("chunkAADBytes len = %d, want %d", len(got), len(wantPrefix)+8)
	}
	// Suffix is big-endian uint64(42) = 0x00...002a.
	wantSuffix := []byte{0, 0, 0, 0, 0, 0, 0, 42}
	if !bytes.Equal(got[len(wantPrefix):], wantSuffix) {
		t.Errorf("chunkAADBytes suffix = %x, want %x", got[len(wantPrefix):], wantSuffix)
	}
	if nilAAD := chunkAADBytes(nil, 7); nilAAD != nil {
		t.Errorf("chunkAADBytes(nil) = %v, want nil", nilAAD)
	}
	if emptyAAD := chunkAADBytes([]byte{}, 7); emptyAAD != nil {
		t.Errorf("chunkAADBytes([]) = %v, want nil", emptyAAD)
	}
}

// TestEncryptedSize_MatchesActualOutput pins EncryptedSize's
// prediction against the bytes that EncryptObject actually emits.
// The gateway PUT path advertises EncryptedSize to the backend as
// the ciphertext Content-Length before the SDK reader has produced
// any bytes — if the prediction drifts from reality, every
// streaming PUT writes an over- or under-sized object and the
// backend either truncates the body or hangs waiting for bytes
// that never come. This test makes the equality structural.
func TestEncryptedSize_MatchesActualOutput(t *testing.T) {
	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	cases := []struct {
		name      string
		size      int
		chunkSize int
	}{
		{"empty", 0, 0},
		{"smaller-than-chunk", 1024, 0},
		{"exact-chunk-default", DefaultChunkSize, 0},
		{"one-byte-over-chunk", DefaultChunkSize + 1, 0},
		{"two-chunks-default", 2 * DefaultChunkSize, 0},
		{"odd-tail-default", 3*DefaultChunkSize + 17, 0},
		{"small-chunk-multi", 4096, 1024},
		{"small-chunk-odd-tail", 3333, 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plaintext := make([]byte, tc.size)
			if tc.size > 0 {
				if _, err := rand.Read(plaintext); err != nil {
					t.Fatalf("rand: %v", err)
				}
			}
			opts := Options{ChunkSize: tc.chunkSize}
			encR, err := EncryptObject(bytes.NewReader(plaintext), dek, opts)
			if err != nil {
				t.Fatalf("EncryptObject: %v", err)
			}
			ct, err := io.ReadAll(encR)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			want := int64(len(ct))
			got, err := EncryptedSize(int64(len(plaintext)), opts)
			if err != nil {
				t.Fatalf("EncryptedSize(plain=%d, chunk=%d) returned err=%v",
					len(plaintext), opts.chunkSize(), err)
			}
			if got != want {
				t.Fatalf("EncryptedSize(plain=%d, chunk=%d) = %d; actual ciphertext = %d",
					len(plaintext), opts.chunkSize(), got, want)
			}
		})
	}
}

// TestEncryptedSize_NegativeReturnsTypedError pins the contract
// that EncryptedSize rejects negative inputs with
// ErrEncryptedSizeNegativePlaintext. The gateway falls back to
// the buffered (non-streaming) PUT path when the client did not
// supply a Content-Length (r.ContentLength == -1), so in
// practice the streaming PUT path never reaches this branch —
// but the typed return forces every other potential caller
// (multipart pre-checks, dedup pre-checks, future helpers) to
// acknowledge the error rather than silently fall back to a 0
// sentinel that would conflate with the legitimate empty-input
// case.
func TestEncryptedSize_NegativeReturnsTypedError(t *testing.T) {
	for _, n := range []int64{-1, -1024, math.MinInt64} {
		got, err := EncryptedSize(n, Options{})
		if got != 0 {
			t.Errorf("EncryptedSize(%d) returned non-zero %d on error path; want 0", n, got)
		}
		if !errors.Is(err, ErrEncryptedSizeNegativePlaintext) {
			t.Errorf("EncryptedSize(%d) err = %v; want errors.Is(_, ErrEncryptedSizeNegativePlaintext)", n, err)
		}
	}
}

// TestEncryptedSize_EmptyInputReturnsZeroNoError pins the
// legitimate-empty-input contract: an empty object (no frame
// emitted by EncryptObject) must return (0, nil), NOT the
// pre-refactor (0, sentinel) overload. This is the case the
// typed-error refactor was designed to disambiguate from
// overflow.
func TestEncryptedSize_EmptyInputReturnsZeroNoError(t *testing.T) {
	got, err := EncryptedSize(0, Options{})
	if err != nil {
		t.Errorf("EncryptedSize(0) err = %v; want nil (empty objects are legitimate)", err)
	}
	if got != 0 {
		t.Errorf("EncryptedSize(0) = %d; want 0 (no frame emitted for empty plaintext)", got)
	}
}

// TestEncryptedSize_OverflowReturnsTypedError pins the contract
// that EncryptedSize refuses to wrap int64 on a hostile /
// nonsensical plaintextLen and surfaces overflow as
// ErrEncryptedSizeOverflow. A malicious client could send
// "Content-Length: 9223372036854775807" to drive the streaming
// PUT path through EncryptedSize; without overflow guards the
// (plaintextLen + chunk - 1) intermediate would wrap to a
// negative numerator, the numChunks * overhead multiplication
// would silently wrap again, and the gateway would advertise a
// negative ContentLength to the backend (or worse: a small
// positive number that lets the request begin a stream the
// backend cannot complete).
//
// The typed error lets callers distinguish overflow (HTTP 400
// InvalidContentLength) from negative input or invalid chunk
// configuration without needing an out-of-band "plaintextLen > 0"
// disambiguation guard at the callsite. Pre-refactor every
// callsite had to do `if got == 0 && plaintextLen > 0 { fail }` —
// a foot-gun for any future caller that omitted the guard.
func TestEncryptedSize_OverflowReturnsTypedError(t *testing.T) {
	cases := []struct {
		name        string
		plaintext   int64
		opts        Options
		description string
	}{
		{
			name:        "MaxInt64-default-chunk",
			plaintext:   math.MaxInt64,
			opts:        Options{},
			description: "ceil(MaxInt64 / DefaultChunkSize) * overhead exceeds remaining int64 headroom",
		},
		{
			name:        "MaxInt64-tiny-chunk",
			plaintext:   math.MaxInt64,
			opts:        Options{ChunkSize: 1},
			description: "1-byte chunks: per-chunk overhead is (24+4+16)x plaintext, numChunks * overhead wraps in bits.Mul64",
		},
		{
			name:        "MaxInt64-minus-1-tiny-chunk",
			plaintext:   math.MaxInt64 - 1,
			opts:        Options{ChunkSize: 1},
			description: "boundary: even a single byte short of MaxInt64 overflows when chunk=1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncryptedSize(tc.plaintext, tc.opts)
			if got != 0 {
				t.Errorf("EncryptedSize(%d, chunk=%d) = %d; want 0 on overflow\n%s",
					tc.plaintext, tc.opts.chunkSize(), got, tc.description)
			}
			if !errors.Is(err, ErrEncryptedSizeOverflow) {
				t.Errorf("EncryptedSize(%d, chunk=%d) err = %v; want errors.Is(_, ErrEncryptedSizeOverflow)\n%s",
					tc.plaintext, tc.opts.chunkSize(), err, tc.description)
			}
		})
	}
}

// TestEncryptedSize_LargeButSafeReturnsExact confirms the overflow
// guards do not over-eagerly reject inputs that fit cleanly. A 1 GiB
// upload is representative of the upper end of the streaming PUT
// path's real workload and must produce a deterministic, non-zero
// prediction.
func TestEncryptedSize_LargeButSafeReturnsExact(t *testing.T) {
	const oneGiB = int64(1 << 30)
	got, err := EncryptedSize(oneGiB, Options{})
	if err != nil {
		t.Fatalf("EncryptedSize(1 GiB, default) returned err=%v; want nil (1 GiB is well within int64 with default chunk)", err)
	}
	if got <= 0 {
		t.Fatalf("EncryptedSize(1 GiB, default) = %d; want positive", got)
	}
	// numChunks = ceil(1 GiB / 16 MiB) = 64; overhead = 64 * 44 = 2816.
	want := oneGiB + 64*int64(chunkHeaderSize+chunkTrailerSize)
	if got != want {
		t.Errorf("EncryptedSize(1 GiB, default) = %d; want %d (1 GiB + 64 frames * (header+tag))", got, want)
	}
}

// TestChunkTrailerSize_TracksAEADOverhead is a compile-time guard
// against a future contributor swapping the AEAD in EncryptObject
// without updating the constant chunkTrailerSize. Bound to
// chacha20poly1305.Overhead in the SDK so the prediction stays in
// lockstep with the actual on-disk tag length.
func TestChunkTrailerSize_TracksAEADOverhead(t *testing.T) {
	if chunkTrailerSize != chacha20poly1305.Overhead {
		t.Fatalf("chunkTrailerSize = %d; want chacha20poly1305.Overhead (= %d)",
			chunkTrailerSize, chacha20poly1305.Overhead)
	}
}

// TestChaCha20Poly1305NewXCopiesKey is a structural / dependency-
// upgrade guard pinning the invariant the gateway's DEK scrubbing
// (api/s3compat/encryption_pipeline.go) relies on: chacha20poly1305
// .NewX MUST copy the caller's key bytes into the AEAD's internal
// state so the caller is free to clear the key slice once NewX has
// returned. If a future golang.org/x/crypto release switches NewX
// to retain a reference to the caller's slice (e.g. for a zero-
// allocation Open/Seal API), every clear(dek) callsite in the
// gateway would silently corrupt the AEAD and produce garbled
// ciphertext / unauthenticatable tags. This test catches that
// regression before it ships.
//
// The test is structural rather than mocked: it builds a real
// AEAD, mutates the caller's key slice AFTER NewX returns, then
// verifies that Seal on the now-mutated key still produces
// ciphertext decryptable with the original key bytes. If the
// invariant holds, the AEAD ignores the post-construct mutation.
// If a future copy-removal lands, Seal would key off the mutated
// bytes and decrypt with the original would fail authentication.
func TestChaCha20Poly1305NewXCopiesKey(t *testing.T) {
	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}

	// Snapshot the original key bytes before any mutation.
	original := make([]byte, len(dek))
	copy(original, dek)

	aead, err := chacha20poly1305.NewX(dek)
	if err != nil {
		t.Fatalf("chacha20poly1305.NewX: %v", err)
	}

	// Mutate every byte of the caller's slice. If NewX retained
	// a reference to it (no copy), Seal below would key off the
	// 0xFF-filled buffer, not the snapshot.
	for i := range dek {
		dek[i] = 0xFF
	}

	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("rand.Read nonce: %v", err)
	}
	plaintext := []byte("structural test: NewX must copy its key argument")
	ciphertext := aead.Seal(nil, nonce, plaintext, nil)

	// Verify Seal used the ORIGINAL key (snapshot) by opening
	// with a fresh AEAD constructed from that snapshot. If the
	// invariant ever breaks, Open below returns an auth error.
	verifier, err := chacha20poly1305.NewX(original)
	if err != nil {
		t.Fatalf("chacha20poly1305.NewX(snapshot): %v", err)
	}
	got, err := verifier.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("AEAD.Open with snapshot key failed: chacha20poly1305.NewX appears to NOT copy its key argument anymore; every clear(dek) callsite in the gateway now corrupts the AEAD. Update DEK scrubbing strategy. err=%v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("decrypted plaintext mismatch: got %q want %q", got, plaintext)
	}

	// Sanity check the inverse: an AEAD constructed from the
	// MUTATED slice must NOT decrypt the ciphertext (otherwise
	// the test is vacuous — e.g. NewX silently maps to a
	// constant key). This protects future maintainers from
	// regressions where the test passes for the wrong reason.
	mutated, err := chacha20poly1305.NewX(dek)
	if err != nil {
		t.Fatalf("chacha20poly1305.NewX(mutated): %v", err)
	}
	if _, err := mutated.Open(nil, nonce, ciphertext, nil); err == nil {
		t.Fatalf("AEAD constructed from mutated key unexpectedly decrypted the ciphertext; the test is no longer probing the copy-on-construct invariant")
	}
}
