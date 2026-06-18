// Encryption wiring for the S3-compatible handler.
//
// The gateway consumes the encryption SDK at the HTTP surface so
// tenant-level encryption policy (managed / public_distribution /
// client_side) is applied uniformly across single-piece,
// erasure-coded, and multipart paths. DEK wrapping is pluggable
// behind the Wrapper interface: LocalFileWrapper, KMSWrapper, and
// VaultWrapper are interchangeable without touching the handler.

package s3compat

import (
	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
)

// GatewayEncryption holds the gateway's key material for managed
// and public_distribution encryption modes. A nil GatewayEncryption
// on the Handler Config causes PUTs for those modes to fail with
// EncryptionNotConfigured.
type GatewayEncryption struct {
	// Wrapper wraps / unwraps per-object DEKs. Any client_sdk
	// Wrapper implementation (LocalFileWrapper, KMSWrapper,
	// VaultWrapper) plugs in behind the same interface.
	Wrapper client_sdk.Wrapper

	// CMK is the customer master key reference used for wrapping.
	// The URI is recorded on each manifest's EncryptionConfig.KeyID
	// so readers can address the right wrapped DEK even after a
	// key rotation.
	CMK encryption.CustomerMasterKeyRef
}
