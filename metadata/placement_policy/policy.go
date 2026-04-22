// Package placement_policy defines the Phase 1 placement policy DSL
// described in docs/PROPOSAL.md §3.9.
//
// Phase 1 exposes only four knobs: provider, region, country, and
// storage_class, plus an optional cache_location hint. DC / rack /
// node placement is deliberately not exposed until Phase 2+.
package placement_policy

import (
	"fmt"
	"strings"
)

// Policy is the deserialized form of a tenant or bucket placement
// policy. It is designed to round-trip through YAML (yaml.v3 tags) and
// JSON.
type Policy struct {
	Tenant string       `json:"tenant" yaml:"tenant"`
	Bucket string       `json:"bucket,omitempty" yaml:"bucket,omitempty"`
	Spec   PolicySpec   `json:"policy" yaml:"policy"`
}

// PolicySpec is the body of a policy.
type PolicySpec struct {
	Encryption EncryptionSpec `json:"encryption" yaml:"encryption"`
	Placement  PlacementSpec  `json:"placement" yaml:"placement"`
}

// EncryptionSpec names the encryption mode and the KMS reference.
//
// Mode values mirror metadata.EncryptionConfig.Mode:
// "client_side" / "managed" / "public_distribution".
type EncryptionSpec struct {
	Mode string `json:"mode" yaml:"mode"`
	KMS  string `json:"kms,omitempty" yaml:"kms,omitempty"`
}

// PlacementSpec captures the Phase 1 knobs.
type PlacementSpec struct {
	// Provider is the set of allowed storage providers, e.g.
	// ["wasabi", "local-cell-1"]. At least one must be set.
	Provider []string `json:"provider" yaml:"provider"`
	// Region is the set of allowed cloud regions, e.g.
	// ["ap-southeast-1"]. Optional.
	Region []string `json:"region,omitempty" yaml:"region,omitempty"`
	// Country is the set of allowed ISO-3166 alpha-2 country codes,
	// e.g. ["SG", "DE"]. Optional but strongly recommended for
	// sovereignty-sensitive tenants.
	Country []string `json:"country,omitempty" yaml:"country,omitempty"`
	// StorageClass is the per-provider tier hint, e.g.
	// ["standard", "archive"].
	StorageClass []string `json:"storage_class,omitempty" yaml:"storage_class,omitempty"`
	// CacheLocation is an optional hint for the L0/L1 cache tier
	// (e.g. "linode-sg"). It is advisory; the placement engine may
	// override it for fair-use or cost reasons.
	CacheLocation string `json:"cache_location,omitempty" yaml:"cache_location,omitempty"`
}

// Validate performs structural and cross-field checks on the policy.
// It does not call out to external systems.
func (p *Policy) Validate() error {
	if p.Tenant == "" {
		return fmt.Errorf("placement_policy: tenant is required")
	}
	if err := p.Spec.Encryption.validate(); err != nil {
		return err
	}
	if err := p.Spec.Placement.validate(); err != nil {
		return err
	}
	return nil
}

func (e EncryptionSpec) validate() error {
	switch e.Mode {
	case "client_side", "managed", "public_distribution":
	case "":
		return fmt.Errorf("placement_policy: encryption.mode is required")
	default:
		return fmt.Errorf("placement_policy: unknown encryption.mode %q", e.Mode)
	}
	return nil
}

func (p *PlacementSpec) validate() error {
	if len(p.Provider) == 0 {
		return fmt.Errorf("placement_policy: placement.provider must list at least one provider")
	}
	for i := range p.Country {
		p.Country[i] = strings.TrimSpace(p.Country[i])
		if len(p.Country[i]) != 2 {
			return fmt.Errorf("placement_policy: placement.country[%d]=%q is not an ISO-3166 alpha-2 code", i, p.Country[i])
		}
	}
	return nil
}
