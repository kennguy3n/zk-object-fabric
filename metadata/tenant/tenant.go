// Package tenant defines the multi-tenancy model for ZK Object Fabric.
//
// The schema mirrors the conceptual YAML in docs/PROPOSAL.md §5.5.
// Every tenant carries its contract type, license tier, key
// references, default placement policy, per-tenant budgets, abuse
// configuration, and billing metadata.
//
// The struct tags support round-trip through both JSON (control-plane
// APIs) and YAML (operator-authored tenant files) so the same record
// can be loaded from an operator checkout and written to the control-
// plane database without translation.
package tenant

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ContractType names how a tenant consumes the fabric.
type ContractType string

const (
	// ContractB2CPooled is the default self-service tier on pooled
	// infrastructure.
	ContractB2CPooled ContractType = "b2c_pooled"
	// ContractB2BDedicated is a dedicated-cell enterprise contract.
	ContractB2BDedicated ContractType = "b2b_dedicated"
	// ContractSovereign is a sovereign-placement contract with
	// hard-boundary country / DC / rack controls.
	ContractSovereign ContractType = "sovereign"
	// ContractBYOC is bring-your-own-cloud: customer owns the cloud
	// account, ZK Object Fabric ships the gateway and control plane.
	ContractBYOC ContractType = "byoc"
)

// LicenseTier names a purchased product tier. The set mirrors the
// phase-specific product tables in docs/PROPOSAL.md §2.4.
type LicenseTier string

const (
	LicenseBeta      LicenseTier = "beta"
	LicenseArchive   LicenseTier = "archive"
	LicenseStandard  LicenseTier = "standard"
	LicenseHot       LicenseTier = "hot"
	LicenseDedicated LicenseTier = "dedicated"
	LicenseSovereign LicenseTier = "sovereign"
)

// DEKPolicy names a tenant's per-object-key rotation strategy.
type DEKPolicy string

const (
	// DEKPerObject wraps a fresh DEK per object. This is the default
	// and the marketed zero-knowledge mode.
	DEKPerObject DEKPolicy = "per_object"
	// DEKPerBucket wraps one DEK per bucket. Only offered to
	// customers who explicitly opt in; not the default.
	DEKPerBucket DEKPolicy = "per_bucket"
)

// Tenant is the control-plane record for a single tenant. It is the
// canonical Go representation of docs/PROPOSAL.md §5.5.
type Tenant struct {
	ID               string           `json:"id" yaml:"id"`
	Name             string           `json:"name" yaml:"name"`
	ContractType     ContractType     `json:"contract_type" yaml:"contract_type"`
	LicenseTier      LicenseTier      `json:"license_tier" yaml:"license_tier"`
	Keys             Keys             `json:"keys" yaml:"keys"`
	PlacementDefault PlacementDefault `json:"placement_default" yaml:"placement_default"`
	Budgets          Budgets          `json:"budgets" yaml:"budgets"`
	Abuse            AbuseConfig      `json:"abuse" yaml:"abuse"`
	Billing          Billing          `json:"billing" yaml:"billing"`
}

// Keys describes a tenant's encryption key material by reference.
// Plaintext key material never appears in this struct.
type Keys struct {
	// RootKeyRef is an opaque CMK locator, e.g.
	// "cmk://acme/prod/root" or "aws-kms://arn:aws:kms:...".
	RootKeyRef string `json:"root_key_ref" yaml:"root_key_ref"`
	// DEKPolicy names the per-object-key rotation strategy.
	DEKPolicy DEKPolicy `json:"dek_policy" yaml:"dek_policy"`
}

// PlacementDefault names the placement policy applied to new buckets
// and objects unless overridden at bucket creation time.
type PlacementDefault struct {
	// PolicyRef is the handle of a placement_policy.Policy stored in
	// the control plane (e.g. "p_country_strict").
	PolicyRef string `json:"policy_ref" yaml:"policy_ref"`
}

// Budgets bounds a tenant's monthly resource usage.
type Budgets struct {
	// EgressTBMonth is the soft-cap on monthly egress in TB. 0 means
	// "no cap configured"; the control plane then falls back to the
	// global default.
	EgressTBMonth float64 `json:"egress_tb_month" yaml:"egress_tb_month"`
	// RequestsPerSec is the steady-state request rate ceiling
	// applied by the gateway fleet's rate limiter.
	RequestsPerSec int `json:"requests_per_sec" yaml:"requests_per_sec"`
}

