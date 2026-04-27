package compliance

import (
	"errors"
	"testing"
)

func TestResidencyEnforcer_AllowsWhenNoAllowlist(t *testing.T) {
	e := NewResidencyEnforcer(nil)
	if err := e.Check("t1", "US", nil); err != nil {
		t.Errorf("nil allowlist should allow, got %v", err)
	}
}

func TestResidencyEnforcer_AllowsEmptyCountry(t *testing.T) {
	e := NewResidencyEnforcer(StaticAllowlist(map[string][]string{"t1": {"DE"}}))
	if err := e.Check("t1", "", nil); err != nil {
		t.Errorf("empty backend country should allow, got %v", err)
	}
}

func TestResidencyEnforcer_RejectsForeignCountry(t *testing.T) {
	e := NewResidencyEnforcer(StaticAllowlist(map[string][]string{"t1": {"DE", "FR"}}))
	err := e.Check("t1", "US", nil)
	if !errors.Is(err, ErrResidencyViolation) {
		t.Errorf("got %v, want ErrResidencyViolation", err)
	}
}

func TestResidencyEnforcer_AllowsListedCountryCaseInsensitive(t *testing.T) {
	e := NewResidencyEnforcer(StaticAllowlist(map[string][]string{"t1": {"de"}}))
	if err := e.Check("t1", "DE", nil); err != nil {
		t.Errorf("case-insensitive match failed: %v", err)
	}
}

func TestResidencyEnforcer_PolicyResidencyIntersect(t *testing.T) {
	// Tenant allowlist is {DE,FR}; per-object policy narrows to {FR}.
	// Backend in DE must now be rejected.
	e := NewResidencyEnforcer(StaticAllowlist(map[string][]string{"t1": {"DE", "FR"}}))
	err := e.Check("t1", "DE", []string{"FR"})
	if !errors.Is(err, ErrResidencyViolation) {
		t.Errorf("intersect should reject DE, got %v", err)
	}
	if err := e.Check("t1", "FR", []string{"FR"}); err != nil {
		t.Errorf("FR should be allowed, got %v", err)
	}
}

func TestResidencyEnforcer_PolicyResidencyOnly(t *testing.T) {
	// No tenant allowlist: per-object hint becomes the active list.
	e := NewResidencyEnforcer(nil)
	if err := e.Check("t1", "DE", []string{"FR"}); !errors.Is(err, ErrResidencyViolation) {
		t.Errorf("DE should be rejected by policy-only list, got %v", err)
	}
}
