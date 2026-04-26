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

func TestEncryptObject_ConvergentNonceUnimplemented(t *testing.T) {
	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	if _, err := EncryptObject(bytes.NewReader([]byte("x")), dek, Options{ConvergentNonce: true}); err != ErrConvergentNonceNotImplemented {
		t.Fatalf("EncryptObject with ConvergentNonce=true: want ErrConvergentNonceNotImplemented, got %v", err)
	}
	if _, err := DecryptObject(bytes.NewReader([]byte("x")), dek, Options{ConvergentNonce: true}); err != ErrConvergentNonceNotImplemented {
		t.Fatalf("DecryptObject with ConvergentNonce=true: want ErrConvergentNonceNotImplemented, got %v", err)
	}
}
