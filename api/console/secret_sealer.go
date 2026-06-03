package console

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// SecretSealer seals and opens a control-plane secret for storage at
// rest in the SQL-backed MFA stores. Today the only sealed value is the
// TOTP shared secret, which — unlike a password or refresh token —
// cannot be hashed because the verifier needs the secret itself to
// recompute codes. Sealing it under a gateway-held key means a database
// admin (or an attacker with only read access to the control-plane
// store) cannot read every enrolled tenant's TOTP seed straight out of
// the mfa_credentials.secret column, which would otherwise be a standing
// second-factor bypass for the whole deployment.
//
// A nil SecretSealer stores the secret in the clear. That is the
// dev / in-memory posture and mirrors manifest_store.BodyEncryptor:
// the persistent backends seal before INSERT and open after SELECT only
// when a sealer is configured. The gateway wires a sealer whenever the
// at-rest key is configured, and the existing production manifest-body
// guard already refuses to boot a persistent control-plane store under
// env=production without that key — so a persistent MFA store in
// production is always sealed.
type SecretSealer interface {
	// Seal returns the at-rest form of plaintext, bound to tenantID so
	// a sealed secret cannot be lifted onto another tenant's row and
	// opened. The returned string is safe to store in a TEXT column.
	Seal(plaintext, tenantID string) (string, error)
	// Open is the inverse of Seal. A value that was never sealed (a
	// legacy plaintext row written before a key was configured) is
	// returned unchanged, so enabling sealing does not require
	// re-keying existing rows up front.
	Open(stored, tenantID string) (string, error)
}

// mfaStoreOptions collects optional construction settings shared by the
// SQL-backed MFA stores.
type mfaStoreOptions struct {
	sealer SecretSealer
}

// MFAStoreOption configures a SQL-backed MFAStore at construction.
type MFAStoreOption func(*mfaStoreOptions)

// WithSecretSealer makes a SQL-backed MFA store seal the TOTP secret at
// rest. When omitted (or nil), the secret is stored in the clear.
func WithSecretSealer(sealer SecretSealer) MFAStoreOption {
	return func(o *mfaStoreOptions) { o.sealer = sealer }
}

func applyMFAStoreOptions(opts []MFAStoreOption) mfaStoreOptions {
	var o mfaStoreOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// sealSecretWith returns the at-rest form of plaintext, or plaintext
// unchanged when no sealer is configured (dev / in-memory posture).
func sealSecretWith(sealer SecretSealer, plaintext, tenantID string) (string, error) {
	if sealer == nil {
		return plaintext, nil
	}
	return sealer.Seal(plaintext, tenantID)
}

// openSecretWith is the inverse of sealSecretWith.
func openSecretWith(sealer SecretSealer, stored, tenantID string) (string, error) {
	if sealer == nil {
		return stored, nil
	}
	return sealer.Open(stored, tenantID)
}

// sealedSecretPrefix tags a value produced by AEADSecretSealer.Seal.
// A base32 TOTP secret never contains ':' so the prefix unambiguously
// distinguishes a sealed value from a legacy plaintext one, letting
// Open pass plaintext through during a rolling enablement.
const sealedSecretPrefix = "enc:v1:"

// AEADSecretSealer is the XChaCha20-Poly1305 implementation of
// SecretSealer. It frames every blob as [24-byte nonce][ciphertext with
// 16-byte Poly1305 tag], base64-encodes it, and prefixes sealedSecretPrefix.
// It mirrors manifest_store.AEADBodyEncryptor (same cipher, same framing)
// but uses an MFA-specific AAD namespace so a manifest-body ciphertext
// and an MFA-secret ciphertext are not interchangeable even under a
// shared key.
//
// The key is held by the gateway process (loaded from the same at-rest
// key file as the manifest body encryptor) and MUST NOT be shared with
// any tenant or operator who only has database access.
type AEADSecretSealer struct {
	aead cipherAEADSealer
}

// cipherAEADSealer is a tiny local interface so the struct field is
// testable with a fake and the package does not need crypto/cipher just
// for the type.
type cipherAEADSealer interface {
	NonceSize() int
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}

// NewAEADSecretSealer returns a sealer keyed off the given 32 bytes.
func NewAEADSecretSealer(key []byte) (*AEADSecretSealer, error) {
	if len(key) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("console: secret sealer key must be %d bytes, got %d", chacha20poly1305.KeySize, len(key))
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("console: new xchacha20-poly1305: %w", err)
	}
	return &AEADSecretSealer{aead: aead}, nil
}

// secretAAD binds a sealed secret to its tenant. The "mfa-secret-v1|"
// namespace also separates these ciphertexts from any other use of the
// same key (e.g. manifest bodies), so a blob sealed for one purpose
// fails to open for another.
func secretAAD(tenantID string) []byte {
	return []byte("mfa-secret-v1|" + tenantID)
}

// Seal implements SecretSealer.
func (e *AEADSecretSealer) Seal(plaintext, tenantID string) (string, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("console: seal secret nonce: %w", err)
	}
	ct := e.aead.Seal(nil, nonce, []byte(plaintext), secretAAD(tenantID))
	blob := make([]byte, 0, len(nonce)+len(ct))
	blob = append(blob, nonce...)
	blob = append(blob, ct...)
	return sealedSecretPrefix + base64.RawStdEncoding.EncodeToString(blob), nil
}

// Open implements SecretSealer.
func (e *AEADSecretSealer) Open(stored, tenantID string) (string, error) {
	rest, sealed := strings.CutPrefix(stored, sealedSecretPrefix)
	if !sealed {
		// A value written before sealing was enabled. Pass it through
		// so a deployment can turn on sealing without re-keying rows.
		return stored, nil
	}
	blob, err := base64.RawStdEncoding.DecodeString(rest)
	if err != nil {
		return "", fmt.Errorf("console: decode sealed secret: %w", err)
	}
	ns := e.aead.NonceSize()
	if len(blob) < ns {
		return "", errors.New("console: sealed secret too short")
	}
	pt, err := e.aead.Open(nil, blob[:ns], blob[ns:], secretAAD(tenantID))
	if err != nil {
		return "", fmt.Errorf("console: open sealed secret: %w", err)
	}
	return string(pt), nil
}
