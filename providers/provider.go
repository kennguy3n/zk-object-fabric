// Package providers defines the StorageProvider interface that every
// storage backend (Wasabi, Backblaze B2, Cloudflare R2, AWS S3, local
// DC cell, local fs dev) must implement.
//
// The interface is the canonical abstraction described in
// docs/PROPOSAL.md §3.4. It lets the fabric add, remove, and migrate
// between backends without customer-visible changes.
package providers

import (
	"context"
	"io"
)

// StorageProvider is the backend-neutral interface used by the gateway
// and migration engine to manipulate ciphertext pieces.
type StorageProvider interface {
	PutPiece(ctx context.Context, pieceID string, r io.Reader, opts PutOptions) (PutResult, error)
	GetPiece(ctx context.Context, pieceID string, byteRange *ByteRange) (io.ReadCloser, error)
	HeadPiece(ctx context.Context, pieceID string) (PieceMetadata, error)
	DeletePiece(ctx context.Context, pieceID string) error
	ListPieces(ctx context.Context, prefix, cursor string) (ListResult, error)
	Capabilities() ProviderCapabilities
	CostModel() ProviderCostModel
	PlacementLabels() PlacementLabels
}

// PutOptions carries per-request hints and metadata for a PutPiece call.
type PutOptions struct {
	// ContentLength is the expected number of bytes. -1 means unknown
	// (provider must handle streaming with no length header).
	ContentLength int64
	// ContentType is an opaque MIME type stored with the piece. It is
	// never used for content inspection.
	ContentType string
	// StorageClass is a provider-specific tier hint (e.g. "standard",
	// "archive", "deep_archive").
	StorageClass string
	// Metadata is a small set of user-defined key/value pairs stored
	// alongside the piece. Values are opaque and never interpreted.
	Metadata map[string]string
	// IfNoneMatch, when true, asks the provider to fail if the piece
	// already exists. Providers that cannot honour this MUST return an
	// error.
	IfNoneMatch bool
}

// PutResult reports the outcome of a successful PutPiece.
type PutResult struct {
	PieceID   string
	ETag      string
	SizeBytes int64
	Backend   string
	Locator   string
}

// ByteRange is a closed byte range [Start, End] where both endpoints
// are inclusive. End == -1 means "to the end of the piece".
type ByteRange struct {
	Start int64
	End   int64
}

// PieceMetadata describes an existing piece without transferring its
// bytes.
type PieceMetadata struct {
	PieceID      string
	SizeBytes    int64
	ETag         string
	ContentType  string
	StorageClass string
	Metadata     map[string]string
}

// ListResult is a single page of ListPieces output.
type ListResult struct {
	Pieces     []PieceMetadata
	NextCursor string
}

// ProviderCapabilities reports what a backend can and cannot do. The
// placement engine consults these flags before routing a request.
type ProviderCapabilities struct {
	SupportsRangeReads     bool
	SupportsMultipart      bool
	SupportsIfNoneMatch    bool
	SupportsServerSideCopy bool
	MaxObjectSizeBytes     int64
	MinStorageDurationDays int
}

// ProviderCostModel is a coarse per-provider price snapshot used by
// the placement engine. Units are USD unless noted otherwise.
type ProviderCostModel struct {
	StorageUSDPerTBMonth  float64
	EgressUSDPerGB        float64
	PutRequestUSDPer1000  float64
	GetRequestUSDPer1000  float64
	MinStorageDurationDay int
	FreeEgressRatio       float64
}

// PlacementLabels are the provider-side tags that the placement engine
// matches against tenant policies.
type PlacementLabels struct {
	Provider     string
	Region       string
	Country      string
	StorageClass string
	FailureZone  string
	Tags         map[string]string
}
