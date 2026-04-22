// Package wasabi implements the Phase 2 primary storage backend.
//
// Wasabi is S3-compatible, so this adapter embeds *s3_generic.Provider
// and only overrides the fields that differ for Wasabi: provider
// identity, capability envelope (90-day minimum storage duration),
// cost model (~$6.99 / TB-month with fair-use egress), and placement
// labels.
package wasabi

import (
	"errors"
	"fmt"

	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/s3_generic"
)

// Config is the Wasabi-specific runtime configuration.
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

// Provider is the Wasabi StorageProvider implementation. It embeds
// *s3_generic.Provider for the bulk of the S3 API and overrides the
// Wasabi-specific descriptive methods.
type Provider struct {
	*s3_generic.Provider
	cfg Config
}

// New returns a Provider configured for Wasabi.
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
	base, err := s3_generic.New(s3_generic.Config{
		Name:      "wasabi",
		Endpoint:  cfg.Endpoint,
		Region:    cfg.Region,
		Bucket:    cfg.Bucket,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
	})
	if err != nil {
		return nil, fmt.Errorf("wasabi: %w", err)
	}
	return &Provider{Provider: base, cfg: cfg}, nil
}

// NewWithClient returns a Provider wrapping a caller-supplied S3API.
// Tests use this to exercise the Wasabi adapter against an in-memory
// fake without opening a network connection.
func NewWithClient(cfg Config, client s3_generic.S3API) (*Provider, error) {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Bucket == "" {
		return nil, errors.New("wasabi: bucket is required")
	}
	if cfg.AccessKey == "" {
		cfg.AccessKey = "test"
	}
	if cfg.SecretKey == "" {
		cfg.SecretKey = "test"
	}
	base, err := s3_generic.NewWithClient(s3_generic.Config{
		Name:      "wasabi",
		Endpoint:  cfg.Endpoint,
		Region:    cfg.Region,
		Bucket:    cfg.Bucket,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
	}, client)
	if err != nil {
		return nil, fmt.Errorf("wasabi: %w", err)
	}
	return &Provider{Provider: base, cfg: cfg}, nil
}

// Capabilities reports what Wasabi supports. It widens the generic S3
// envelope with Wasabi's documented 90-day minimum storage duration.
func (p *Provider) Capabilities() providers.ProviderCapabilities {
	caps := p.Provider.Capabilities()
	caps.MinStorageDurationDays = 90
	return caps
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

// PlacementLabels reports the provider identity. Region, country,
// storage class, and failure zone are populated from Config.
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

// regionToCountry is a Phase 1/2 approximation. A real mapping lives
// in the control-plane's provider inventory.
//
// TODO(phase-3): replace with a lookup against the control-plane
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
