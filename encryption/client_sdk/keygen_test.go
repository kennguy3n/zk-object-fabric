package client_sdk

import (
	"bytes"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

func TestDeriveConvergentDEK_Deterministic(t *testing.T) {
	hash := []byte("blake3:cafebabe")
	tenant := "tnt_abc"
	a, err := DeriveConvergentDEK(hash, tenant)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	b, err := DeriveConvergentDEK(hash, tenant)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("Derive is not deterministic")
	}
}

func TestDeriveConvergentDEK_KeyLength(t *testing.T) {
	dek, err := DeriveConvergentDEK([]byte{0x01, 0x02}, "tnt")
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if got, want := len(dek), chacha20poly1305.KeySize; got != want {
		t.Fatalf("Derive length = %d, want %d", got, want)
	}
}

func TestDeriveConvergentDEK_DistinctTenants(t *testing.T) {
	hash := []byte("blake3:deadbeef")
	a, err := DeriveConvergentDEK(hash, "tnt_a")
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	b, err := DeriveConvergentDEK(hash, "tnt_b")
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("distinct tenants produced identical DEKs (cross-tenant key collision)")
	}
}

func TestDeriveConvergentDEK_DistinctHashes(t *testing.T) {
	tenant := "tnt"
	a, err := DeriveConvergentDEK([]byte("hash-a"), tenant)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	b, err := DeriveConvergentDEK([]byte("hash-b"), tenant)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("distinct content hashes produced identical DEKs")
	}
}

func TestDeriveConvergentDEK_RejectsEmptyInputs(t *testing.T) {
	if _, err := DeriveConvergentDEK(nil, "tnt"); err == nil {
		t.Fatal("empty contentHash: want error, got nil")
	}
	if _, err := DeriveConvergentDEK([]byte("h"), ""); err == nil {
		t.Fatal("empty tenantID: want error, got nil")
	}
}
