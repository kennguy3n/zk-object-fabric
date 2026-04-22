// Package dual_write implements the dual-write StorageProvider used
// during the DualWrite migration phase (see migration.DualWrite).
//
// During a cloud→local cut-over the fabric needs every new write to
// land on both the legacy origin (Wasabi) and the new destination
// (the local Ceph RGW cell or another provider), while reads
// preferentially hit the primary until the migration advances.
// DualWriteProvider is the StorageProvider shim that enforces that
// policy behind the gateway's ordinary provider registry lookup.
package dual_write

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// DualWriteProvider wraps two StorageProviders: Primary is the
// authoritative origin during the migration; Secondary is the
// destination being populated. Writes go to both; reads prefer
// Primary with Secondary as fallback.
type DualWriteProvider struct {
	Name      string
	Primary   providers.StorageProvider
	Secondary providers.StorageProvider
	// Logger receives secondary-side write failures so they surface
	// in the gateway log without failing the request.
	Logger *log.Logger
}

// New returns a DualWriteProvider. name is the registry key the
// gateway exposes (typically "dual_write").
func New(name string, primary, secondary providers.StorageProvider) *DualWriteProvider {
	return &DualWriteProvider{
		Name:      name,
		Primary:   primary,
		Secondary: secondary,
	}
}

// PutPiece writes to the primary and then to the secondary. A
// primary failure aborts the request; a secondary failure is logged
// but not propagated — the background rebalancer will pick up the
// gap on its next pass.
func (d *DualWriteProvider) PutPiece(ctx context.Context, pieceID string, r io.Reader, opts providers.PutOptions) (providers.PutResult, error) {
	if d.Primary == nil || d.Secondary == nil {
		return providers.PutResult{}, errors.New("dual_write: both providers are required")
	}
	// Primary needs the body; tee into a buffer so secondary can
	// replay it. For Phase 2 we buffer to memory which is consistent
	// with ChunkSize-bounded pieces.
	buf, err := io.ReadAll(r)
	if err != nil {
		return providers.PutResult{}, fmt.Errorf("dual_write: buffer piece: %w", err)
	}

	primaryRes, err := d.Primary.PutPiece(ctx, pieceID, bytesReader(buf), opts)
	if err != nil {
		return providers.PutResult{}, fmt.Errorf("dual_write: primary put: %w", err)
	}
	if _, err := d.Secondary.PutPiece(ctx, pieceID, bytesReader(buf), opts); err != nil {
		d.logf("dual_write: secondary put %q: %v", pieceID, err)
	}
	return primaryRes, nil
}

// GetPiece reads from the primary; on not-found (or error) it falls
// back to the secondary. Range reads are forwarded verbatim.
func (d *DualWriteProvider) GetPiece(ctx context.Context, pieceID string, byteRange *providers.ByteRange) (io.ReadCloser, error) {
	rc, err := d.Primary.GetPiece(ctx, pieceID, byteRange)
	if err == nil {
		return rc, nil
	}
	d.logf("dual_write: primary get %q failed: %v; falling back to secondary", pieceID, err)
	return d.Secondary.GetPiece(ctx, pieceID, byteRange)
}

// HeadPiece probes the primary first, then the secondary.
func (d *DualWriteProvider) HeadPiece(ctx context.Context, pieceID string) (providers.PieceMetadata, error) {
	md, err := d.Primary.HeadPiece(ctx, pieceID)
	if err == nil {
		return md, nil
	}
	return d.Secondary.HeadPiece(ctx, pieceID)
}

// DeletePiece deletes from both backends best-effort. A secondary
// failure is logged; a primary failure is returned.
func (d *DualWriteProvider) DeletePiece(ctx context.Context, pieceID string) error {
	primaryErr := d.Primary.DeletePiece(ctx, pieceID)
	if err := d.Secondary.DeletePiece(ctx, pieceID); err != nil {
		d.logf("dual_write: secondary delete %q: %v", pieceID, err)
	}
	return primaryErr
}

// ListPieces delegates to the primary. LIST is a read-through and
// must stay in sync with the manifest store, which the dual-write
// proxy does not touch; the gateway handles cross-backend LIST at
// the ObjectManifest layer.
func (d *DualWriteProvider) ListPieces(ctx context.Context, prefix, cursor string) (providers.ListResult, error) {
	return d.Primary.ListPieces(ctx, prefix, cursor)
}

// Capabilities returns the intersection of primary and secondary
// capabilities so callers never rely on features only one backend
// supports.
func (d *DualWriteProvider) Capabilities() providers.ProviderCapabilities {
	p := d.Primary.Capabilities()
	s := d.Secondary.Capabilities()
	return providers.ProviderCapabilities{
		SupportsRangeReads:     p.SupportsRangeReads && s.SupportsRangeReads,
		SupportsMultipart:      p.SupportsMultipart && s.SupportsMultipart,
		SupportsIfNoneMatch:    p.SupportsIfNoneMatch && s.SupportsIfNoneMatch,
		SupportsServerSideCopy: p.SupportsServerSideCopy && s.SupportsServerSideCopy,
		MaxObjectSizeBytes:     minInt64(p.MaxObjectSizeBytes, s.MaxObjectSizeBytes),
		MinStorageDurationDays: maxInt(p.MinStorageDurationDays, s.MinStorageDurationDays),
	}
}

// CostModel returns the primary's cost model. Dual-write is a
// transient topology; placement decisions are made against the real
// target backends, not the shim.
func (d *DualWriteProvider) CostModel() providers.ProviderCostModel {
	return d.Primary.CostModel()
}

// PlacementLabels returns the primary's labels for the same reason.
func (d *DualWriteProvider) PlacementLabels() providers.PlacementLabels {
	return d.Primary.PlacementLabels()
}

func (d *DualWriteProvider) logf(format string, args ...any) {
	if d.Logger == nil {
		return
	}
	d.Logger.Printf(format, args...)
}

// bytesReader wraps a []byte in a fresh io.Reader on every call so
// PutPiece can be replayed to the secondary backend.
func bytesReader(b []byte) io.Reader { return &bytesReplay{buf: b} }

type bytesReplay struct {
	buf []byte
	pos int
}

func (r *bytesReplay) Read(p []byte) (int, error) {
	if r.pos >= len(r.buf) {
		return 0, io.EOF
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	return n, nil
}

func minInt64(a, b int64) int64 {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Ensure DualWriteProvider satisfies the StorageProvider interface.
var _ providers.StorageProvider = (*DualWriteProvider)(nil)
