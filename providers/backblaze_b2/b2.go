// Package backblaze_b2 is the StorageProvider adapter for Backblaze
// B2 via its S3-compatible endpoint.
//
// B2 is the designated B2C-alternative to Wasabi: $6 / TB-month
// storage, cheap egress (free inside the 3x-stored-bytes tier, free
// via the Bandwidth Alliance to Cloudflare). The adapter embeds
// *s3_generic.Provider so the PUT/GET/HEAD/DELETE/LIST surface is
// inherited wholesale from the shared implementation; this file
// carries the B2-specific identity, capabilities, cost model, and
// placement labels.
//
// Reference: https://www.backblaze.com/b2/docs/s3_compatible_api.html
package backblaze_b2

import (
	"errors"
	"fmt"

	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/s3_generic"
)

// Config is the B2 adapter wiring.
type Config struct {
	// Endpoint is the B2 S3-compatible endpoint, e.g.
	// "https://s3.us-west-002.backblazeb2.com". Required.
	Endpoint string
	// Region is the B2 region label embedded in the endpoint,
	// e.g. "us-west-002". Required for SigV4 signing.
	Region string
	// Bucket is the backing bucket.
	Bucket string
	// AccessKey / SecretKey are B2 application key credentials.
	// Operators should scope the key to a single bucket and only
	// the operations the fabric actually issues (listAllBucketNames
	// is NOT needed).
	AccessKey string
	SecretKey string
	// UsePathStyle forces path-style addressing. B2 accepts both
	// path-style and virtual-hosted-style; the default of false
	// (virtual-hosted) matches Backblaze's current recommendation
	// but operators with wildcard-cert issues can flip this.
	UsePathStyle bool
}

// Provider implements providers.StorageProvider on top of Backblaze
// B2's S3-compatible endpoint.
type Provider struct {
	*s3_generic.Provider
	cfg Config
}

// New builds a Provider.
func New(cfg Config) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	base, err := s3_generic.New(s3_generic.Config{
		Name:         "backblaze_b2",
		Endpoint:     cfg.Endpoint,
		Region:       cfg.Region,
		Bucket:       cfg.Bucket,
		AccessKey:    cfg.AccessKey,
		SecretKey:    cfg.SecretKey,
		UsePathStyle: cfg.UsePathStyle,
	})
	if err != nil {
		return nil, fmt.Errorf("backblaze_b2: build s3_generic base: %w", err)
	}
	return &Provider{Provider: base, cfg: cfg}, nil
}

// NewWithClient returns a Provider wired to a caller-supplied S3API.
// Unit tests use this to drive the adapter against an in-memory
// fake.
func NewWithClient(cfg Config, client s3_generic.S3API) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	base, err := s3_generic.NewWithClient(s3_generic.Config{
		Name:         "backblaze_b2",
		Endpoint:     cfg.Endpoint,
		Region:       cfg.Region,
		Bucket:       cfg.Bucket,
		AccessKey:    cfg.AccessKey,
		SecretKey:    cfg.SecretKey,
		UsePathStyle: cfg.UsePathStyle,
	}, client)
	if err != nil {
		return nil, fmt.Errorf("backblaze_b2: build s3_generic base: %w", err)
	}
	return &Provider{Provider: base, cfg: cfg}, nil
}

func (c Config) validate() error {
	if c.Endpoint == "" {
		return errors.New("backblaze_b2: Endpoint is required")
	}
	if c.Region == "" {
		return errors.New("backblaze_b2: Region is required")
	}
	if c.Bucket == "" {
		return errors.New("backblaze_b2: Bucket is required")
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return errors.New("backblaze_b2: AccessKey and SecretKey are required")
	}
	return nil
}

// Capabilities reports what B2 supports. B2 does not charge a
// minimum storage duration (unlike Wasabi's 90 days) and supports
// the full S3 compat envelope covered by s3_generic.
func (p *Provider) Capabilities() providers.ProviderCapabilities {
	caps := p.Provider.Capabilities()
	caps.MinStorageDurationDays = 0
	return caps
}

// CostModel is the public Backblaze B2 pricing snapshot. Egress is
// billed above the 3x-stored-bytes free tier; inside the free tier
// and via the Bandwidth Alliance (e.g. to Cloudflare) egress is
// effectively zero. Class A (write) transactions are free; Class B
// (read) transactions are $0.004 / 10k requests.
func (p *Provider) CostModel() providers.ProviderCostModel {
	return providers.ProviderCostModel{
		StorageUSDPerTBMonth:  6.0,
		EgressUSDPerGB:        0.01,
		PutRequestUSDPer1000:  0.0,
		GetRequestUSDPer1000:  0.004,
		MinStorageDurationDay: 0,
		FreeEgressRatio:       3.0,
	}
}

// PlacementLabels tags this adapter. B2's regions are US (us-west,
// us-east) and EU (eu-central). Country defaults to US unless the
// region label encodes an EU location.
func (p *Provider) PlacementLabels() providers.PlacementLabels {
	country := "US"
	if startsWith(p.cfg.Region, "eu-") {
		country = "NL"
	}
	return providers.PlacementLabels{
		Provider:    "backblaze_b2",
		Region:      p.cfg.Region,
		Country:     country,
		FailureZone: p.cfg.Region,
		Tags: map[string]string{
			"class":    "b2c_alternate",
			"endpoint": p.cfg.Endpoint,
			"bucket":   p.cfg.Bucket,
		},
	}
}

// String returns a human-readable description for logs.
func (p *Provider) String() string {
	return fmt.Sprintf("backblaze_b2(%s/%s)", p.cfg.Region, p.cfg.Bucket)
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

var _ providers.StorageProvider = (*Provider)(nil)
