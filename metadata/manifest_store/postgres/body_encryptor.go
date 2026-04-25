// Manifest body encryption for the Postgres store.
//
// The Phase 2 control plane records manifests as opaque JSON
// documents in Postgres. Strict ZK operators also want those
// documents encrypted at rest so a Postgres admin cannot read
// object keys, piece locations, or tenant sizes directly out of
// the database. BodyEncryptor is the hook that makes that
// transparent: when the store has an encryptor, Put encrypts the
// marshalled JSON before INSERT and Get / List decrypt after
// SELECT. When it is nil the store behaves exactly as before.
//
// AEADBodyEncryptor is the shipped concrete implementation, using
// XChaCha20-Poly1305 with a 32-byte gateway-held key (separate
// from any per-object CMK).

package postgres

import (
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// BodyEncryptor seals and opens the manifest JSON document stored
// in the Postgres body column. A nil BodyEncryptor leaves the JSON
// in the clear (the legacy Phase 2 layout).
//
// Implementations must be safe for concurrent use by the store.
type BodyEncryptor interface {
	// Encrypt returns the on-disk form of plaintext. The returned
	// bytes are opaque to the store — they are stored verbatim in
	// the body column and passed back to Decrypt at read time.
	Encrypt(plaintext []byte) ([]byte, error)
	// Decrypt is the inverse of Encrypt.
	Decrypt(ciphertext []byte) ([]byte, error)
}

// AEADBodyEncryptor is the XChaCha20-Poly1305 implementation of
// BodyEncryptor. It frames every blob as [24-byte nonce][ciphertext
// with 16-byte Poly1305 tag] so Decrypt can parse without a
// separate length prefix.
//
// The key is held by the gateway process (typically loaded from a
// local file or a KMS) and MUST NOT be shared with any tenant or
// operator who only has Postgres access; that would defeat the
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
		return nil, fmt.Errorf("postgres: body encryptor key must be %d bytes, got %d", chacha20poly1305.KeySize, len(key))
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("postgres: new xchacha20-poly1305: %w", err)
	}
	return &AEADBodyEncryptor{aead: aead}, nil
}

// Encrypt seals plaintext with a fresh nonce and returns
// [nonce || ciphertext].
func (e *AEADBodyEncryptor) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("postgres: body nonce: %w", err)
	}
	sealed := e.aead.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(nonce)+len(sealed))
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// Decrypt reverses Encrypt. Any error here indicates the body was
// tampered with or encrypted with a different key; the store
// surfaces this as a read error rather than silently returning
// garbage JSON.
func (e *AEADBodyEncryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < e.aead.NonceSize()+e.aead.Overhead() {
		return nil, errors.New("postgres: ciphertext shorter than nonce + tag")
	}
	nonce := ciphertext[:e.aead.NonceSize()]
	body := ciphertext[e.aead.NonceSize():]
	plaintext, err := e.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("postgres: open body: %w", err)
	}
	return plaintext, nil
}
