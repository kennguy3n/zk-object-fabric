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
	Put(ctx context.Context, key ManifestKey, m *metadata.ObjectManifest) error

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

// EncodeScanCursor serialises a ManifestKey into the opaque cursor
// token ScanManifests returns. The four key fields are arbitrary
// byte-strings (object keys and hashes can contain any character), so
// a delimiter-joined form would be ambiguous; JSON + base64 sidesteps
// escaping entirely and keeps the token URL-safe. Both the memory and
// Postgres stores use this codec so a cursor is portable across them.
func EncodeScanCursor(key ManifestKey) string {
	b, _ := json.Marshal(key)
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
	var key ManifestKey
	if err := json.Unmarshal(raw, &key); err != nil {
		return ManifestKey{}, errors.New("manifest_store: malformed scan cursor")
	}
	return key, nil
}
