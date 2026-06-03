// White-box tests for AAD v1 per-chunk binding.
//
// These exercise the gateway crypto helpers directly (same package)
// so the AAD shape can be asserted at the AEAD layer without going
// through the full S3 HTTP stack. The end-to-end wiring (which code
// path records AADVersion, copy re-encryption, multipart version
// persistence) is covered by tests/s3_compat/aad_version_test.go.
//
// The matrix proves the four invariants the live encryption pipeline
// depends on:
//
//   1. v1 ciphertext round-trips when decrypted under the identical
//      object identity.
//   2. v1 ciphertext is rejected (AEAD tag failure) when ANY identity
//      component differs — this is what makes a verbatim server-side
//      copy of a v1 object undecryptable and forces copyReencrypt.
//   3. The AADVersion flag actually selects the AAD shape: a v1
//      ciphertext fails to open as legacy (nil AAD) and a legacy
//      ciphertext fails to open as v1.
//   4. Legacy (nil-AAD) ciphertext round-trips regardless of the
//      identity passed on the decrypt side.
package s3compat

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
	"github.com/kennguy3n/zk-object-fabric/metadata"
)

// newAADTestHandler returns a Handler wired with a LocalFileWrapper
// over a fresh random CMK — the minimum needed to drive
// encryptForStorage / decryptFromStorage.
func newAADTestHandler(t *testing.T) *Handler {
	t.Helper()
	cmkPath := filepath.Join(t.TempDir(), "cmk.key")
	cmk := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(cmk); err != nil {
		t.Fatalf("rand cmk: %v", err)
	}
	if err := os.WriteFile(cmkPath, cmk, 0o600); err != nil {
		t.Fatalf("write cmk: %v", err)
	}
	return New(Config{
		Encryption: &GatewayEncryption{
			Wrapper: client_sdk.LocalFileWrapper{Path: cmkPath},
			CMK: encryption.CustomerMasterKeyRef{
				URI:         "cmk://test/primary",
				Version:     1,
				HolderClass: "gateway_hsm",
			},
		},
	})
}

func sampleIdentity() aadIdentity {
	return aadIdentity{
		TenantID:      "tenant-a",
		Bucket:        "bucket-a",
		ObjectKeyHash: "deadbeef",
		VersionID:     "v-0001",
	}
}

// v1Config builds the EncryptionConfig the encrypt path records for a
// v1 write (AADVersion = "v1").
func v1Config(w client_sdk.WrappedDEK) metadata.EncryptionConfig {
	return metadata.EncryptionConfig{
		Mode:          "managed",
		Algorithm:     client_sdk.ContentAlgorithm,
		KeyID:         w.KeyID,
		WrappedDEK:    w.WrappedKey,
		WrapAlgorithm: w.WrapAlgorithm,
		AADVersion:    AADVersionV1,
	}
}

func TestAADv1_RoundTripSameIdentity(t *testing.T) {
	h := newAADTestHandler(t)
	id := sampleIdentity()
	plaintext := []byte("aad v1 round-trip must succeed under the same identity")

	ciphertext, wrapped, err := h.encryptForStorage(append([]byte(nil), plaintext...), id)
	if err != nil {
		t.Fatalf("encryptForStorage: %v", err)
	}
	if bytes.Contains(ciphertext, plaintext) {
		t.Fatal("ciphertext leaked plaintext")
	}

	got, err := h.decryptFromStorage(ciphertext, v1Config(wrapped), id)
	if err != nil {
		t.Fatalf("decryptFromStorage (same identity): %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip mismatch: want %q got %q", plaintext, got)
	}
}

