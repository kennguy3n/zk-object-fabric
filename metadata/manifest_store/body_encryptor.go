// Manifest body encryption shared by the Postgres and SQLite stores.
//
// The control plane records manifests as opaque JSON documents.
// Strict ZK operators also want those documents encrypted at rest so
// a database admin cannot read object keys, piece locations, or
// tenant sizes directly out of the database file. BodyEncryptor is
// the hook that makes that transparent: when a store has an
// encryptor, Put encrypts the marshalled JSON before INSERT and Get /
// List decrypt after SELECT. When it is nil the store behaves exactly
// as before (plaintext JSON).
//
// AEADBodyEncryptor is the shipped concrete implementation, using
// XChaCha20-Poly1305 with a 32-byte gateway-held key (separate from
// any per-object CMK). It lives here, in the parent manifest_store
// package, so the Postgres and SQLite backends share one
// implementation rather than each maintaining its own copy of the
// crypto.
package manifest_store

import (
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// BodyContext binds a manifest body to a specific manifest key. It
// is passed to BodyEncryptor on every Encrypt / Decrypt call so the
// implementation can mix the key into the AEAD AAD; that prevents
// a body sealed for one (tenant, bucket, object_key_hash) from
// decrypting cleanly if a database admin swaps it onto a different
// row. All three fields are required — the store enforces that
// before invoking the encryptor.
type BodyContext struct {
	TenantID      string
	Bucket        string
	ObjectKeyHash string
}

// BodyEncryptor seals and opens the manifest JSON document stored
// in the body column. A nil BodyEncryptor leaves the JSON in the
// clear (the legacy Phase 2 layout).
//
// Implementations must be safe for concurrent use by the store.
// The BodyContext argument is mandatory on every call so the
// implementation can bind the ciphertext to the manifest key it
// belongs to. Implementations that don't need the binding may
// ignore the argument, but the canonical AEAD encryptor uses it
// as AEAD AAD and rejects a mismatched row on Decrypt.
type BodyEncryptor interface {
	// Encrypt returns the on-disk form of plaintext. The returned
	// bytes are opaque to the store — they are stored verbatim in
	// the body column and passed back to Decrypt at read time.
	Encrypt(plaintext []byte, ctx BodyContext) ([]byte, error)
	// Decrypt is the inverse of Encrypt. Implementations that bind
	// ciphertext to BodyContext should also accept legacy
	// ciphertext sealed with an empty BodyContext (i.e. nil AAD),
	// so a deployment can roll forward without re-encrypting every
	// existing row up-front.
	Decrypt(ciphertext []byte, ctx BodyContext) ([]byte, error)
}

// bodyContextAAD returns the canonical pipe-separated AAD string
// for a BodyContext. The format is reproducible byte-for-byte
// across implementations so a future SDK / re-key tool can verify
// AAD bindings without re-implementing this package.
func bodyContextAAD(c BodyContext) []byte {
	if c.TenantID == "" && c.Bucket == "" && c.ObjectKeyHash == "" {
		return nil
	}
	aad := make([]byte, 0, len(c.TenantID)+len(c.Bucket)+len(c.ObjectKeyHash)+2)
	aad = append(aad, c.TenantID...)
	aad = append(aad, '|')
	aad = append(aad, c.Bucket...)
	aad = append(aad, '|')
	aad = append(aad, c.ObjectKeyHash...)
	return aad
}

// AEADBodyEncryptor is the XChaCha20-Poly1305 implementation of
// BodyEncryptor. It frames every blob as [24-byte nonce][ciphertext
// with 16-byte Poly1305 tag] so Decrypt can parse without a
// separate length prefix.
//
// The key is held by the gateway process (typically loaded from a
// local file or a KMS) and MUST NOT be shared with any tenant or
// operator who only has database access; that would defeat the
// at-rest protection the encryptor provides.
type AEADBodyEncryptor struct {
	aead cipherAEAD
}

// cipherAEAD is a tiny local alias so this package doesn't pull in
// `crypto/cipher` just to satisfy the interface check.
type cipherAEAD interface {
	NonceSize() int
	Overhead() int
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}

// NewAEADBodyEncryptor returns an encryptor keyed off the given 32
// bytes. The caller is responsible for key material: loading it
// from a file, pulling it from KMS, etc.
func NewAEADBodyEncryptor(key []byte) (*AEADBodyEncryptor, error) {
	if len(key) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("manifest_store: body encryptor key must be %d bytes, got %d", chacha20poly1305.KeySize, len(key))
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("manifest_store: new xchacha20-poly1305: %w", err)
	}
	return &AEADBodyEncryptor{aead: aead}, nil
}

// Encrypt seals plaintext with a fresh nonce, binds the ciphertext
// to BodyContext via AEAD AAD, and returns [nonce || ciphertext].
// The AAD is the canonical pipe-separated form (tenant_id|bucket|
// object_key_hash). When BodyContext is the zero value the
// encryptor falls back to AAD = nil for symmetry with Decrypt's
// legacy path.
func (e *AEADBodyEncryptor) Encrypt(plaintext []byte, ctx BodyContext) ([]byte, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("manifest_store: body nonce: %w", err)
	}
	sealed := e.aead.Seal(nil, nonce, plaintext, bodyContextAAD(ctx))
	out := make([]byte, 0, len(nonce)+len(sealed))
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// Decrypt reverses Encrypt. It first tries to open the ciphertext
// with the canonical BodyContext AAD; if Open returns a MAC
// failure it retries with AAD = nil so legacy rows written by the
// pre-AAD layout still decrypt cleanly. The store re-encrypts
// legacy rows on the next Put so a deployment converges to fully
// AAD-bound ciphertext without an up-front backfill.
func (e *AEADBodyEncryptor) Decrypt(ciphertext []byte, ctx BodyContext) ([]byte, error) {
	if len(ciphertext) < e.aead.NonceSize()+e.aead.Overhead() {
		return nil, errors.New("manifest_store: ciphertext shorter than nonce + tag")
	}
	nonce := ciphertext[:e.aead.NonceSize()]
	body := ciphertext[e.aead.NonceSize():]
	aad := bodyContextAAD(ctx)
	if plaintext, err := e.aead.Open(nil, nonce, body, aad); err == nil {
		return plaintext, nil
	}
	// Backwards-compat path: pre-AAD rows were sealed with AAD=nil.
	// Retry with nil AAD so they still decrypt; the caller (Store)
	// re-encrypts on the next Put which migrates the row.
	if aad == nil {
		return nil, errors.New("manifest_store: open body: aead.Open failed")
	}
	plaintext, err := e.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("manifest_store: open body: %w", err)
	}
	return plaintext, nil
}
