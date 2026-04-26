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
	TenantID string `json:"tenant_id"`
	Bucket   string `json:"bucket"`
	// ObjectKey is the opaque byte-string that identifies the object
	// within (tenant, bucket). In managed-encryption mode the gateway
	// sees plaintext S3 keys so this field carries them verbatim; in
	// strict zero-knowledge mode the client SDK wraps the key with
	// tenant-held material before PUT so the field carries ciphertext.
	// Either way, LIST round-trips it back to clients so subsequent
	// GET/HEAD/DELETE calls can address the object directly.
	ObjectKey     string `json:"object_key"`
	ObjectKeyHash string `json:"object_key_hash"`
	VersionID     string `json:"version_id"`
	ObjectSize    int64  `json:"object_size"`
	ChunkSize     int64  `json:"chunk_size"`
	// ContentHash is the BLAKE3 hash used for intra-tenant dedup lookups.
	// For Pattern B (gateway convergent): BLAKE3(plaintext).
	// For Pattern C (client-side convergent): BLAKE3(ciphertext).
	// Empty when dedup is not enabled on the bucket.
	ContentHash     string           `json:"content_hash,omitempty"`
	Encryption      EncryptionConfig `json:"encryption"`
	PlacementPolicy PlacementPolicy  `json:"placement_policy"`
	Pieces          []Piece          `json:"pieces"`
	MigrationState  MigrationState   `json:"migration_state"`
}

// EncryptionConfig describes how the object is encrypted.
//
// Mode values (shared with encryption.EncryptionMode and
// placement_policy.EncryptionSpec):
//   - "":                    legacy / pre-encryption object (backward compat)
//   - "client_side":         strict zero-knowledge, customer-held DEK
//   - "managed":             gateway-side encryption (confidential managed storage)
//   - "public_distribution": ciphertext at rest, plaintext at the edge
//
// For "managed" and "public_distribution" the gateway wraps the
// per-object DEK with its CMK and stores the sealed bytes on
// WrappedDEK so the GET path can unwrap them at read time. For
// "client_side" these fields are empty — the client holds the DEK.
type EncryptionConfig struct {
	Mode              string `json:"mode"`
	Algorithm         string `json:"algorithm"`
	KeyID             string `json:"key_id"`
	WrappedDEK        []byte `json:"wrapped_dek,omitempty"`
	WrapAlgorithm     string `json:"wrap_algorithm,omitempty"`
	ManifestEncrypted bool   `json:"manifest_encrypted"`
}

// Piece is a single backend-stored chunk of ciphertext.
//
// Most objects are single-piece (PartNumber = 0, ShardIndex = 0).
// Multi-piece manifests arise in two places:
//
//   - S3 multipart uploads: PartNumber 1..N identifies the part the
//     piece came from. The manifest lists pieces in ascending
//     PartNumber order and the GET path concatenates them.
//
//   - Erasure coding: StripeIndex 0..S-1 and ShardIndex 0..k+m-1
//     identify the (stripe, shard) position of the piece. ShardKind
//     names the role (ShardKindData or ShardKindParity). The GET
//     path fetches at least k data shards per stripe and decodes;
//     missing shards can be reconstructed from any k surviving
//     shards per stripe.
//
// Multipart and erasure coding never combine in the same manifest:
// the gateway applies EC to a multipart upload at the per-part
// level or the whole-object level, not both.
type Piece struct {
	PieceID string `json:"piece_id"`
	Hash    string `json:"hash"`
	Backend string `json:"backend"`
	Locator string `json:"locator"`
	State   string `json:"state"`

	// PartNumber is the 1-based S3 multipart index. Zero means the
	// piece was not uploaded via multipart.
	PartNumber int `json:"part_number,omitempty"`

	// SizeBytes is the on-wire size of this specific piece. For
	// multipart the handler needs per-part sizes to emit correct
	// Content-Range responses; for EC manifests it records the
	// shard size.
	SizeBytes int64 `json:"size_bytes,omitempty"`

	// StripeIndex is the 0-based stripe index within an
	// erasure-coded object. Zero when the manifest is not EC.
	StripeIndex int `json:"stripe_index,omitempty"`

	// ShardIndex is the 0-based shard index within a stripe
	// (0 .. DataShards+ParityShards-1). Zero when the manifest
	// is not EC.
	ShardIndex int `json:"shard_index,omitempty"`

	// ShardKind is "data" or "parity" for EC pieces and empty for
	// non-EC pieces.
	ShardKind string `json:"shard_kind,omitempty"`
}

// Shard kinds for EC pieces.
const (
	ShardKindData   = "data"
	ShardKindParity = "parity"
)

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

	// ErasureProfile, when non-empty, names a profile registered in
	// metadata/erasure_coding that the gateway uses to shard
	// objects on this tenant/bucket. Empty means single-piece
	// writes (the provider's own durability takes over).
	ErasureProfile string `json:"erasure_profile,omitempty"`

	// EncryptionMode is the per-object encryption mode chosen by
	// the tenant's policy at PUT time. Valid values mirror
	// EncryptionConfig.Mode ("client_side", "managed",
	// "public_distribution"). Empty means the tenant has no
	// policy and the gateway treats the object as legacy /
	// unencrypted for backward compatibility.
	EncryptionMode string `json:"encryption_mode,omitempty"`

	// DedupPolicy controls intra-tenant deduplication for this
	// object. nil disables dedup; this is the field's zero value
	// and is what `omitempty` actually omits (a value-type
	// DedupPolicy with all zero fields is never omitted by
	// encoding/json). See docs/PROPOSAL.md §3.14.
	DedupPolicy *DedupPolicy `json:"dedup_policy,omitempty"`
}

// DedupPolicy controls intra-tenant deduplication for this object.
// See docs/PROPOSAL.md §3.14.
//
// Scope is always "intra_tenant"; cross-tenant dedup is permanently
// excluded from the fabric. Level selects the dedup tier:
//
//   - "object":       gateway-managed object-level dedup via
//     metadata/content_index (all backends).
//   - "object+block": object-level dedup plus Ceph RGW native
//     RADOS-tier chunk dedup (dedicated B2B cells
//     only).
type DedupPolicy struct {
	Enabled bool   `json:"enabled"`
	Scope   string `json:"scope"` // always "intra_tenant"
	Level   string `json:"level"` // "object" or "object+block"
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
