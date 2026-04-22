package client_sdk

import (
	"crypto/rand"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// randReader is overridable in tests but always points at a
// cryptographically secure source in production.
var randReader io.Reader = rand.Reader

// GenerateDEK returns a fresh 256-bit DataEncryptionKey sampled from
// crypto/rand. The key is suitable for XChaCha20-Poly1305 (which
// requires a 32-byte key). Callers MUST wrap the DEK with WrapDEK
// before persisting it; the plaintext key returned here is held
// transiently in memory for the duration of the encrypt/decrypt
// operation only.
func GenerateDEK() (DataEncryptionKey, error) {
	k := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(randReader, k); err != nil {
		return nil, fmt.Errorf("client_sdk: generate dek: %w", err)
	}
	return DataEncryptionKey(k), nil
}
