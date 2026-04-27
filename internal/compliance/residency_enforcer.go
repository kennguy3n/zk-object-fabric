package compliance

import (
	"errors"
	"strings"
)

// ErrResidencyViolation is returned by ResidencyEnforcer.Check
// when the resolved provider's country is not in the tenant's
// allowed country list. The S3 handler maps this to a 403
// DataResidencyViolation response.
var ErrResidencyViolation = errors.New("compliance: data residency violation")

// AllowlistLookup returns the country list a tenant is permitted
// to land data in. An empty slice means "no restriction" — the
// enforcer treats this as allow-all so legacy tenants without an
// allowlist row do not regress.
type AllowlistLookup func(tenantID string) ([]string, error)

// ResidencyEnforcer is a pre-flight check called from the PUT
// hot path. The check is intentionally cheap: a string-set
// containment with case-insensitive comparison.
type ResidencyEnforcer struct {
	Allowlist AllowlistLookup
}

// NewResidencyEnforcer constructs an enforcer using the provided
// lookup. A nil lookup makes Check a no-op so the gateway is safe
// to start without a configured Postgres dependency.
func NewResidencyEnforcer(lookup AllowlistLookup) *ResidencyEnforcer {
	return &ResidencyEnforcer{Allowlist: lookup}
}

// Check returns nil if backendCountry is permitted for tenantID
// or if the tenant has no allowlist configured. It returns
// ErrResidencyViolation when the country is rejected. Any other
// error is the lookup's transport error.
//
// policyResidency is the per-object residency hint baked into the
// manifest's PlacementPolicy. When non-empty, it is intersected
// with the tenant allowlist; the strictest list wins.
func (e *ResidencyEnforcer) Check(tenantID, backendCountry string, policyResidency []string) error {
	if e == nil {
		return nil
	}
	bc := strings.ToUpper(strings.TrimSpace(backendCountry))
	if bc == "" {
		// Provider did not advertise a country. Treat as
		// non-residency-aware and allow.
		return nil
	}
	allowed := []string{}
	if e.Allowlist != nil {
		raw, err := e.Allowlist(tenantID)
		if err != nil {
			return err
		}
		allowed = append(allowed, raw...)
	}
	if len(policyResidency) > 0 {
		// Intersect with the per-object hint so the strictest
		// list wins. If the tenant has no allowlist, the
		// per-object list is the active one.
		if len(allowed) == 0 {
			allowed = append(allowed, policyResidency...)
		} else {
			allowed = intersect(allowed, policyResidency)
		}
	}
	if len(allowed) == 0 {
		return nil
	}
	for _, c := range allowed {
		if strings.ToUpper(strings.TrimSpace(c)) == bc {
			return nil
		}
	}
	return ErrResidencyViolation
}

func intersect(a, b []string) []string {
	set := map[string]struct{}{}
	for _, x := range a {
		set[strings.ToUpper(strings.TrimSpace(x))] = struct{}{}
	}
	out := []string{}
	for _, y := range b {
		k := strings.ToUpper(strings.TrimSpace(y))
		if _, ok := set[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// StaticAllowlist returns an AllowlistLookup backed by an
// in-memory map. Useful for tests and small deployments without
// a Postgres dependency.
func StaticAllowlist(m map[string][]string) AllowlistLookup {
	return func(tenantID string) ([]string, error) {
		v, ok := m[tenantID]
		if !ok {
			return nil, nil
		}
		return v, nil
	}
}
