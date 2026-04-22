// Package backblaze_b2 is the StorageProvider adapter for Backblaze
// B2 (S3-compatible endpoint). It is a B2C-alternative to Wasabi: $6 /
// TB-month storage, low egress via the Bandwidth Alliance.
//
// The adapter is currently a scaffold. It embeds s3_generic.Provider
// for the API surface and overrides descriptive methods; wiring is
// validated by the shared conformance suite before this provider is
// considered production-ready.
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
	// e.g. "us-west-002".
	Region string
	// Bucket is the backing bucket.
	Bucket string
	// AccessKey / SecretKey are B2 application key credentials.
	AccessKey string
	SecretKey string
}

// Provider implements providers.StorageProvider on top of Backblaze
// B2's S3-compatible endpoint.
type Provider struct {
	*s3_generic.Provider
	cfg Config
}

// New builds a Provider. B2 accepts virtual-hosted style for most
// regions; set UsePathStyle via the underlying s3_generic config if
// your account needs it.
func New(cfg Config) (*Provider, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("backblaze_b2: Endpoint is required")
	}
	if cfg.Region == "" {
		return nil, errors.New("backblaze_b2: Region is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("backblaze_b2: Bucket is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("backblaze_b2: AccessKey and SecretKey are required")
	}
	base, err := s3_generic.New(s3_generic.Config{
		Name:      "backblaze_b2",
		Endpoint:  cfg.Endpoint,
		Region:    cfg.Region,
		Bucket:    cfg.Bucket,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
	})
	if err != nil {
		return nil, fmt.Errorf("backblaze_b2: build s3_generic base: %w", err)
	}
	return &Provider{Provider: base, cfg: cfg}, nil
}

// Capabilities reports what B2 supports. B2 does not charge a
// minimum storage duration (unlike Wasabi's 90 days).
func (p *Provider) Capabilities() providers.ProviderCapabilities {
	caps := p.Provider.Capabilities()
	caps.MinStorageDurationDays = 0
	return caps
}

// CostModel is the public Backblaze B2 pricing snapshot. Egress is
// billed above the 3x-stored-bytes free tier; inside the free tier
// and via the Bandwidth Alliance (e.g. to Cloudflare) egress is
// effectively zero.
func (p *Provider) CostModel() providers.ProviderCostModel {
	return providers.ProviderCostModel{
		StorageUSDPerTBMonth:  6.0,
		EgressUSDPerGB:        0.01,
		PutRequestUSDPer1000:  0.0,  // Class A transactions are free
		GetRequestUSDPer1000:  0.004, // Class B transactions
		MinStorageDurationDay: 0,
		FreeEgressRatio:       3.0,
	}
}

// PlacementLabels tags this adapter. B2's regions are US/EU; the
// country defaults to the US unless the endpoint encodes an EU
// region.
func (p *Provider) PlacementLabels() providers.PlacementLabels {
	country := "US"
	if startsWith(p.cfg.Region, "eu-") {
		country = "NL" // Backblaze EU is Amsterdam as of writing
	}
	return providers.PlacementLabels{
		Provider: "backblaze_b2",
		Region:   p.cfg.Region,
		Country:  country,
		Tags: map[string]string{
			"class": "b2c_alternate",
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
