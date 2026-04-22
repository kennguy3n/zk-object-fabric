// Package s3_generic is a stub of the generic S3 adapter base.
//
// Several adapters (AWS S3, Wasabi, Backblaze B2, Cloudflare R2) speak
// the S3 API with provider-specific quirks (endpoint resolution,
// storage classes, storage duration minimums). Phase 2 will refactor
// the Wasabi adapter on top of this shared base; for Phase 1 the
// package carries only the configuration and method stubs so downstream
// code can begin importing the path.
package s3_generic

import (
	"context"
	"errors"
	"io"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// Config captures the fields every S3-compatible adapter needs.
type Config struct {
	Name      string
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// UsePathStyle addressing is required for some S3-compatible
	// providers (e.g. MinIO). Default is virtual-hosted style.
	UsePathStyle bool
}

// Provider is the shared S3-compatible adapter base.
//
// TODO(phase-2): pull in github.com/aws/aws-sdk-go-v2/service/s3 and
// make this the canonical implementation that other adapters embed.
type Provider struct {
	cfg Config
}

// New returns a Provider configured for any S3-compatible backend.
func New(cfg Config) (*Provider, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("s3_generic: endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3_generic: bucket is required")
	}
	return &Provider{cfg: cfg}, nil
}

var errNotImplemented = errors.New("s3_generic: not implemented in Phase 1 stub")

func (p *Provider) PutPiece(_ context.Context, _ string, _ io.Reader, _ providers.PutOptions) (providers.PutResult, error) {
	return providers.PutResult{}, errNotImplemented
}

func (p *Provider) GetPiece(_ context.Context, _ string, _ *providers.ByteRange) (io.ReadCloser, error) {
	return nil, errNotImplemented
}

func (p *Provider) HeadPiece(_ context.Context, _ string) (providers.PieceMetadata, error) {
	return providers.PieceMetadata{}, errNotImplemented
}

func (p *Provider) DeletePiece(_ context.Context, _ string) error {
	return errNotImplemented
}

func (p *Provider) ListPieces(_ context.Context, _, _ string) (providers.ListResult, error) {
	return providers.ListResult{}, errNotImplemented
}

func (p *Provider) Capabilities() providers.ProviderCapabilities {
	return providers.ProviderCapabilities{
		SupportsRangeReads:     true,
		SupportsMultipart:      true,
		SupportsIfNoneMatch:    true,
		SupportsServerSideCopy: true,
		MaxObjectSizeBytes:     5 * 1024 * 1024 * 1024 * 1024, // 5 TiB
	}
}

func (p *Provider) CostModel() providers.ProviderCostModel {
	return providers.ProviderCostModel{}
}

func (p *Provider) PlacementLabels() providers.PlacementLabels {
	return providers.PlacementLabels{
		Provider: p.cfg.Name,
		Region:   p.cfg.Region,
	}
}

var _ providers.StorageProvider = (*Provider)(nil)
