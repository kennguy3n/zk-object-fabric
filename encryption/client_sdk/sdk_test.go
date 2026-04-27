package client_sdk

import (
	"bytes"
	"crypto/rand"
	"io"
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
