// Package wasabi is a stub of the Phase 1 primary storage backend.
//
// Wasabi is S3-compatible, so the real adapter will wrap the AWS SDK
// for Go v2 S3 client pointed at a Wasabi endpoint. This file defines
// the configuration surface, the constructor, and method stubs with
// TODO markers; real implementation lands in Phase 2 per
// docs/PROGRESS.md.
package wasabi

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// Config is the Wasabi-specific runtime configuration.
//
// TODO(phase-2): decide on the final canonical Wasabi endpoint/region
// naming (e.g. "s3.ap-southeast-1.wasabisys.com" vs per-region host).
type Config struct {
	// Endpoint is the Wasabi S3 endpoint URL, e.g.
	// "https://s3.ap-southeast-1.wasabisys.com".
	Endpoint string
	// Region is the Wasabi region label used when signing requests,
	// e.g. "ap-southeast-1".
	Region string
	// Bucket is the Wasabi bucket name used by this adapter instance.
	// The fabric uses one Wasabi bucket per (region, cell) pair.
	Bucket string
	// AccessKey / SecretKey are the Wasabi service credentials. They
	// are never logged. Prefer loading them via the secrets manager
	// defined in internal/config.
	AccessKey string
	SecretKey string
}

// Provider is the Wasabi StorageProvider implementation.
//
// TODO(phase-2): wire in github.com/aws/aws-sdk-go-v2/service/s3 with
// a custom endpoint resolver pointing at Config.Endpoint.
type Provider struct {
	cfg Config
	// s3 *s3.Client // TODO(phase-2): populate in New
}

// New returns a Provider configured for Wasabi. The current Phase 1
// stub validates the config but does not open a network connection.
func New(cfg Config) (*Provider, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("wasabi: endpoint is required")
	}
	if cfg.Region == "" {
		return nil, errors.New("wasabi: region is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("wasabi: bucket is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("wasabi: access_key and secret_key are required")
	}
	return &Provider{cfg: cfg}, nil
}

var errNotImplemented = errors.New("wasabi: not implemented in Phase 1 stub")

// PutPiece is a stub.
//
// TODO(phase-2): call s3.PutObject against Config.Bucket with a
// Wasabi-compatible storage class and the ciphertext reader. Honour
// opts.IfNoneMatch via the If-None-Match header, which Wasabi
// supports.
func (p *Provider) PutPiece(_ context.Context, _ string, _ io.Reader, _ providers.PutOptions) (providers.PutResult, error) {
	return providers.PutResult{}, errNotImplemented
}

// GetPiece is a stub.
//
// TODO(phase-2): call s3.GetObject with an optional Range header
// derived from byteRange.
func (p *Provider) GetPiece(_ context.Context, _ string, _ *providers.ByteRange) (io.ReadCloser, error) {
	return nil, errNotImplemented
}

// HeadPiece is a stub.
//
// TODO(phase-2): call s3.HeadObject and project into PieceMetadata.
func (p *Provider) HeadPiece(_ context.Context, _ string) (providers.PieceMetadata, error) {
	return providers.PieceMetadata{}, errNotImplemented
}

// DeletePiece is a stub.
//
// TODO(phase-2): call s3.DeleteObject. Respect Wasabi's 90-day
// minimum storage duration; emit a billing warning if pieces are
// deleted before that window.
func (p *Provider) DeletePiece(_ context.Context, _ string) error {
	return errNotImplemented
}

// ListPieces is a stub.
//
// TODO(phase-2): call s3.ListObjectsV2 with ContinuationToken=cursor
// and translate responses to providers.ListResult.
func (p *Provider) ListPieces(_ context.Context, _, _ string) (providers.ListResult, error) {
	return providers.ListResult{}, errNotImplemented
}

// Capabilities reports what Wasabi supports. Values match the
// S3-compatible subset Wasabi documents.
func (p *Provider) Capabilities() providers.ProviderCapabilities {
	return providers.ProviderCapabilities{
		SupportsRangeReads:     true,
		SupportsMultipart:      true,
		SupportsIfNoneMatch:    true,
		SupportsServerSideCopy: true,
		MaxObjectSizeBytes:     5 * 1024 * 1024 * 1024 * 1024, // 5 TiB
		MinStorageDurationDays: 90,
	}
}

// CostModel is the public Wasabi pricing snapshot at the time of
// writing. Per docs/PROPOSAL.md §2.1 the storage price is ~$6.99 /
// TB-month with a fair-use egress policy (<=1× stored).
func (p *Provider) CostModel() providers.ProviderCostModel {
	return providers.ProviderCostModel{
		StorageUSDPerTBMonth:  6.99,
		EgressUSDPerGB:        0.0,
		PutRequestUSDPer1000:  0.0,
		GetRequestUSDPer1000:  0.0,
		MinStorageDurationDay: 90,
		FreeEgressRatio:       1.0,
	}
}

// PlacementLabels reports the provider identity. Region, country, and
// failure-zone are populated from Config.
func (p *Provider) PlacementLabels() providers.PlacementLabels {
	return providers.PlacementLabels{
		Provider:     "wasabi",
		Region:       p.cfg.Region,
		Country:      regionToCountry(p.cfg.Region),
		StorageClass: "standard",
		FailureZone:  p.cfg.Region,
		Tags: map[string]string{
			"endpoint": p.cfg.Endpoint,
			"bucket":   p.cfg.Bucket,
		},
	}
}

// regionToCountry is a Phase 1 approximation. A real mapping lives in
// the control-plane's provider inventory.
//
// TODO(phase-2): replace with a lookup against the control-plane
// inventory, which records the authoritative region → country mapping.
func regionToCountry(region string) string {
	switch {
	case region == "":
		return ""
	case startsWith(region, "ap-southeast-1"):
		return "SG"
	case startsWith(region, "eu-central-"):
		return "DE"
	case startsWith(region, "us-east-"), startsWith(region, "us-west-"):
		return "US"
	default:
		return ""
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// String returns a human-readable description for logs.
func (p *Provider) String() string {
	return fmt.Sprintf("wasabi(%s/%s)", p.cfg.Region, p.cfg.Bucket)
}

var _ providers.StorageProvider = (*Provider)(nil)
