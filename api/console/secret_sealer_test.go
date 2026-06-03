package console

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
)

func newTestSealer(t *testing.T) *AEADSecretSealer {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	sealer, err := NewAEADSecretSealer(key)
	if err != nil {
		t.Fatalf("NewAEADSecretSealer: %v", err)
	}
	return sealer
}

func TestAEADSecretSealerRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)
	const tenant, secret = "tenant-1", "JBSWY3DPEHPK3PXP"

	sealed, err := s.Seal(secret, tenant)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(sealed, sealedSecretPrefix) {
		t.Fatalf("sealed value %q missing prefix %q", sealed, sealedSecretPrefix)
	}
	if strings.Contains(sealed, secret) {
		t.Fatalf("sealed value %q leaks the plaintext secret", sealed)
	}
	// Each Seal uses a fresh nonce, so two seals of the same input differ.
	sealed2, err := s.Seal(secret, tenant)
	if err != nil {
		t.Fatalf("Seal (2): %v", err)
	}
	if sealed == sealed2 {
		t.Fatal("two seals of the same secret produced identical ciphertext (nonce reuse?)")
	}
	got, err := s.Open(sealed, tenant)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != secret {
		t.Fatalf("Open round-trip = %q, want %q", got, secret)
	}
}

func TestAEADSecretSealerRejectsWrongTenant(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)
	sealed, err := s.Seal("SECRET", "tenant-a")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// The tenant id is bound as AEAD AAD: opening under another tenant
	// must fail, so a row lifted onto a different tenant won't decrypt.
	if _, err := s.Open(sealed, "tenant-b"); err == nil {
		t.Fatal("Open under a different tenant should fail (AAD mismatch)")
	}
}

func TestAEADSecretSealerRejectsTamper(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)
	sealed, err := s.Seal("SECRET", "t")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Decode the blob, flip a byte inside the ciphertext (past the
	// nonce), and re-seal: AEAD authentication must then reject it.
	blob, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(sealed, sealedSecretPrefix))
	if err != nil {
		t.Fatalf("decode sealed blob: %v", err)
	}
	blob[len(blob)-1] ^= 0xFF
	tampered := sealedSecretPrefix + base64.RawStdEncoding.EncodeToString(blob)
	if _, err := s.Open(tampered, "t"); err == nil {
		t.Fatal("Open of a tampered blob should fail")
	}
}

func TestAEADSecretSealerPassesThroughPlaintext(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)
	// A value written before sealing was enabled has no prefix and must
	// be returned unchanged so enabling sealing needs no up-front re-key.
	const legacy = "JBSWY3DPEHPK3PXP"
	got, err := s.Open(legacy, "t")
	if err != nil {
		t.Fatalf("Open(legacy plaintext): %v", err)
	}
	if got != legacy {
		t.Fatalf("Open(legacy) = %q, want pass-through %q", got, legacy)
	}
}

func TestNewAEADSecretSealerRejectsBadKey(t *testing.T) {
	t.Parallel()
	if _, err := NewAEADSecretSealer(make([]byte, 16)); err == nil {
		t.Fatal("NewAEADSecretSealer should reject a 16-byte key")
	}
}

// TestSQLiteMFAStoreSealsSecretAtRest verifies the end-to-end property:
// with a sealer configured, the raw secret column holds ciphertext (not
// the base32 seed), yet GetMFA transparently returns the plaintext.
func TestSQLiteMFAStoreSealsSecretAtRest(t *testing.T) {
	t.Parallel()
	db, err := embeddeddb.Open(filepath.Join(t.TempDir(), "mfa.db"))
	if err != nil {
		t.Fatalf("open embedded db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const tenant, secret = "tenant-seal", "JBSWY3DPEHPK3PXP"
	store, err := NewSQLiteMFAStore(db, WithSecretSealer(newTestSealer(t)))
	if err != nil {
		t.Fatalf("new sqlite mfa store: %v", err)
	}
	if err := store.BeginEnrollment(tenant, secret); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}

	var raw string
	if err := db.QueryRow(`SELECT secret FROM mfa_credentials WHERE tenant_id = ?`, tenant).Scan(&raw); err != nil {
		t.Fatalf("read raw secret column: %v", err)
	}
	if raw == secret {
		t.Fatal("secret column holds the plaintext seed; sealing did not happen")
	}
	if !strings.HasPrefix(raw, sealedSecretPrefix) {
		t.Fatalf("raw column %q is not a sealed blob", raw)
	}

	rec, ok, err := store.GetMFA(tenant)
	if err != nil || !ok {
		t.Fatalf("GetMFA = (_, %v, %v)", ok, err)
	}
	if rec.Secret != secret {
		t.Fatalf("GetMFA secret = %q, want plaintext %q", rec.Secret, secret)
	}

	// A store opened over the same db WITHOUT the sealer key sees only
	// ciphertext — proving a DB-only adversary cannot recover the seed.
	plainStore, err := NewSQLiteMFAStore(db)
	if err != nil {
		t.Fatalf("new sqlite mfa store (no sealer): %v", err)
	}
	rec2, ok, err := plainStore.GetMFA(tenant)
	if err != nil || !ok {
		t.Fatalf("GetMFA (no sealer) = (_, %v, %v)", ok, err)
	}
	if rec2.Secret == secret {
		t.Fatal("a store without the sealer key recovered the plaintext secret")
	}
	if !strings.HasPrefix(rec2.Secret, sealedSecretPrefix) {
		t.Fatalf("no-sealer GetMFA returned %q, expected opaque ciphertext", rec2.Secret)
	}
}
