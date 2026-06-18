package placement_policy

import (
	"fmt"
	"sort"
	"sync"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// Engine is the concrete PlacementEngine. It maps a
// (tenant, bucket, key) request to one of the StorageProviders in the
// Providers registry, honouring the tenant's Policy and falling back
// to Default when no tenant-specific policy is registered.
//
// Thread-safe: Providers and Policies are consulted under an RWMutex
// so control-plane updates (policy changes, new backends) can land
// without restarting the gateway.
type Engine struct {
	mu        sync.RWMutex
	Providers map[string]providers.StorageProvider
	Policies  map[string]*Policy
	// Default is the provider name used when no tenant policy is
	// registered. It must exist in Providers.
	Default string
}

// NewEngine returns an Engine ready to resolve requests.
func NewEngine(defaultBackend string, providers map[string]providers.StorageProvider, policies map[string]*Policy) *Engine {
	return &Engine{
		Providers: providers,
		Policies:  policies,
		Default:   defaultBackend,
	}
}

// RegisterProvider adds or replaces a backend at runtime.
func (e *Engine) RegisterProvider(name string, p providers.StorageProvider) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Providers == nil {
		e.Providers = map[string]providers.StorageProvider{}
	}
	e.Providers[name] = p
}

// SetPolicy installs or replaces the policy for tenantID.
func (e *Engine) SetPolicy(tenantID string, p *Policy) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Policies == nil {
		e.Policies = map[string]*Policy{}
	}
	e.Policies[tenantID] = p
}

// ResolveBackend implements s3compat.PlacementEngine. It returns the
// chosen backend name plus the materialized PlacementPolicy that the
// gateway records on the manifest.
func (e *Engine) ResolveBackend(tenantID, bucket, objectKey string) (string, metadata.PlacementPolicy, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	policy, ok := e.Policies[tenantID]
	def := e.Default

	if !ok || policy == nil {
		if def == "" {
			return "", metadata.PlacementPolicy{}, fmt.Errorf("placement: no policy for tenant %q and no default backend", tenantID)
		}
		if _, ok := e.Providers[def]; !ok {
			return "", metadata.PlacementPolicy{}, fmt.Errorf("placement: default backend %q is not registered", def)
		}
		return def, metadata.PlacementPolicy{
			AllowedBackends:   []string{def},
			MinFailureDomains: 1,
		}, nil
	}

	eligible := filterProviders(e.Providers, policy.Spec.Placement)
	if len(eligible) == 0 {
		return "", metadata.PlacementPolicy{}, fmt.Errorf("placement: no registered backend satisfies tenant %q policy", tenantID)
	}
	// WorkloadHint is derived from the policy's WorkloadProfile so
	// write-heavy and cold-archive tenants do not get ranked
	// against the same egress-amortised cost as a read-heavy CDN
	// workload. Zero values fall back to legacyEgressWeight so
	// existing policies without a WorkloadProfile keep the same
	// ordering as before this change landed.
	hint := workloadHintFromPolicy(policy)
	sort.Slice(eligible, func(i, j int) bool {
		return storageRank(e.Providers[eligible[i]], hint) < storageRank(e.Providers[eligible[j]], hint)
	})
	chosen := eligible[0]

	return chosen, metadata.PlacementPolicy{
		Residency:         append([]string(nil), policy.Spec.Placement.Country...),
		AllowedBackends:   eligible,
		MinFailureDomains: 1,
		HotCache:          policy.Spec.Placement.CacheLocation != "",
		EncryptionMode:    policy.Spec.Encryption.Mode,
	}, nil
}

// filterProviders keeps providers whose PlacementLabels are compatible
// with the policy's constraints. Providers missing from a non-empty
// allow-list are excluded. Empty constraint sets match anything.
func filterProviders(all map[string]providers.StorageProvider, spec PlacementSpec) []string {
	out := make([]string, 0, len(all))
	for name, p := range all {
		if !matchesString(spec.Provider, name) {
			continue
		}
		labels := p.PlacementLabels()
		if !matchesString(spec.Region, labels.Region) {
			continue
		}
		if !matchesString(spec.Country, labels.Country) {
			continue
		}
		if !matchesString(spec.StorageClass, labels.StorageClass) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func matchesString(allow []string, got string) bool {
	if len(allow) == 0 {
		return true
	}
	for _, v := range allow {
		if v == got {
			return true
		}
	}
	return false
}

// WorkloadHint is the engine-internal projection of the tenant's
// PlacementSpec.WorkloadProfile that storageRank consumes. It is a
// distinct type from WorkloadProfile so the engine can later be
// fed from sources other than the policy struct (e.g. live billing
// rollups) without changing the policy schema.
type WorkloadHint struct {
	// ReadWriteRatio is reads-per-write. See WorkloadProfile.
	ReadWriteRatio float64
	// AvgObjectSizeMB is the average object size in MiB. See
	// WorkloadProfile.
	AvgObjectSizeMB float64
}

// legacyEgressWeight is the weight storageRank gave to per-GB
// egress before WorkloadHint landed. It is used as the fallback
// when a tenant policy carries no WorkloadProfile so existing
// deployments do not see their backend ordering shift on upgrade.
const legacyEgressWeight = 1000.0

// egressWeight returns the per-GB egress multiplier storageRank
// should apply for this hint. When ReadWriteRatio and
// AvgObjectSizeMB are both positive, the multiplier is the
// expected GB egressed per GB stored (avg object size in MiB
// times reads per write, divided by 1024 MiB/GiB). Otherwise it
// falls back to legacyEgressWeight, which preserves the pre-PR-9
// behaviour for policies that have not declared a workload
// profile yet.
func (h WorkloadHint) egressWeight() float64 {
	if h.ReadWriteRatio <= 0 || h.AvgObjectSizeMB <= 0 {
		return legacyEgressWeight
	}
	return h.ReadWriteRatio * h.AvgObjectSizeMB / 1024.0
}

// workloadHintFromPolicy projects a policy's optional
// WorkloadProfile into the engine's WorkloadHint. A nil profile
// yields a zero hint, which storageRank treats as the legacy
// behaviour fallback.
func workloadHintFromPolicy(p *Policy) WorkloadHint {
	if p == nil || p.Spec.Placement.WorkloadProfile == nil {
		return WorkloadHint{}
	}
	wp := p.Spec.Placement.WorkloadProfile
	return WorkloadHint{
		ReadWriteRatio:  wp.ReadWriteRatio,
		AvgObjectSizeMB: wp.AvgObjectSizeMB,
	}
}

// storageRank is the proxy cost used to pick "cheapest". Lower is
// better. The scalar collapses the ProviderCostModel to
// (storage $/TB-month + expected egress $/GB × egressWeight),
// where egressWeight is workload-aware: a read-heavy small-object
// tenant amortises a lot of egress over each GB stored, while a
// write-heavy or cold-archive tenant amortises very little. Policies
// without a declared WorkloadProfile fall back to the historic
// flat weight of 1000 so this change is non-breaking for existing
// deployments. Richer shaping (SLA weights, per-region
// adjustments) remains deferred.
func storageRank(p providers.StorageProvider, hint WorkloadHint) float64 {
	c := p.CostModel()
	return c.StorageUSDPerTBMonth + c.EgressUSDPerGB*hint.egressWeight()
}

// compile-time assertion that Engine implements the shape expected
// by s3compat.PlacementEngine. The s3compat package imports metadata
// but not this package, so the check lives here via a local alias.
var _ interface {
	ResolveBackend(tenantID, bucket, objectKey string) (string, metadata.PlacementPolicy, error)
} = (*Engine)(nil)