func TestAADv1_MismatchedIdentityRejected(t *testing.T) {
	h := newAADTestHandler(t)
	id := sampleIdentity()
	plaintext := []byte("aad v1 must fail closed when the identity differs")

	ciphertext, wrapped, err := h.encryptForStorage(append([]byte(nil), plaintext...), id)
	if err != nil {
		t.Fatalf("encryptForStorage: %v", err)
	}
	enc := v1Config(wrapped)

	// Each component of the identity is part of the bound AAD;
	// changing any one must surface as an AEAD tag failure on
	// decrypt. This is exactly the condition that makes a verbatim
	// server-side copy (which changes at least the version) of a v1
	// object undecryptable, so copy must re-encrypt.
	mutations := map[string]aadIdentity{
		"tenant":     {TenantID: "tenant-OTHER", Bucket: id.Bucket, ObjectKeyHash: id.ObjectKeyHash, VersionID: id.VersionID},
		"bucket":     {TenantID: id.TenantID, Bucket: "bucket-OTHER", ObjectKeyHash: id.ObjectKeyHash, VersionID: id.VersionID},
		"object_key": {TenantID: id.TenantID, Bucket: id.Bucket, ObjectKeyHash: "feedface", VersionID: id.VersionID},
		"version":    {TenantID: id.TenantID, Bucket: id.Bucket, ObjectKeyHash: id.ObjectKeyHash, VersionID: "v-OTHER"},
	}
	for name, wrongID := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, derr := h.decryptFromStorage(ciphertext, enc, wrongID); derr == nil {
				t.Fatalf("decrypt with mismatched %s identity: want AEAD failure, got nil", name)
			}
		})
	}
}

func TestAADv1_FlagSelectsAADShape(t *testing.T) {
	h := newAADTestHandler(t)
	id := sampleIdentity()
	plaintext := []byte("the AADVersion flag must drive the AAD shape on decrypt")

	// v1 ciphertext opened as legacy (nil AAD) must fail.
	v1Cipher, v1Wrapped, err := h.encryptForStorage(append([]byte(nil), plaintext...), id)
	if err != nil {
		t.Fatalf("encryptForStorage: %v", err)
	}
	legacyView := v1Config(v1Wrapped)
	legacyView.AADVersion = "" // pretend the manifest said legacy
	if _, derr := h.decryptFromStorage(v1Cipher, legacyView, id); derr == nil {
		t.Fatal("v1 ciphertext decrypted as legacy (nil AAD): want failure, got nil")
	}

	// Legacy ciphertext (sealed with nil AAD) opened as v1 must fail.
	legacyCipher, legacyEnc := sealLegacy(t, h, plaintext)
	v1View := legacyEnc
	v1View.AADVersion = AADVersionV1 // pretend the manifest said v1
	if _, derr := h.decryptFromStorage(legacyCipher, v1View, id); derr == nil {
		t.Fatal("legacy ciphertext decrypted as v1 (bound AAD): want failure, got nil")
	}
}

func TestAADLegacy_NilAADRoundTrip(t *testing.T) {
	h := newAADTestHandler(t)
	plaintext := []byte("legacy nil-AAD ciphertext must round-trip under any identity")

	ciphertext, enc := sealLegacy(t, h, plaintext)

	// Legacy decrypt ignores the identity (gatewayDecryptOptions
	// returns nil AAD when AADVersion is ""), so even a populated,
	// "wrong" identity must still open the object.
	got, err := h.decryptFromStorage(ciphertext, enc, sampleIdentity())
	if err != nil {
		t.Fatalf("legacy decryptFromStorage: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("legacy round-trip mismatch: want %q got %q", plaintext, got)
	}
}

// sealLegacy reproduces a pre-AAD object: ciphertext sealed with the
// SDK's nil-AAD Options and a wrapped DEK, recorded with an empty
// AADVersion exactly as objects written before this change carry.
func sealLegacy(t *testing.T, h *Handler, plaintext []byte) ([]byte, metadata.EncryptionConfig) {
	t.Helper()
	dek, err := client_sdk.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	r, err := client_sdk.EncryptObject(bytes.NewReader(plaintext), dek, client_sdk.Options{})
	if err != nil {
		t.Fatalf("EncryptObject (legacy nil AAD): %v", err)
	}
	ciphertext, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read legacy ciphertext: %v", err)
	}
	wrapped, err := h.cfg.Encryption.Wrapper.WrapDEK(dek, h.cfg.Encryption.CMK)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	return ciphertext, metadata.EncryptionConfig{
		Mode:          "managed",
		Algorithm:     client_sdk.ContentAlgorithm,
		KeyID:         wrapped.KeyID,
		WrappedDEK:    wrapped.WrappedKey,
		WrapAlgorithm: wrapped.WrapAlgorithm,
		AADVersion:    "",
	}
}
