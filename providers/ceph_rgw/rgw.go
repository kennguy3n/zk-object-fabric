// Package ceph_rgw is the StorageProvider adapter for Ceph RADOS
// Gateway. It is the recommended Phase 2+ local-DC storage base for
// B2B dedicated cells and sovereign placement.
//
// Ceph RGW is an S3-compatible surface so the adapter embeds
// s3_generic.Provider and overrides only the descriptive methods
// (Capabilities, CostModel, PlacementLabels). Reference:
// https://docs.ceph.com/en/latest/radosgw/.
package ceph_rgw

import (
	"errors"
	"fmt"

	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/s3_generic"
)

// Config is the Ceph RGW adapter wiring.
//
// A Ceph RGW deployment is typically reachable at a stable HTTPS
// endpoint (e.g. https://rgw.dc-sg-1.internal). Region and Bucket
// semantics mirror S3; Zone/ZoneGroup names are currently carried in
// the Region field so the single Config shape works for both
// single-zone and multi-site deployments.
type Config struct {
	// Endpoint is the full RGW base URL, including scheme.
	Endpoint string

	// Region is the Ceph zonegroup label, used as S3 Region.
	Region string

	// Bucket is the backing bucket.
	Bucket string

	// AccessKey / SecretKey are Ceph RGW user credentials.
	AccessKey string
	SecretKey string

	// Cell is the operator-assigned cell identifier
	// (e.g. "ceph-sg-1"). Surfaced via PlacementLabels.Tags so the
	// placement engine can pin workloads to specific cells.
	Cell string

	// Country is the ISO-3166 alpha-2 code of the hosting facility.
	// Ceph RGW is deployed by the operator, so there is no
	// region-to-country map to consult.
	Country string
}

// Provider implements providers.StorageProvider on top of Ceph RGW.
type Provider struct {
	*s3_generic.Provider
	cfg Config
}

// New builds a Provider that connects to the configured RGW endpoint
// using AWS SDK v2. Path-style addressing is required for RGW in most
// deployments, so this constructor turns it on by default.
func New(cfg Config) (*Provider, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("ceph_rgw: Endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("ceph_rgw: Bucket is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("ceph_rgw: AccessKey and SecretKey are required")
	}
	region := cfg.Region
	if region == "" {
		// RGW often runs with the literal zonegroup "default" when
		// multi-site is not configured. The S3 signer still requires
		// a non-empty region.
		region = "default"
	}
	base, err := s3_generic.New(s3_generic.Config{
		Name:         "ceph_rgw",
		Endpoint:     cfg.Endpoint,
		Region:       region,
		Bucket:       cfg.Bucket,
		AccessKey:    cfg.AccessKey,
		SecretKey:    cfg.SecretKey,
		UsePathStyle: true,
	})
	if err != nil {
		return nil, fmt.Errorf("ceph_rgw: build s3_generic base: %w", err)
	}
	return &Provider{Provider: base, cfg: cfg}, nil
}

// Capabilities reports what Ceph RGW supports. Ceph is operator-run
// so there is no provider-imposed minimum storage duration.
func (p *Provider) Capabilities() providers.ProviderCapabilities {
	caps := p.Provider.Capabilities()
	caps.MinStorageDurationDays = 0
	return caps
}

// CostModel returns a conservative local-DC economics snapshot. The
// numbers reflect a self-hosted Ceph cluster on commodity hardware
// (no per-GB egress, low request cost, higher storage COGS than
// public-cloud hyperscalers).
func (p *Provider) CostModel() providers.ProviderCostModel {
	return providers.ProviderCostModel{
		// Placeholder: local-DC $/TB-month is operator-specific.
		// The figure below is an ops-reviewed estimate for Phase 2
		// capacity planning; operators must override per cell.
		StorageUSDPerTBMonth:  10.0,
		EgressUSDPerGB:        0.0,
		PutRequestUSDPer1000:  0.0,
		GetRequestUSDPer1000:  0.0,
		MinStorageDurationDay: 0,
		FreeEgressRatio:       0.0,
	}
}

// PlacementLabels tags this provider as Ceph RGW and pins the country
// / cell from Config so the placement engine can match sovereign
// policies.
func (p *Provider) PlacementLabels() providers.PlacementLabels {
	labels := providers.PlacementLabels{
		Provider: "ceph_rgw",
		Region:   p.cfg.Region,
		Country:  p.cfg.Country,
		Tags: map[string]string{
			"class": "local_dc",
		},
	}
	if p.cfg.Cell != "" {
		labels.Tags["cell"] = p.cfg.Cell
	}
	return labels
}

// String returns a human-readable description for logs.
func (p *Provider) String() string {
	return fmt.Sprintf("ceph_rgw(%s/%s)", p.cfg.Region, p.cfg.Bucket)
}

var _ providers.StorageProvider = (*Provider)(nil)
