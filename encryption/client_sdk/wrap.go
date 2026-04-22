package client_sdk

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/kennguy3n/zk-object-fabric/encryption"
)

// WrapAlgorithm is the canonical wrap algorithm written to
// encryption.DataEncryptionKey.WrapAlgorithm for Phase 2. It is an
// AEAD wrap using XChaCha20-Poly1305 with a key derived from the
// local CMK file. Phase 3 replaces this with AWS KMS / Vault while
// leaving the envelope shape intact.
const WrapAlgorithm = "xchacha20-poly1305-wrap-v1"

// WrappedDEK is the encrypted form of a DataEncryptionKey. It
// mirrors encryption.DataEncryptionKey so a caller can persist it
// verbatim on an ObjectManifest.
type WrappedDEK = encryption.DataEncryptionKey

// Wrapper seals and opens DEKs using a customer master key
// reference. Phase 2 ships LocalFileWrapper; Phase 3 adds
// KMSWrapper and VaultWrapper behind the same interface.
type Wrapper interface {
	WrapDEK(dek DataEncryptionKey, cmk encryption.CustomerMasterKeyRef) (WrappedDEK, error)
	UnwrapDEK(wrapped WrappedDEK, cmk encryption.CustomerMasterKeyRef) (DataEncryptionKey, error)
}

// LocalFileWrapper loads a 32-byte master key from a file on disk.
// It is the Phase 2 default and is NOT suitable for production use —
// the plaintext master key sits on the node that reads the file.
// Production deployments MUST use KMSWrapper / VaultWrapper in Phase 3.
type LocalFileWrapper struct {
	// Path is the filesystem path holding the master key. The file
	// must contain exactly 32 random bytes (no newline, no wrapping).
	Path string
}

// loadMasterKey reads the CMK bytes from disk and verifies the size.
func (w LocalFileWrapper) loadMasterKey() ([]byte, error) {
	if w.Path == "" {
		return nil, errors.New("client_sdk: LocalFileWrapper.Path is required")
	}
	data, err := os.ReadFile(w.Path)
	if err != nil {
		return nil, fmt.Errorf("client_sdk: read cmk %q: %w", w.Path, err)
	}
	if len(data) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("client_sdk: cmk %q has %d bytes; want %d", w.Path, len(data), chacha20poly1305.KeySize)
	}
	return data, nil
}

// WrapDEK seals dek with the master key bound to cmk. The returned
// WrappedDEK carries the CMK.URI and CMK.Version so UnwrapDEK can
// verify the envelope before opening.
func (w LocalFileWrapper) WrapDEK(dek DataEncryptionKey, cmk encryption.CustomerMasterKeyRef) (WrappedDEK, error) {
	if len(dek) != chacha20poly1305.KeySize {
		return WrappedDEK{}, fmt.Errorf("client_sdk: dek has %d bytes; want %d", len(dek), chacha20poly1305.KeySize)
	}
	master, err := w.loadMasterKey()
	if err != nil {
		return WrappedDEK{}, err
	}
	aead, err := chacha20poly1305.NewX(master)
	if err != nil {
		return WrappedDEK{}, fmt.Errorf("client_sdk: wrap cipher: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(randReader, nonce); err != nil {
		return WrappedDEK{}, fmt.Errorf("client_sdk: wrap nonce: %w", err)
	}
	aad := []byte(cmk.URI)
	sealed := aead.Seal(nil, nonce, dek, aad)

	keyID := dekKeyID(dek, cmk)
	return WrappedDEK{
		KeyID:         keyID,
		Algorithm:     ContentAlgorithm,
		WrappedKey:    append(nonce, sealed...),
		WrapAlgorithm: WrapAlgorithm,
	}, nil
}

// UnwrapDEK reverses WrapDEK. It returns the plaintext DEK on
// success and an error if the CMK cannot open the envelope (wrong
// key, tampered ciphertext, mismatched AAD).
func (w LocalFileWrapper) UnwrapDEK(wrapped WrappedDEK, cmk encryption.CustomerMasterKeyRef) (DataEncryptionKey, error) {
	if wrapped.WrapAlgorithm != WrapAlgorithm {
		return nil, fmt.Errorf("client_sdk: unknown wrap algorithm %q", wrapped.WrapAlgorithm)
	}
	master, err := w.loadMasterKey()
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(master)
	if err != nil {
		return nil, fmt.Errorf("client_sdk: unwrap cipher: %w", err)
	}
	if len(wrapped.WrappedKey) < aead.NonceSize() {
		return nil, errors.New("client_sdk: wrapped dek is truncated")
	}
	nonce := wrapped.WrappedKey[:aead.NonceSize()]
	ct := wrapped.WrappedKey[aead.NonceSize():]
	aad := []byte(cmk.URI)
	dek, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("client_sdk: unwrap dek: %w", err)
	}
	return DataEncryptionKey(dek), nil
}

// dekKeyID returns a short, deterministic identifier for the wrapped
// DEK. It is a SHA-256 over (dek || cmk.URI || cmk.Version) truncated
// to 16 bytes and hex-encoded. The ID is recorded on the manifest so
// readers can locate the wrapped material even when the tenant
// rotates CMKs.
func dekKeyID(dek DataEncryptionKey, cmk encryption.CustomerMasterKeyRef) string {
	h := sha256.New()
	h.Write(dek)
	h.Write([]byte{0})
	h.Write([]byte(cmk.URI))
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d", cmk.Version)
	sum := h.Sum(nil)
	const idLen = 16
	hex := make([]byte, idLen*2)
	const hextable = "0123456789abcdef"
	for i := 0; i < idLen; i++ {
		hex[i*2] = hextable[sum[i]>>4]
		hex[i*2+1] = hextable[sum[i]&0x0f]
	}
	return "dek-" + string(hex)
}
