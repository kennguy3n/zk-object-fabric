// Package aws_s3 is the StorageProvider adapter for Amazon S3. In
// the ZK fabric, AWS S3 is a BYOC / disaster-recovery target — not a
// B2C primary — because its egress pricing undermines the fair-use
// story.
//
// The adapter is currently a scaffold. It embeds s3_generic.Provider
// and overrides descriptive methods; live conformance is gated
// behind an env var so CI does not need AWS credentials.
//
// Reference: https://docs.aws.amazon.com/AmazonS3/latest/API/Welcome.html
package aws_s3

import (
	"errors"
	"fmt"

	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/s3_generic"
)

// Config is the AWS S3 adapter wiring.
type Config struct {
	// Region is the AWS region label, e.g. "ap-southeast-1".
	Region string
	// Bucket is the backing bucket.
	Bucket string
	// AccessKey / SecretKey are static IAM credentials. For
	// production BYOC deployments operators should rotate to IRSA
	// / IAM roles for service accounts and use this adapter only
	// for bootstrap.
	AccessKey string
	SecretKey string
	// Endpoint overrides the default AWS S3 endpoint. Leave empty
	// to use the region's default endpoint.
	Endpoint string
}

// Provider implements providers.StorageProvider on top of Amazon S3.
type Provider struct {
	*s3_generic.Provider
	cfg Config
}

// New builds a Provider.
func New(cfg Config) (*Provider, error) {
	if cfg.Region == "" {
		return nil, errors.New("aws_s3: Region is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("aws_s3: Bucket is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("aws_s3: AccessKey and SecretKey are required")
	}
	base, err := s3_generic.New(s3_generic.Config{
		Name:      "aws_s3",
		Endpoint:  cfg.Endpoint,
		Region:    cfg.Region,
		Bucket:    cfg.Bucket,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
	})
	if err != nil {
		return nil, fmt.Errorf("aws_s3: build s3_generic base: %w", err)
	}
	return &Provider{Provider: base, cfg: cfg}, nil
}

// Capabilities reports what S3 supports. S3 Standard has no minimum
// storage duration; Glacier tiers do, but those are surfaced via the
// storage_class dimension rather than a provider-level flag.
func (p *Provider) Capabilities() providers.ProviderCapabilities {
	return p.Provider.Capabilities()
}

// CostModel is the public AWS S3 Standard pricing snapshot. Egress
// is expensive ($0.09/GB after the 100 GB/month free tier for most
// regions) which is why AWS S3 is reserved for BYOC / DR in the
// fabric.
func (p *Provider) CostModel() providers.ProviderCostModel {
	return providers.ProviderCostModel{
		StorageUSDPerTBMonth:  23.0,
		EgressUSDPerGB:        0.09,
		PutRequestUSDPer1000:  5.0,
		GetRequestUSDPer1000:  0.4,
		MinStorageDurationDay: 0,
		FreeEgressRatio:       0.0,
	}
}

// PlacementLabels tags this adapter. Country is derived from the
// region prefix (e.g. "ap-southeast-1" -> "SG").
func (p *Provider) PlacementLabels() providers.PlacementLabels {
	return providers.PlacementLabels{
		Provider: "aws_s3",
		Region:   p.cfg.Region,
		Country:  regionToCountry(p.cfg.Region),
		Tags: map[string]string{
			"class": "byoc",
		},
	}
}

// String returns a human-readable description for logs.
func (p *Provider) String() string {
	return fmt.Sprintf("aws_s3(%s/%s)", p.cfg.Region, p.cfg.Bucket)
}

// regionToCountry maps a subset of AWS region codes to ISO-3166
// alpha-2 country codes. Unknown regions return "XX".
func regionToCountry(region string) string {
	switch {
	case startsWith(region, "ap-southeast-1"):
		return "SG"
	case startsWith(region, "ap-southeast-2"):
		return "AU"
	case startsWith(region, "ap-northeast-1"):
		return "JP"
	case startsWith(region, "ap-south-1"):
		return "IN"
	case startsWith(region, "eu-west-"):
		return "IE"
	case startsWith(region, "eu-central-"):
		return "DE"
	case startsWith(region, "us-east-"), startsWith(region, "us-west-"):
		return "US"
	}
	return "XX"
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

var _ providers.StorageProvider = (*Provider)(nil)
