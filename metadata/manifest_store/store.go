// Package manifest_store defines the ManifestStore interface.
//
// ManifestStore is the control-plane contract for reading and writing
// encrypted object manifests. See docs/PROPOSAL.md §3.3 and §3.5.
// Concrete implementations (Postgres/RDS in Phase 1, CockroachDB in
// Phase 2+) live outside this package.
package manifest_store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/kennguy3n/zk-object-fabric/metadata"
)

// ErrNotFound is returned by Get and Delete when the requested
// manifest does not exist. Callers should use errors.Is(err,
// ErrNotFound) rather than comparing with ==.
var ErrNotFound = errors.New("manifest_store: manifest not found")

// ManifestKey identifies a single manifest.
type ManifestKey struct {
	TenantID      string
	Bucket        string
	ObjectKeyHash string
	VersionID     string
}

// ManifestStore persists encrypted object manifests.
//
// All implementations MUST treat the manifest body as opaque. Policy
// decisions should be driven by the separately-indexed placement tags
// held in the control plane, not by inspecting manifest contents.
type ManifestStore interface {
	// Put stores a manifest at key. If the manifest already exists,
	// behaviour is defined by the implementation's consistency model.
	//
	// Put establishes (or re-establishes) key as the LATEST version of
	// its (tenant, bucket, object_key_hash) triple. Callers that need
	// to amend an existing version's body without changing which
	// version is latest must use UpdateManifest instead.
	Put(ctx context.Context, key ManifestKey, m *metadata.ObjectManifest) error

	// UpdateManifest replaces the opaque body of an EXISTING manifest
	// version in place, without altering latest-version ordering. The
	// key MUST name a specific stored version: VersionID is matched
	// exactly (the empty-VersionID "latest" sentinel that Get/Delete
	// accept is NOT resolved here — pass the manifest's own VersionID).
	//
	// This is the metadata-amend path for sub-resources that mutate an
	// already-stored object in place (object tagging today; retention
	// and legal-hold later). Using Put for those would promote the
	// amended version to latest — corrupting unversioned GET/HEAD/
	// DELETE/LIST resolution when the amended version is not the most
	// recent one. UpdateManifest leaves the latest pointer
	// (memory: s.latest; Postgres: updated_at; SQLite: write_seq)
	// untouched.
	//
	// Returns ErrNotFound if no manifest exists at key.
	UpdateManifest(ctx context.Context, key ManifestKey, m *metadata.ObjectManifest) error

	// Get fetches the manifest at key. It returns ErrNotFound if no
	// manifest exists at that key.
	Get(ctx context.Context, key ManifestKey) (*metadata.ObjectManifest, error)

	// Delete removes the manifest at key. It returns ErrNotFound if
	// no manifest exists at that key.
	Delete(ctx context.Context, key ManifestKey) error

	// List paginates manifests under a tenant/bucket prefix. The
	// caller supplies an opaque cursor (empty for the first page) and
	// receives a next-page cursor.
	List(ctx context.Context, tenantID, bucket, cursor string, limit int) (ListResult, error)

	// HasManifestWithPieceID reports whether the given tenant
	// has at least one manifest that references pieceID. Used
	// by the orphan GC worker to decide whether a piece is
	// still live before deleting it from the backend.
	HasManifestWithPieceID(ctx context.Context, tenantID, pieceID string) (bool, error)

	// ListVersions returns every persisted version of the
	// manifest at (tenantID, bucket, objectKeyHash), most-recent
	// first. Used by the S3 ListObjectVersions handler. Returns
	// an empty slice (not ErrNotFound) when no versions exist.
	ListVersions(ctx context.Context, tenantID, bucket, objectKeyHash string) ([]*metadata.ObjectManifest, error)

	// ScanManifests paginates over EVERY persisted manifest
	// version across all tenants and buckets, for background
	// workers that must visit every object exactly once (e.g. the
	// AAD v1 migration worker). It differs from List in two ways
	// that List cannot satisfy for a migration sweep:
	//
	//   - it is not scoped to a (tenant, bucket) — the sweep has
	//     no tenant list to iterate over; and
	//   - it returns every version, not just the latest, because
	//     older overwrite versions are independently addressable
	//     and must be migrated too.
	//
	// Results are ordered by the full primary key
	// (tenant_id, bucket, object_key_hash, version_id) so the
	// cursor is a stable keyset: pass "" for the first page and the
	// returned NextCursor for each subsequent page. NextCursor is
	// "" when the final page has been returned. Each entry carries
	// the manifest's ManifestKey so the worker can re-Put it
	// without reconstructing the key from the body.
	ScanManifests(ctx context.Context, cursor string, limit int) (ScanResult, error)
}

