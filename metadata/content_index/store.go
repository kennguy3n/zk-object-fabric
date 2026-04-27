// Package content_index defines the intra-tenant deduplication
// content-index store. See docs/PROPOSAL.md §3.14.
//
// The content index maps `(tenant_id, content_hash)` to the physical
// piece that holds the deduped ciphertext. The primary key is
// scoped to tenant_id by design: cross-tenant deduplication is
// permanently excluded from the fabric.
//
// The Store interface is the integration boundary the gateway
// consumes; concrete implementations live in subpackages
// (postgres/, in-memory test fakes, etc.) and are wired through
// dependency injection at gateway startup.
//
// Phase 3.5 status: scaffolding only. Implementations land
// alongside the PUT/GET/DELETE wiring in subsequent PRs.
package content_index

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by Lookup / IncrementRef / DecrementRef
// when no row exists for `(tenant_id, content_hash)`.
var ErrNotFound = errors.New("content_index: entry not found")

// ErrAlreadyExists is returned by Register when a row already
// exists for `(tenant_id, content_hash)`. The PUT path treats this
// as a race-recovery signal: drop the just-written piece and call
// IncrementRef on the existing row instead.
var ErrAlreadyExists = errors.New("content_index: entry already exists")

// ErrInvalidRefCount is returned by DecrementRef when the existing
// RefCount is already zero or negative. The schema's CHECK
// constraint forbids RefCount < 0; this sentinel surfaces the
// programmer error of decrementing a row that should have already
// been deleted.
var ErrInvalidRefCount = errors.New("content_index: invalid refcount")

// ErrRefCountNonZero is returned by Delete when the row exists but
// its RefCount is greater than zero. The DELETE handler races
// concurrent PUTs that may IncrementRef between the caller's
// DecrementRef returning 0 and this Delete; surfacing it as a
// distinct error lets the handler skip the backend piece deletion
// (the piece is still needed by the racing uploader) without
// confusing it with the row-actually-missing case.
var ErrRefCountNonZero = errors.New("content_index: ref_count not zero")

// ContentIndexEntry is a single row in the content_index table.
//
// See docs/PROPOSAL.md §3.14.3 for the canonical schema; the SQL
// definition lives in schema.sql alongside this file.
type ContentIndexEntry struct {
	// TenantID scopes the entry. Cross-tenant lookups are not
	// supported; the primary key is (TenantID, ContentHash).
	TenantID string

	// ContentHash is the BLAKE3 hash used as the dedup lookup
	// key. For Pattern B (gateway convergent) it is
	// BLAKE3(plaintext); for Pattern C (client-side convergent)
	// it is BLAKE3(ciphertext). Encoded as a hex or
	// base64-without-padding string at the storage boundary.
	ContentHash string

	// PieceID identifies the physical piece on the backend that
	// holds the deduped ciphertext. Manifests of deduped
	// objects all reference this same PieceID.
	PieceID string

	// Backend names the provider that holds the piece (e.g.
	// "wasabi", "ceph_rgw", "backblaze_b2"). Required for
	// orphan GC and for routing GETs to the correct adapter.
	Backend string

	// RefCount tracks how many manifests reference this piece.
	// DELETE decrements; only the final DELETE (RefCount == 0)
	// triggers backend removal.
	RefCount int

	// SizeBytes is the on-wire size of the deduped piece. Used
	// for billing reconciliation and orphan-GC accounting.
	SizeBytes int64

	// ETag is the original PUT response ETag for the canonical
	// piece (i.e. the value the first uploader's S3 client saw
	// in the PUT response). Recorded at Register time so that a
	// dedup-hit PUT, GET, and HEAD return the same ETag any
	// non-dedup PUT of the same content would have returned.
	// Optional for entries written before this field existed.
	ETag string

	// CreatedAt is set by the store at first INSERT.
	CreatedAt time.Time
}

// Store is the persistence boundary for the content index. All
// methods are scoped to a single tenant; the store is responsible
// for ensuring its underlying transactions never read or write
// across tenants.
//
// Implementations must be safe for concurrent use. The PUT path
// races multiple uploads of the same content within a tenant, so
// IncrementRef / Register must be atomic against ErrNotFound.
type Store interface {
	// Lookup returns the entry for (tenantID, contentHash) or
	// ErrNotFound if no row exists. It does not mutate
	// RefCount.
	Lookup(ctx context.Context, tenantID, contentHash string) (*ContentIndexEntry, error)

	// Register inserts a new entry with RefCount = 1. It is the
	// "miss" path: the gateway has just written a new piece and
	// needs to record it. Returns an error if a row already
	// exists for the key (the caller should retry the PUT path
	// with IncrementRef).
	Register(ctx context.Context, entry ContentIndexEntry) error

	// IncrementRef atomically increments RefCount on an
	// existing row. Used by the "hit" path of dedup PUT.
	// Returns ErrNotFound if no row exists.
	IncrementRef(ctx context.Context, tenantID, contentHash string) error

	// DecrementRef atomically decrements RefCount on an
	// existing row and returns the new count. Used by the
	// DELETE path. Callers MUST call provider.DeletePiece and
	// then Delete when newCount == 0. Returns ErrNotFound if
	// no row exists.
	DecrementRef(ctx context.Context, tenantID, contentHash string) (newCount int, err error)

	// Delete removes the row for (tenantID, contentHash) only
	// when its RefCount is zero. Returns ErrNotFound if no row
	// exists, and ErrRefCountNonZero if a concurrent
	// IncrementRef bumped the count between the caller's
	// DecrementRef and this Delete. The caller MUST attempt
	// Delete BEFORE deleting the backend piece, and skip the
	// backend piece deletion when this method returns
	// ErrRefCountNonZero — the racing uploader is now the
	// canonical reference.
	Delete(ctx context.Context, tenantID, contentHash string) error

	// ScanAll returns every content_index row for the given
	// tenant. It is used by the orphan GC sweep to identify
	// rows whose piece is no longer referenced by any live
	// manifest. Implementations MAY page or stream internally
	// but the surface returns the full slice; callers are
	// expected to invoke it on a per-tenant basis so the slice
	// stays bounded.
	ScanAll(ctx context.Context, tenantID string) ([]ContentIndexEntry, error)

	// ListTenants returns the set of distinct tenant_ids that
	// have at least one content_index row. The orphan GC sweep
	// uses it to enumerate work without requiring an external
	// tenant directory.
	ListTenants(ctx context.Context) ([]string, error)
}
