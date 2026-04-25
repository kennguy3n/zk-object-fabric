package placement_policy

import (
	"fmt"
	"sort"
	"sync"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// Engine is the Phase 2 concrete PlacementEngine. It maps a
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
	sort.Slice(eligible, func(i, j int) bool {
		return storageRank(e.Providers[eligible[i]]) < storageRank(e.Providers[eligible[j]])
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

// storageRank is the proxy cost used to pick "cheapest". Lower is
// better. Phase 2 collapses the ProviderCostModel to a single scalar
// of (storage $/TB-month + expected egress $/GB × 1000). This is
// deliberately coarse: richer cost-shaping (per-tenant egress
// estimates, SLA weights) is deferred until Phase 3 billing is live.
func storageRank(p providers.StorageProvider) float64 {
	c := p.CostModel()
	return c.StorageUSDPerTBMonth + c.EgressUSDPerGB*1000
}

// compile-time assertion that Engine implements the shape expected
// by s3compat.PlacementEngine. The s3compat package imports metadata
// but not this package, so the check lives here via a local alias.
var _ interface {
	ResolveBackend(tenantID, bucket, objectKey string) (string, metadata.PlacementPolicy, error)
} = (*Engine)(nil)
