package tenant

import "testing"

func TestDefaultTierConfigs_AllValidTiers(t *testing.T) {
	cfgs := DefaultTierConfigs()
	if len(cfgs) != 5 {
		t.Fatalf("len=%d, want 5", len(cfgs))
	}
	for _, c := range cfgs {
		if !c.Tier.Valid() {
			t.Errorf("tier %q is not in the LicenseTier whitelist", c.Tier)
		}
		if c.DefaultECProfile == "" || c.CachePolicy == "" || c.DedupPolicy == "" {
			t.Errorf("tier %q has empty required fields: %+v", c.Tier, c)
		}
	}
}

func TestTierConfigFor_KnownTier(t *testing.T) {
	c, ok := TierConfigFor(LicenseSovereign)
	if !ok {
		t.Fatal("sovereign tier must resolve")
	}
	if !c.CountryLocked {
		t.Error("sovereign tier must be country-locked")
	}
	if c.PlacementMode != "sovereign" {
		t.Errorf("sovereign placement mode = %q, want sovereign", c.PlacementMode)
	}
}

func TestTierConfigFor_UnknownTier(t *testing.T) {
	if _, ok := TierConfigFor("nonsense"); ok {
		t.Error("unknown tier must return (TierConfig{}, false)")
	}
}

func TestTierConfigMap_NoDuplicateTiers(t *testing.T) {
	m := TierConfigMap()
	if len(m) != len(DefaultTierConfigs()) {
		t.Errorf("DefaultTierConfigs has duplicate Tier keys")
	}
}
