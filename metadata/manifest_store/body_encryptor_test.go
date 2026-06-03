package manifest_store

import (
	"bytes"
	"crypto/rand"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	return k
}

func TestAEADBodyEncryptor_RoundTrip_WithContext(t *testing.T) {
	enc, err := NewAEADBodyEncryptor(randKey(t))
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}
	ctx := BodyContext{TenantID: "tnt-a", Bucket: "logs", ObjectKeyHash: "0xabc"}
	pt := []byte(`{"tenant_id":"tnt-a"}`)
	ct, err := enc.Encrypt(pt, ctx)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := enc.Decrypt(ct, ctx)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("plaintext mismatch: got %q want %q", got, pt)
	}
}

func TestAEADBodyEncryptor_AADMismatch_Fails(t *testing.T) {
	enc, err := NewAEADBodyEncryptor(randKey(t))
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}
	pt := []byte("body")
	ctxA := BodyContext{TenantID: "tnt-a", Bucket: "logs", ObjectKeyHash: "0xabc"}
	ctxB := BodyContext{TenantID: "tnt-b", Bucket: "logs", ObjectKeyHash: "0xabc"} // different tenant
	ct, err := enc.Encrypt(pt, ctxA)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := enc.Decrypt(ct, ctxB); err == nil {
		t.Fatal("decrypt with wrong tenant: want error, got nil")
	}
}

func TestAEADBodyEncryptor_LegacyCiphertext_DecryptsWithoutAAD(t *testing.T) {
	enc, err := NewAEADBodyEncryptor(randKey(t))
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}
	// Simulate a legacy (pre-AAD) row by sealing with the zero
	// BodyContext, which the encryptor maps to nil AAD.
	pt := []byte("legacy body")
	legacyCT, err := enc.Encrypt(pt, BodyContext{})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// New read path passes a populated BodyContext; the encryptor
	// must transparently fall back to nil AAD so legacy rows
	// keep decrypting.
	got, err := enc.Decrypt(legacyCT, BodyContext{TenantID: "tnt-a", Bucket: "b", ObjectKeyHash: "h"})
	if err != nil {
		t.Fatalf("decrypt legacy: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("plaintext mismatch: got %q want %q", got, pt)
	}
}

func TestAEADBodyEncryptor_TooShort(t *testing.T) {
	enc, err := NewAEADBodyEncryptor(randKey(t))
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}
	if _, err := enc.Decrypt([]byte("x"), BodyContext{}); err == nil {
		t.Fatal("short ciphertext: want error, got nil")
	}
}

func TestBodyContextAAD_CanonicalFormat(t *testing.T) {
	got := bodyContextAAD(BodyContext{TenantID: "t", Bucket: "b", ObjectKeyHash: "h"})
	want := []byte("t|b|h")
	if !bytes.Equal(got, want) {
		t.Fatalf("aad = %q, want %q", got, want)
	}
	if v := bodyContextAAD(BodyContext{}); v != nil {
		t.Fatalf("aad(zero) = %v, want nil", v)
	}
}
