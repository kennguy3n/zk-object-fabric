package billing

import (
	"fmt"
	"log"
	"sort"
	"sync"
)

// ProviderFactory builds a BillingProvider from the per-deployment
// ProviderConfig key/value map. Plug-ins register themselves at
// init() time via RegisterProvider so cmd/gateway/main.go can
// resolve the configured provider without importing each
// vendor package directly.
type ProviderFactory func(cfg ProviderFactoryConfig) (BillingProvider, error)

// ProviderFactoryConfig is the input every ProviderFactory
// receives. The key/value map mirrors BillingConfig.ProviderConfig
// from internal/config, kept as a free-form bag so adding a new
// provider does not require widening the config schema.
type ProviderFactoryConfig struct {
	// Name is the registered provider name (e.g. "stripe").
	Name string
	// Settings is the vendor-specific config bag.
	Settings map[string]string
	// Logger is the logger the provider should adopt for its
	// internal diagnostics. Nil disables logging.
	Logger *log.Logger
}

var (
	providerMu        sync.RWMutex
	providerFactories = map[string]ProviderFactory{}
)

// RegisterProvider records factory under name. Calls with a
// duplicate name panic so misconfigured init() ordering surfaces
// at startup. Names are case-insensitive but stored lower-cased.
func RegisterProvider(name string, factory ProviderFactory) {
	if name == "" {
		panic("billing: RegisterProvider: empty name")
	}
	if factory == nil {
		panic("billing: RegisterProvider: nil factory for " + name)
	}
	providerMu.Lock()
	defer providerMu.Unlock()
	key := normalizeProviderName(name)
	if _, dup := providerFactories[key]; dup {
		panic("billing: duplicate provider registration: " + key)
	}
	providerFactories[key] = factory
}

// LookupProvider returns the factory registered under name and
// reports whether it was found.
func LookupProvider(name string) (ProviderFactory, bool) {
	providerMu.RLock()
	defer providerMu.RUnlock()
	f, ok := providerFactories[normalizeProviderName(name)]
	return f, ok
}

// RegisteredProviders returns the registered provider names in
// alphabetical order. Useful for surfacing the available set in
// the operator dashboard or in --help output.
func RegisteredProviders() []string {
	providerMu.RLock()
	names := make([]string, 0, len(providerFactories))
	for k := range providerFactories {
		names = append(names, k)
	}
	providerMu.RUnlock()
	sort.Strings(names)
	return names
}

// BuildProvider resolves the factory bound to cfg.Name and runs
// it. An empty name resolves to "noop" so deployments that have
// not configured a provider still get a working gateway.
func BuildProvider(cfg ProviderFactoryConfig) (BillingProvider, error) {
	name := cfg.Name
	if name == "" {
		name = "noop"
	}
	factory, ok := LookupProvider(name)
	if !ok {
		return nil, fmt.Errorf("billing: no provider registered as %q (registered: %v)", name, RegisteredProviders())
	}
	return factory(ProviderFactoryConfig{Name: name, Settings: cfg.Settings, Logger: cfg.Logger})
}

func normalizeProviderName(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

func init() {
	RegisterProvider("noop", func(cfg ProviderFactoryConfig) (BillingProvider, error) {
		return NewNoopProvider(cfg.Logger), nil
	})
}
