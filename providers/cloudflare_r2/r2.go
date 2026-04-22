// Package cloudflare_r2 is the StorageProvider adapter for Cloudflare
// R2. R2 is the recommended B2C hot-egress backend: zero egress
// charge, strong global footprint, S3-compatible API.
//
// The adapter is currently a scaffold. It embeds s3_generic.Provider
// and overrides descriptive methods; conformance tests are gated
// behind an env var so CI does not need R2 credentials.
//
// Reference: https://developers.cloudflare.com/r2/api/s3/api/
package cloudflare_r2

import (
	"errors"
	"fmt"

	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/s3_generic"
)

// Config is the R2 adapter wiring.
type Config struct {
	// AccountID is the Cloudflare account ID. It is the host prefix
	// of the S3-compatible endpoint (https://<AccountID>.r2.cloudflarestorage.com).
	AccountID string
	// Endpoint overrides the derived endpoint. Leave empty to build
	// it from AccountID.
	Endpoint string
	// Bucket is the backing bucket.
	Bucket string
	// AccessKey / SecretKey are R2 API token credentials.
	AccessKey string
	SecretKey string
}

// Provider implements providers.StorageProvider on top of Cloudflare
// R2.
type Provider struct {
	*s3_generic.Provider
	cfg Config
}

// New builds a Provider pointing at the derived R2 endpoint. R2's
// region is fixed to "auto".
func New(cfg Config) (*Provider, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("cloudflare_r2: Bucket is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("cloudflare_r2: AccessKey and SecretKey are required")
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		if cfg.AccountID == "" {
			return nil, errors.New("cloudflare_r2: AccountID or Endpoint is required")
		}
		endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.AccountID)
	}
	base, err := s3_generic.New(s3_generic.Config{
		Name:      "cloudflare_r2",
		Endpoint:  endpoint,
		Region:    "auto",
		Bucket:    cfg.Bucket,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
	})
	if err != nil {
		return nil, fmt.Errorf("cloudflare_r2: build s3_generic base: %w", err)
	}
	return &Provider{Provider: base, cfg: cfg}, nil
}

// Capabilities reports what R2 supports. R2 has no minimum storage
// duration and no egress fees.
func (p *Provider) Capabilities() providers.ProviderCapabilities {
	caps := p.Provider.Capabilities()
	caps.MinStorageDurationDays = 0
	return caps
}

// CostModel is the public R2 pricing snapshot. Zero egress is the
// headline; request costs apply.
func (p *Provider) CostModel() providers.ProviderCostModel {
	return providers.ProviderCostModel{
		StorageUSDPerTBMonth:  15.0,
		EgressUSDPerGB:        0.0,
		PutRequestUSDPer1000:  4.5, // Class A
		GetRequestUSDPer1000:  0.36, // Class B
		MinStorageDurationDay: 0,
		FreeEgressRatio:       9999.0, // effectively unlimited
	}
}

// PlacementLabels tags this adapter. R2 is globally distributed; the
// Country label is "XX" (worldwide) to match the placement engine's
// convention for non-sovereign backends.
func (p *Provider) PlacementLabels() providers.PlacementLabels {
	return providers.PlacementLabels{
		Provider: "cloudflare_r2",
		Region:   "auto",
		Country:  "XX",
		Tags: map[string]string{
			"class": "b2c_hot_egress",
		},
	}
}

// String returns a human-readable description for logs.
func (p *Provider) String() string {
	return fmt.Sprintf("cloudflare_r2(%s)", p.cfg.Bucket)
}

var _ providers.StorageProvider = (*Provider)(nil)
