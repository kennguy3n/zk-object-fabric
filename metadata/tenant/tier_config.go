package tenant

// TierConfig is the canonical mapping from a LicenseTier to the
// product knobs the gateway pre-fills when a new tenant lands on
// that tier:
//
//   - DefaultECProfile: the erasure-coding name on PlacementPolicy.
//   - CachePolicy: hot-object cache aggressiveness ("none", "l1", "l0+l1").
//   - DedupPolicy: convergent dedup scope ("disabled", "object", "object+block").
//   - EgressBudgetTBMonth: the soft-cap on monthly egress used by the
//     abuse guard / budget enforcer.
//   - PricePerTBMonth: the headline list price the console shows on
//     the tier-comparison page; not a billing source of truth.
//   - PlacementMode: whether the tier defaults to "shared", "dedicated",
//     or "sovereign" placement.
//
// Operators can override any field per-tenant at signup time by
// merging onto the result of TierConfigFor; the canonical map below
// only sets defaults.
type TierConfig struct {
	Tier                LicenseTier `json:"tier"`
	DisplayName         string      `json:"display_name"`
	DefaultECProfile    string      `json:"default_ec_profile"`
	CachePolicy         string      `json:"cache_policy"`
	DedupPolicy         string      `json:"dedup_policy"`
	EgressBudgetTBMonth uint64      `json:"egress_budget_tb_month"`
	PricePerTBMonth     float64     `json:"price_per_tb_month"`
	PlacementMode       string      `json:"placement_mode"`
	CountryLocked       bool        `json:"country_locked"`
}

// DefaultTierConfigs returns the canonical mapping defined in the
// Phase 4 Tier 3 product brief. The returned slice is in
// product-ordered presentation order; consumers that need a map
// can build one with TierConfigMap.
func DefaultTierConfigs() []TierConfig {
	return []TierConfig{
		{
			Tier:                LicenseArchive,
			DisplayName:         "Archive",
			DefaultECProfile:    "rs-16-4",
			CachePolicy:         "none",
			DedupPolicy:         "object",
			EgressBudgetTBMonth: 1,
			PricePerTBMonth:     2.50,
			PlacementMode:       "shared",
		},
		{
			Tier:                LicenseStandard,
			DisplayName:         "Standard",
			DefaultECProfile:    "rs-8-3",
			CachePolicy:         "l1",
			DedupPolicy:         "object",
			EgressBudgetTBMonth: 5,
			PricePerTBMonth:     8.00,
			PlacementMode:       "shared",
		},
		{
			Tier:                LicenseHot,
			DisplayName:         "Hot",
			DefaultECProfile:    "rs-6-2",
			CachePolicy:         "l0+l1",
			DedupPolicy:         "object",
			EgressBudgetTBMonth: 25,
			PricePerTBMonth:     20.00,
			PlacementMode:       "shared",
		},
		{
			Tier:                LicenseDedicated,
			DisplayName:         "Dedicated",
			DefaultECProfile:    "rs-8-3",
			CachePolicy:         "l0+l1",
			DedupPolicy:         "object+block",
			EgressBudgetTBMonth: 100,
			PricePerTBMonth:     45.00,
			PlacementMode:       "dedicated",
		},
		{
			Tier:                LicenseSovereign,
			DisplayName:         "Sovereign",
			DefaultECProfile:    "rs-8-3",
			CachePolicy:         "l0+l1",
			DedupPolicy:         "object",
			EgressBudgetTBMonth: 50,
			PricePerTBMonth:     65.00,
			PlacementMode:       "sovereign",
			CountryLocked:       true,
		},
	}
}

// TierConfigMap returns DefaultTierConfigs() keyed by LicenseTier.
func TierConfigMap() map[LicenseTier]TierConfig {
	cfgs := DefaultTierConfigs()
	out := make(map[LicenseTier]TierConfig, len(cfgs))
	for _, c := range cfgs {
		out[c.Tier] = c
	}
	return out
}

// TierConfigFor returns the canonical config for tier or
// (TierConfig{}, false) if the tier is unknown.
func TierConfigFor(tier LicenseTier) (TierConfig, bool) {
	c, ok := TierConfigMap()[tier]
	return c, ok
}
