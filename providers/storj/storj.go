// Package storj implements the BYOC decentralized storage backend
// for the ZK Object Fabric.
//
// Storj is a decentralized S3-compatible storage network. We talk
// to it via the native uplink Go library rather than its S3 gateway
// so we can take advantage of client-side erasure coding and the
// native piece-distribution model documented at
// https://github.com/storj/storj and https://pkg.go.dev/storj.io/uplink.
//
// The adapter is structured around an UplinkProject interface so
// tests can swap in a fake without reaching to the Storj network.
// Operators wire the real storj.io/uplink.Project at program start;
// see cmd/gateway/main.go.
package storj

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// Config is the Storj-specific runtime configuration.
type Config struct {
	// AccessGrant is the base64-encoded macaroon-backed grant a
	// Storj satellite issues for a specific project + bucket. It is
	// never logged; prefer loading it through internal/config's
	// secrets manager.
	AccessGrant string
	// Bucket is the Storj bucket holding the fabric's pieces. The
	// fabric uses one Storj bucket per (project, cell) pair.
	Bucket string
	// SatelliteAddress is an optional override for the satellite
	// URL. When empty, the satellite embedded in AccessGrant is
	// used — which is the supported path for production deploys.
	SatelliteAddress string
}

// UplinkProject is the subset of storj.io/uplink.Project we depend
// on. Keeping it narrow lets tests inject a fake without pulling
// in the real uplink dependency graph and keeps the adapter
// honest about which uplink features actually ship in this phase.
//
// Operators wire the real uplink.Project in cmd/gateway/main.go via
// NewWithUplink. The parameter types intentionally mirror the
// uplink API (io.Reader / io.ReadCloser / byte ranges) so the
// bridge is a line-for-line pass-through.
type UplinkProject interface {
	// UploadObject streams r into (bucket, key) and returns the
	// uploaded object's ETag and final size.
	UploadObject(ctx context.Context, bucket, key string, r io.Reader, opts UploadOptions) (UploadedObject, error)
	// DownloadObject returns a reader for (bucket, key). When
	// rng != nil, only the requested range is returned.
	DownloadObject(ctx context.Context, bucket, key string, rng *providers.ByteRange) (io.ReadCloser, error)
	// StatObject returns metadata without transferring bytes.
	StatObject(ctx context.Context, bucket, key string) (StatResult, error)
	// DeleteObject removes (bucket, key). Already-missing objects
	// must return a storj-ish not-found error so the adapter can
	// map it to providers.ErrNotFound (added in a future phase).
	DeleteObject(ctx context.Context, bucket, key string) error
	// ListObjects walks bucket/prefix and returns one page.
	ListObjects(ctx context.Context, bucket, prefix, cursor string) (ListPage, error)
	// Close releases the underlying uplink.Project.
	Close() error
}

// UploadOptions carries metadata that the uplink layer attaches to
// the stored object.
type UploadOptions struct {
	ContentType  string
	Metadata     map[string]string
	StorageClass string
}

// UploadedObject is the uplink.UploadInfo subset we consume.
type UploadedObject struct {
	ETag      string
	SizeBytes int64
	CreatedAt time.Time
}

// StatResult is the uplink.Object subset we consume.
type StatResult struct {
	ETag         string
	SizeBytes    int64
	ContentType  string
	StorageClass string
	Metadata     map[string]string
}

// ListPage is one page of ListObjects output.
type ListPage struct {
	Objects    []StatResult
	Keys       []string
	NextCursor string
}

// Provider is the Storj StorageProvider implementation. It carries
// an UplinkProject plus the adapter-level config; feature code
// never talks to uplink directly.
type Provider struct {
	cfg     Config
	project UplinkProject
}

// New opens a real storj.io/uplink.Project against cfg and wraps it
// in a Provider. It is the production entry point.
//
// TODO(storj-wiring): actually dial storj.io/uplink here. The
// scaffold returns an error to keep main.go from silently booting
// with a no-op backend until the dependency is added.
func New(cfg Config) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return nil, errors.New("storj: uplink dialer not wired; use NewWithUplink")
}

// NewWithUplink wraps a caller-supplied UplinkProject. Tests use
// this to exercise the adapter against an in-memory fake; operators
// use it to inject a pre-dialed *uplink.Project.
func NewWithUplink(cfg Config, project UplinkProject) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if project == nil {
		return nil, errors.New("storj: uplink project is required")
	}
	return &Provider{cfg: cfg, project: project}, nil
}

func (c Config) validate() error {
	if c.AccessGrant == "" {
		return errors.New("storj: access_grant is required")
	}
	if c.Bucket == "" {
		return errors.New("storj: bucket is required")
	}
	return nil
}

// Close releases the underlying uplink project.
func (p *Provider) Close() error {
	if p.project == nil {
		return nil
	}
	return p.project.Close()
}