// ListResult is a single page of manifests returned by List.
type ListResult struct {
	Manifests  []*metadata.ObjectManifest
	NextCursor string
}

// ScannedManifest is one entry returned by ScanManifests: a manifest
// together with the key it is stored under.
type ScannedManifest struct {
	Key      ManifestKey
	Manifest *metadata.ObjectManifest
}

// ScanResult is a single page of manifests returned by ScanManifests.
type ScanResult struct {
	Manifests  []ScannedManifest
	NextCursor string
}

// scanCursor is the on-the-wire form of a scan cursor. It carries
// explicit, stable JSON tags so the cursor format is decoupled from
// ManifestKey's own struct layout: ManifestKey is currently untagged
// (it marshals to PascalCase keys), and if it later gains json tags
// for some other use, that must NOT silently change the cursor wire
// format and invalidate an in-flight sweep's cursor mid-page. Encoding
// through this dedicated type makes the cursor contract independent of
// ManifestKey's reflection.
type scanCursor struct {
	TenantID      string `json:"t"`
	Bucket        string `json:"b"`
	ObjectKeyHash string `json:"h"`
	VersionID     string `json:"v"`
}

// EncodeScanCursor serialises a ManifestKey into the opaque cursor
// token ScanManifests returns. The four key fields are arbitrary
// byte-strings (object keys and hashes can contain any character), so
// a delimiter-joined form would be ambiguous; JSON + base64 sidesteps
// escaping entirely and keeps the token URL-safe.
//
// All stores (memory, Postgres, SQLite) share this codec, so the
// cursor's *encoding* is identical everywhere. Resuming a cursor on a
// different store than it was minted on additionally requires the two
// stores to agree on the keyset *ordering*: memory uses Go string
// comparison, Postgres uses its column collation, SQLite uses BINARY.
// Those agree for the byte values these key fields actually hold —
// ObjectKeyHash and VersionID are hex SHA-256, TenantID/Bucket are
// system-issued ASCII — so the cursor is portable in practice. A field
// carrying non-ASCII bytes could order differently across stores; none
// does today, and the keyset would have to be byte-ordered uniformly
// before that changed.
func EncodeScanCursor(key ManifestKey) string {
	b, _ := json.Marshal(scanCursor{
		TenantID:      key.TenantID,
		Bucket:        key.Bucket,
		ObjectKeyHash: key.ObjectKeyHash,
		VersionID:     key.VersionID,
	})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeScanCursor reverses EncodeScanCursor. An empty cursor decodes
// to the zero ManifestKey (the start of the keyset). A malformed
// cursor is an error rather than a silent restart so a caller cannot
// accidentally re-scan the whole table from a corrupted token.
func DecodeScanCursor(cursor string) (ManifestKey, error) {
	if cursor == "" {
		return ManifestKey{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return ManifestKey{}, errors.New("manifest_store: malformed scan cursor")
	}
	var c scanCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return ManifestKey{}, errors.New("manifest_store: malformed scan cursor")
	}
	return ManifestKey{
		TenantID:      c.TenantID,
		Bucket:        c.Bucket,
		ObjectKeyHash: c.ObjectKeyHash,
		VersionID:     c.VersionID,
	}, nil
}
