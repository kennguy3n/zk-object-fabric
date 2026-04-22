// Package aws_s3 is the StorageProvider adapter for Amazon S3.
//
// In the ZK fabric, AWS S3 is a BYOC / disaster-recovery target — not
// a B2C primary — because its egress pricing undermines the fair-use
// story. The adapter embeds *s3_generic.Provider so PutPiece,
// GetPiece, HeadPiece, DeletePiece, and ListPieces are all delegated
// to the shared SigV4 + AWS SDK v2 implementation. This file only
// carries the AWS-specific identity, capabilities, cost model, and
// placement labels, plus a NewWithClient seam so unit tests can
// exercise the adapter without opening a network connection.
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
	// to use the region's default endpoint. Most S3-compatible
	// appliances (MinIO, SeaweedFS) can be driven through this
	// adapter by setting Endpoint.
	Endpoint string
	// UsePathStyle forces path-style addressing. Default is
	// virtual-hosted-style, which matches AWS S3's current
	// recommendation.
	UsePathStyle bool
}

// Provider implements providers.StorageProvider on top of Amazon S3.
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
		Name:         "aws_s3",
		Endpoint:     cfg.Endpoint,
		Region:       cfg.Region,
		Bucket:       cfg.Bucket,
		AccessKey:    cfg.AccessKey,
		SecretKey:    cfg.SecretKey,
		UsePathStyle: cfg.UsePathStyle,
	})
	if err != nil {
		return nil, fmt.Errorf("aws_s3: build s3_generic base: %w", err)
	}
	return &Provider{Provider: base, cfg: cfg}, nil
}

// NewWithClient returns a Provider wired to a caller-supplied S3API.
// Unit tests use this to assert the AWS adapter delegates correctly
// without opening a network connection.
func NewWithClient(cfg Config, client s3_generic.S3API) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	base, err := s3_generic.NewWithClient(s3_generic.Config{
		Name:         "aws_s3",
		Endpoint:     cfg.Endpoint,
		Region:       cfg.Region,
		Bucket:       cfg.Bucket,
		AccessKey:    cfg.AccessKey,
		SecretKey:    cfg.SecretKey,
		UsePathStyle: cfg.UsePathStyle,
	}, client)
	if err != nil {
		return nil, fmt.Errorf("aws_s3: build s3_generic base: %w", err)
	}
	return &Provider{Provider: base, cfg: cfg}, nil
}

func (c Config) validate() error {
	if c.Region == "" {
		return errors.New("aws_s3: Region is required")
	}
	if c.Bucket == "" {
		return errors.New("aws_s3: Bucket is required")
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return errors.New("aws_s3: AccessKey and SecretKey are required")
	}
	return nil
}

// Capabilities reports what S3 supports. S3 Standard has no minimum
// storage duration; Glacier tiers do, but those are surfaced via the
// storage_class dimension rather than a provider-level flag.
func (p *Provider) Capabilities() providers.ProviderCapabilities {
	return p.Provider.Capabilities()
}

// CostModel is the public AWS S3 Standard pricing snapshot. Egress
// is expensive (~$0.09/GB after the 100 GB/month free tier for most
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
//
// Each region is matched exactly rather than by prefix: AWS numbers
// sibling regions in different jurisdictions (e.g. eu-west-1 Dublin
// vs eu-west-2 London vs eu-west-3 Paris) so a prefix match would
// route sovereign workloads to the wrong country.
func regionToCountry(region string) string {
	switch region {
	case "ap-southeast-1":
		return "SG"
	case "ap-southeast-2":
		return "AU"
	case "ap-southeast-3":
		return "ID"
	case "ap-northeast-1":
		return "JP"
	case "ap-northeast-2":
		return "KR"
	case "ap-northeast-3":
		return "JP"
	case "ap-south-1":
		return "IN"
	case "eu-west-1":
		return "IE"
	case "eu-west-2":
		return "GB"
	case "eu-west-3":
		return "FR"
	case "eu-central-1":
		return "DE"
	case "eu-central-2":
		return "CH"
	case "eu-north-1":
		return "SE"
	case "eu-south-1":
		return "IT"
	case "eu-south-2":
		return "ES"
	case "us-east-1", "us-east-2", "us-west-1", "us-west-2":
		return "US"
	case "ca-central-1":
		return "CA"
	case "sa-east-1":
		return "BR"
	}
	return "XX"
}

var _ providers.StorageProvider = (*Provider)(nil)