// PutPiece streams r into the Storj bucket under pieceID.
func (p *Provider) PutPiece(
	ctx context.Context,
	pieceID string,
	r io.Reader,
	opts providers.PutOptions,
) (providers.PutResult, error) {
	info, err := p.project.UploadObject(ctx, p.cfg.Bucket, pieceID, r, UploadOptions{
		ContentType:  opts.ContentType,
		Metadata:     opts.Metadata,
		StorageClass: opts.StorageClass,
	})
	if err != nil {
		return providers.PutResult{}, fmt.Errorf("storj: upload %s: %w", pieceID, err)
	}
	return providers.PutResult{
		PieceID:   pieceID,
		ETag:      info.ETag,
		SizeBytes: info.SizeBytes,
		Backend:   "storj",
		Locator:   fmt.Sprintf("sj://%s/%s", p.cfg.Bucket, pieceID),
	}, nil
}

// GetPiece returns a reader over pieceID, optionally byte-range
// scoped.
func (p *Provider) GetPiece(
	ctx context.Context,
	pieceID string,
	byteRange *providers.ByteRange,
) (io.ReadCloser, error) {
	rc, err := p.project.DownloadObject(ctx, p.cfg.Bucket, pieceID, byteRange)
	if err != nil {
		return nil, fmt.Errorf("storj: download %s: %w", pieceID, err)
	}
	return rc, nil
}

// HeadPiece returns metadata for pieceID without transferring bytes.
func (p *Provider) HeadPiece(ctx context.Context, pieceID string) (providers.PieceMetadata, error) {
	st, err := p.project.StatObject(ctx, p.cfg.Bucket, pieceID)
	if err != nil {
		return providers.PieceMetadata{}, fmt.Errorf("storj: stat %s: %w", pieceID, err)
	}
	return providers.PieceMetadata{
		PieceID:      pieceID,
		SizeBytes:    st.SizeBytes,
		ETag:         st.ETag,
		ContentType:  st.ContentType,
		StorageClass: st.StorageClass,
		Metadata:     st.Metadata,
	}, nil
}

// DeletePiece removes pieceID from the Storj bucket.
func (p *Provider) DeletePiece(ctx context.Context, pieceID string) error {
	if err := p.project.DeleteObject(ctx, p.cfg.Bucket, pieceID); err != nil {
		return fmt.Errorf("storj: delete %s: %w", pieceID, err)
	}
	return nil
}

// ListPieces walks the bucket under prefix, returning one page at a
// time. Cursor is the Storj-native continuation token; the caller
// stops paging when NextCursor is empty.
func (p *Provider) ListPieces(ctx context.Context, prefix, cursor string) (providers.ListResult, error) {
	page, err := p.project.ListObjects(ctx, p.cfg.Bucket, prefix, cursor)
	if err != nil {
		return providers.ListResult{}, fmt.Errorf("storj: list %q: %w", prefix, err)
	}
	out := providers.ListResult{NextCursor: page.NextCursor}
	for i, st := range page.Objects {
		key := ""
		if i < len(page.Keys) {
			key = page.Keys[i]
		}
		out.Pieces = append(out.Pieces, providers.PieceMetadata{
			PieceID:      key,
			SizeBytes:    st.SizeBytes,
			ETag:         st.ETag,
			ContentType:  st.ContentType,
			StorageClass: st.StorageClass,
			Metadata:     st.Metadata,
		})
	}
	return out, nil
}

// Capabilities reports what Storj supports. Range reads and
// multipart are native; server-side copy is not available through
// uplink and must be implemented as a read-then-write in the
// migration engine.
func (p *Provider) Capabilities() providers.ProviderCapabilities {
	return providers.ProviderCapabilities{
		SupportsRangeReads:     true,
		SupportsMultipart:      true,
		SupportsIfNoneMatch:    false,
		SupportsServerSideCopy: false,
		MaxObjectSizeBytes:     0, // effectively unbounded
		MinStorageDurationDays: 0,
	}
}

// CostModel reports the public Storj price snapshot at the time of
// writing (see https://www.storj.io/pricing): $4/TB-month storage,
// $7/TB egress.
func (p *Provider) CostModel() providers.ProviderCostModel {
	return providers.ProviderCostModel{
		StorageUSDPerTBMonth:  4.0,
		EgressUSDPerGB:        0.007,
		PutRequestUSDPer1000:  0.0,
		GetRequestUSDPer1000:  0.0,
		MinStorageDurationDay: 0,
		FreeEgressRatio:       0.0,
	}
}

// PlacementLabels reports the provider identity. Storj is globally
// distributed so country is "XX" rather than a single ISO code; the
// placement engine treats that as "no sovereignty guarantee".
func (p *Provider) PlacementLabels() providers.PlacementLabels {
	return providers.PlacementLabels{
		Provider:     "storj",
		Region:       "global",
		Country:      "XX",
		StorageClass: "byoc_decentralized",
		FailureZone:  "storj-network",
		Tags: map[string]string{
			"bucket":    p.cfg.Bucket,
			"satellite": p.cfg.SatelliteAddress,
		},
	}
}

// String returns a human-readable description for logs.
func (p *Provider) String() string {
	return fmt.Sprintf("storj(%s)", p.cfg.Bucket)
}

var _ providers.StorageProvider = (*Provider)(nil)
