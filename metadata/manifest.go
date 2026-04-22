// Package metadata defines provider-neutral object manifest types.
//
// The manifest format is the canonical specification defined in
// docs/PROPOSAL.md §3.3. It decouples customer-visible objects from
// backend provider locators so that backends can be added, removed,
// or migrated without customer-facing changes.
package metadata

import (
	"encoding/json"
	"fmt"
)

// ObjectManifest is the provider-neutral description of a single
// customer object. See docs/PROPOSAL.md §3.3.
type ObjectManifest struct {
	TenantID        string           `json:"tenant_id"`
	Bucket          string           `json:"bucket"`
	ObjectKeyHash   string           `json:"object_key_hash"`
	VersionID       string           `json:"version_id"`
	ObjectSize      int64            `json:"object_size"`
	ChunkSize       int64            `json:"chunk_size"`
	Encryption      EncryptionConfig `json:"encryption"`
	PlacementPolicy PlacementPolicy  `json:"placement_policy"`
	Pieces          []Piece          `json:"pieces"`
	MigrationState  MigrationState   `json:"migration_state"`
}

// EncryptionConfig describes how the object is encrypted.
//
// Mode values (shared with encryption.EncryptionMode and
// placement_policy.EncryptionSpec):
//   - "client_side":         strict zero-knowledge, customer-held DEK
//   - "managed":             gateway-side encryption (confidential managed storage)
//   - "public_distribution": ciphertext at rest, plaintext at the edge
type EncryptionConfig struct {
	Mode              string `json:"mode"`
	Algorithm         string `json:"algorithm"`
	KeyID             string `json:"key_id"`
	ManifestEncrypted bool   `json:"manifest_encrypted"`
}

// Piece is a single backend-stored chunk of ciphertext.
type Piece struct {
	PieceID string `json:"piece_id"`
	Hash    string `json:"hash"`
	Backend string `json:"backend"`
	Locator string `json:"locator"`
	State   string `json:"state"`
}

// MigrationState captures where the manifest sits in the cloud→local
// migration lifecycle. See docs/PROPOSAL.md §4.3.
type MigrationState struct {
	Generation     int    `json:"generation"`
	PrimaryBackend string `json:"primary_backend"`
	CloudCopy      string `json:"cloud_copy"`
}

// PlacementPolicy is the embedded, materialized placement decision for
// this specific object. The tenant-level DSL lives in
// metadata/placement_policy and is distilled into this struct at PUT
// time.
type PlacementPolicy struct {
	Residency         []string `json:"residency"`
	AllowedBackends   []string `json:"allowed_backends"`
	MinFailureDomains int      `json:"min_failure_domains"`
	HotCache          bool     `json:"hot_cache"`
}

// Validate performs minimal structural checks on a manifest. It is not
// a substitute for cryptographic verification of the manifest itself;
// it only catches obviously malformed records.
func (m *ObjectManifest) Validate() error {
	if m.TenantID == "" {
		return fmt.Errorf("manifest: tenant_id is required")
	}
	if m.Bucket == "" {
		return fmt.Errorf("manifest: bucket is required")
	}
	if m.ObjectKeyHash == "" {
		return fmt.Errorf("manifest: object_key_hash is required")
	}
	if m.ObjectSize < 0 {
		return fmt.Errorf("manifest: object_size must be non-negative")
	}
	if m.ChunkSize <= 0 && m.ObjectSize > 0 {
		return fmt.Errorf("manifest: chunk_size must be positive for non-empty objects")
	}
	for i, p := range m.Pieces {
		if p.PieceID == "" {
			return fmt.Errorf("manifest: piece[%d].piece_id is required", i)
		}
		if p.Backend == "" {
			return fmt.Errorf("manifest: piece[%d].backend is required", i)
		}
	}
	return nil
}

// MarshalJSON round-trips a manifest through encoding/json. It exists
// as a convenience wrapper so callers can switch to an encrypted
// manifest format later without changing call sites.
func (m *ObjectManifest) MarshalJSON() ([]byte, error) {
	type alias ObjectManifest
	return json.Marshal((*alias)(m))
}

// UnmarshalJSON is the mirror of MarshalJSON.
func (m *ObjectManifest) UnmarshalJSON(data []byte) error {
	type alias ObjectManifest
	return json.Unmarshal(data, (*alias)(m))
}