// AbuseConfig captures per-tenant abuse and anomaly handling.
type AbuseConfig struct {
	// AnomalyProfile selects a named anomaly-detection profile
	// (e.g. "finance", "media", "default"). Profile details are
	// resolved by the anomaly service, not this package.
	AnomalyProfile string `json:"anomaly_profile" yaml:"anomaly_profile"`
	// CDNShielding: "enabled" or "disabled". Free-form string to
	// allow more modes in the future without schema breakage.
	CDNShielding string `json:"cdn_shielding" yaml:"cdn_shielding"`
}

// Billing holds invoice-grouping and currency configuration.
type Billing struct {
	Currency     string `json:"currency" yaml:"currency"`
	InvoiceGroup string `json:"invoice_group" yaml:"invoice_group"`
}

// Valid reports whether c is one of the defined contract types.
func (c ContractType) Valid() bool {
	switch c {
	case ContractB2CPooled, ContractB2BDedicated, ContractSovereign, ContractBYOC:
		return true
	default:
		return false
	}
}

// Valid reports whether l is one of the defined license tiers.
func (l LicenseTier) Valid() bool {
	switch l {
	case LicenseBeta, LicenseArchive, LicenseStandard,
		LicenseHot, LicenseDedicated, LicenseSovereign:
		return true
	default:
		return false
	}
}

// Valid reports whether d is one of the defined DEK policies.
func (d DEKPolicy) Valid() bool {
	switch d {
	case DEKPerObject, DEKPerBucket:
		return true
	default:
		return false
	}
}

// Validate performs structural checks on the tenant record. It does
// not resolve external references (the placement policy, the CMK, or
// the anomaly profile); those are the responsibility of the services
// that own them.
func (t *Tenant) Validate() error {
	if t.ID == "" {
		return fmt.Errorf("tenant: id is required")
	}
	if t.Name == "" {
		return fmt.Errorf("tenant: name is required")
	}
	if !t.ContractType.Valid() {
		return fmt.Errorf("tenant: unknown contract_type %q", t.ContractType)
	}
	if !t.LicenseTier.Valid() {
		return fmt.Errorf("tenant: unknown license_tier %q", t.LicenseTier)
	}
	if t.Keys.RootKeyRef == "" {
		return fmt.Errorf("tenant: keys.root_key_ref is required")
	}
	if !t.Keys.DEKPolicy.Valid() {
		return fmt.Errorf("tenant: unknown keys.dek_policy %q", t.Keys.DEKPolicy)
	}
	if t.PlacementDefault.PolicyRef == "" {
		return fmt.Errorf("tenant: placement_default.policy_ref is required")
	}
	if t.Budgets.EgressTBMonth < 0 {
		return fmt.Errorf("tenant: budgets.egress_tb_month must be non-negative")
	}
	if t.Budgets.RequestsPerSec < 0 {
		return fmt.Errorf("tenant: budgets.requests_per_sec must be non-negative")
	}
	if t.Billing.Currency == "" {
		return fmt.Errorf("tenant: billing.currency is required")
	}
	return nil
}

// MarshalJSON exists so tenants always serialize through a stable
// shape even if we later add internal-only fields that should not
// cross the wire.
func (t *Tenant) MarshalJSON() ([]byte, error) {
	type alias Tenant
	return json.Marshal((*alias)(t))
}

// UnmarshalJSON is the mirror of MarshalJSON.
func (t *Tenant) UnmarshalJSON(data []byte) error {
	type alias Tenant
	return json.Unmarshal(data, (*alias)(t))
}

// MarshalYAML round-trips a tenant through yaml.v3. The implementation
// delegates to the struct tags so JSON and YAML stay in lock-step.
func (t *Tenant) MarshalYAML() (any, error) {
	type alias Tenant
	return (*alias)(t), nil
}

// UnmarshalYAML is the mirror of MarshalYAML.
func (t *Tenant) UnmarshalYAML(value *yaml.Node) error {
	type alias Tenant
	return value.Decode((*alias)(t))
}
