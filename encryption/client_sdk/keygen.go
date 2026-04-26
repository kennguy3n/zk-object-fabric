package client_sdk

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
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

// ConvergentDEKInfo is the HKDF info string used by
// DeriveConvergentDEK. Versioned so a future rotation can derive a
// disjoint key space without breaking existing manifests.
const ConvergentDEKInfo = "zkof-convergent-dek-v1"

// DeriveConvergentDEK derives a deterministic 256-bit DEK from a
// content hash and tenant ID using HKDF-SHA256 (RFC 5869). Identical
// plaintext within the same tenant produces the same DEK, enabling
// intra-tenant deduplication. Cross-tenant collisions are excluded
// by construction: the salt binds the derivation to tenantID, so
// distinct tenants always derive distinct DEKs even for identical
// content. Trade-off: stored ciphertext loses forward secrecy for
// the deduped object — a future leak of the DEK reveals every
// historical and future copy under the same (tenant, content_hash)
// key. See docs/PROPOSAL.md §3.14.
//
// The function is purely deterministic: it never reads from
// randReader. contentHash must be non-empty so two distinct objects
// cannot accidentally collide on the empty-hash key. tenantID is
// required for the same reason — and so cross-tenant lookups are
// impossible by construction.
func DeriveConvergentDEK(contentHash []byte, tenantID string) (DataEncryptionKey, error) {
	if len(contentHash) == 0 {
		return nil, errors.New("client_sdk: convergent DEK: contentHash is required")
	}
	if tenantID == "" {
		return nil, errors.New("client_sdk: convergent DEK: tenantID is required")
	}
	salt := []byte(tenantID)
	info := []byte(ConvergentDEKInfo)
	r := hkdf.New(sha256.New, contentHash, salt, info)
	dek := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(r, dek); err != nil {
		return nil, fmt.Errorf("client_sdk: convergent DEK: hkdf read: %w", err)
	}
	return DataEncryptionKey(dek), nil
}
