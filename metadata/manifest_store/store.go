// Package manifest_store defines the ManifestStore interface.
//
// ManifestStore is the control-plane contract for reading and writing
// encrypted object manifests. See docs/PROPOSAL.md §3.3 and §3.5.
// Concrete implementations (Postgres/RDS in Phase 1, CockroachDB in
// Phase 2+) live outside this package.
package manifest_store

import (
	"context"
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
}

// ListResult is a single page of manifests returned by List.
type ListResult struct {
	Manifests  []*metadata.ObjectManifest
	NextCursor string
}
