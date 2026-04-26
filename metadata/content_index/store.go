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

	// Delete removes the row for (tenantID, contentHash). The
	// caller is expected to have already deleted the underlying
	// piece from the backend. Returns ErrNotFound if no row
	// exists.
	Delete(ctx context.Context, tenantID, contentHash string) error
}
