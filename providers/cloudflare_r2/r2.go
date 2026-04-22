// Package cloudflare_r2 is the StorageProvider adapter for Cloudflare
// R2.
//
// R2 is the recommended B2C hot-egress backend: zero egress charge,
// strong global footprint, S3-compatible API. The adapter embeds
// *s3_generic.Provider so PUT/GET/HEAD/DELETE/LIST are delegated to
// the shared implementation; this file carries the R2-specific
// identity, capabilities, cost model, and placement labels plus the
// constructor glue that derives the account-scoped endpoint.
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
	// AccountID is the Cloudflare account ID. It is the host
	// prefix of the S3-compatible endpoint
	// (https://<AccountID>.r2.cloudflarestorage.com). Either
	// AccountID or Endpoint must be set.
	AccountID string
	// Endpoint overrides the derived endpoint. Leave empty to
	// build it from AccountID.
	Endpoint string
	// Bucket is the backing bucket.
	Bucket string
	// AccessKey / SecretKey are R2 API token credentials. The token
	// should be scoped to "Object Read & Write" on the specific
	// bucket; do not use account-wide tokens here.
	AccessKey string
	SecretKey string
	// DisablePathStyle switches to virtual-hosted-style addressing.
	// The adapter defaults to path-style because it avoids the need
	// for per-bucket DNS resolution against R2's globally-anycasted
	// edge; only flip this when operators have explicitly set up the
	// CNAMEs that virtual-hosted-style requires.
	DisablePathStyle bool
}

// Provider implements providers.StorageProvider on top of Cloudflare
// R2.
type Provider struct {
	*s3_generic.Provider
	cfg      Config
	endpoint string
}

// New builds a Provider pointing at the derived R2 endpoint. R2's
// region is fixed to "auto".
func New(cfg Config) (*Provider, error) {
	endpoint, err := cfg.resolveEndpoint()
	if err != nil {
		return nil, err
	}
	if err := cfg.validateCreds(); err != nil {
		return nil, err
	}
	base, err := s3_generic.New(s3_generic.Config{
		Name:         "cloudflare_r2",
		Endpoint:     endpoint,
		Region:       "auto",
		Bucket:       cfg.Bucket,
		AccessKey:    cfg.AccessKey,
		SecretKey:    cfg.SecretKey,
		UsePathStyle: cfg.effectivePathStyle(),
	})
	if err != nil {
		return nil, fmt.Errorf("cloudflare_r2: build s3_generic base: %w", err)
	}
	return &Provider{Provider: base, cfg: cfg, endpoint: endpoint}, nil
}

// NewWithClient returns a Provider wired to a caller-supplied S3API.
// Unit tests use this to exercise the adapter without a live R2
// endpoint.
func NewWithClient(cfg Config, client s3_generic.S3API) (*Provider, error) {
	endpoint, err := cfg.resolveEndpoint()
	if err != nil {
		return nil, err
	}
	if err := cfg.validateCreds(); err != nil {
		return nil, err
	}
	base, err := s3_generic.NewWithClient(s3_generic.Config{
		Name:         "cloudflare_r2",
		Endpoint:     endpoint,
		Region:       "auto",
		Bucket:       cfg.Bucket,
		AccessKey:    cfg.AccessKey,
		SecretKey:    cfg.SecretKey,
		UsePathStyle: cfg.effectivePathStyle(),
	}, client)
	if err != nil {
		return nil, fmt.Errorf("cloudflare_r2: build s3_generic base: %w", err)
	}
	return &Provider{Provider: base, cfg: cfg, endpoint: endpoint}, nil
}

func (c Config) resolveEndpoint() (string, error) {
	if c.Bucket == "" {
		return "", errors.New("cloudflare_r2: Bucket is required")
	}
	if c.Endpoint != "" {
		return c.Endpoint, nil
	}
	if c.AccountID == "" {
		return "", errors.New("cloudflare_r2: AccountID or Endpoint is required")
	}
	return fmt.Sprintf("https://%s.r2.cloudflarestorage.com", c.AccountID), nil
}

func (c Config) validateCreds() error {
	if c.AccessKey == "" || c.SecretKey == "" {
		return errors.New("cloudflare_r2: AccessKey and SecretKey are required")
	}
	return nil
}

// effectivePathStyle is path-style unless DisablePathStyle is set.
// Path-style avoids per-bucket DNS resolution on R2's
// globally-anycasted edge.
func (c Config) effectivePathStyle() bool {
	return !c.DisablePathStyle
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
		PutRequestUSDPer1000:  4.5,  // Class A
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
			"class":    "b2c_hot_egress",
			"endpoint": p.endpoint,
			"bucket":   p.cfg.Bucket,
		},
	}
}

// String returns a human-readable description for logs.
func (p *Provider) String() string {
	return fmt.Sprintf("cloudflare_r2(%s)", p.cfg.Bucket)
}

var _ providers.StorageProvider = (*Provider)(nil)
