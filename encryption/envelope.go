// Package encryption defines the encryption envelope types for ZK
// Object Fabric. See docs/PROPOSAL.md §3.7.
//
// The envelope is the metadata that accompanies every encrypted
// object. It identifies the encryption mode, the wrapped DEK, and the
// customer-managed key reference used to unwrap it.
package encryption

import "fmt"

// EncryptionMode names the operating mode for an object's encryption.
type EncryptionMode string

const (
	// StrictZK: client SDK encrypts. Plaintext keys never reach the
	// service. This is the default and the marketed "zero-knowledge"
	// mode.
	StrictZK EncryptionMode = "strict_zk"

	// ManagedEncrypted: the Linode gateway encrypts. The gateway can
	// see plaintext in memory during request handling. Per
	// PROPOSAL.md §3.7 this must be sold as "confidential managed
	// storage", not as zero-knowledge.
	ManagedEncrypted EncryptionMode = "managed_encrypted"

	// PublicDistribution: ciphertext at rest, served as plaintext at
	// the edge. Useful for public assets, media, and downloads where
	// the origin still benefits from encryption but distribution is
	// explicitly public.
	PublicDistribution EncryptionMode = "public_distribution"
)

// Valid reports whether m is one of the three defined modes.
func (m EncryptionMode) Valid() bool {
	switch m {
	case StrictZK, ManagedEncrypted, PublicDistribution:
		return true
	default:
		return false
	}
}

// DataEncryptionKey is a per-object DEK in its wrapped (ciphertext)
// form. The plaintext DEK never appears in this struct; it is unwrapped
// transiently by the client SDK or, for ManagedEncrypted, by the
// gateway's KMS client.
type DataEncryptionKey struct {
	// KeyID is a stable identifier for this DEK; it is recorded on
	// the manifest so readers can locate the wrapped material.
	KeyID string `json:"key_id"`
	// Algorithm is the content-encryption algorithm the DEK is used
	// with (e.g. "xchacha20-poly1305").
	Algorithm string `json:"algorithm"`
	// WrappedKey is the opaque, CMK-wrapped DEK bytes.
	WrappedKey []byte `json:"wrapped_key"`
	// WrapAlgorithm is the algorithm used to wrap the DEK
	// (e.g. "aes-256-gcm-wrap", "rsa-oaep-sha256").
	WrapAlgorithm string `json:"wrap_algorithm"`
}

// CustomerMasterKeyRef points at the master key used to wrap DEKs.
// The reference is resolved by the client SDK (Strict ZK) or by the
// gateway's KMS client (ManagedEncrypted).
type CustomerMasterKeyRef struct {
	// URI is an opaque key locator, e.g. "cmk://acme/prod/root" or
	// "aws-kms://arn:aws:kms:...".
	URI string `json:"uri"`
	// Version is a monotonic generation number that advances when the
	// key is rotated. It is used to pin DEK wraps to a specific key
	// version.
	Version int `json:"version"`
	// HolderClass describes who holds the plaintext master key.
	// Valid values: "customer", "gateway_hsm", "none".
	HolderClass string `json:"holder_class"`
}

// EncryptionEnvelope is the per-object encryption descriptor attached
// to every manifest.
type EncryptionEnvelope struct {
	Mode              EncryptionMode       `json:"mode"`
	DEK               DataEncryptionKey    `json:"dek"`
	CMK               CustomerMasterKeyRef `json:"cmk"`
	ManifestEncrypted bool                 `json:"manifest_encrypted"`
}

// Validate performs minimal structural checks on the envelope.
func (e *EncryptionEnvelope) Validate() error {
	if !e.Mode.Valid() {
		return fmt.Errorf("encryption: unknown mode %q", e.Mode)
	}
	if e.DEK.KeyID == "" {
		return fmt.Errorf("encryption: dek.key_id is required")
	}
	if e.DEK.Algorithm == "" {
		return fmt.Errorf("encryption: dek.algorithm is required")
	}
	if e.Mode == PublicDistribution {
		return nil
	}
	if e.CMK.URI == "" {
		return fmt.Errorf("encryption: cmk.uri is required for mode %q", e.Mode)
	}
	if e.CMK.HolderClass == "" {
		return fmt.Errorf("encryption: cmk.holder_class is required for mode %q", e.Mode)
	}
	return nil
}
