package erasure_coding

import (
	"fmt"
	"sort"
	"sync"
)

// Registry is a thread-safe map of named profiles to ready-to-use
// Encoders. The gateway keeps one Registry wired into the s3compat
// handler so placement policies can reference profiles by name.
type Registry struct {
	mu       sync.RWMutex
	encoders map[string]*Encoder
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{encoders: map[string]*Encoder{}}
}

// DefaultRegistry returns a Registry pre-populated with the Phase 2+
// StandardProfiles. Operators can still Register additional custom
// profiles on top of it.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	for _, p := range StandardProfiles() {
		_ = r.Register(p)
	}
	return r
}

// Register adds a profile to the registry. It returns an error if
// the profile fails validation.
func (r *Registry) Register(p ErasureCodingProfile) error {
	enc, err := NewEncoder(p)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.encoders[p.Name] = enc
	r.mu.Unlock()
	return nil
}

// Lookup returns the encoder registered under name or an error if
// no such profile exists.
func (r *Registry) Lookup(name string) (*Encoder, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	enc, ok := r.encoders[name]
	if !ok {
		return nil, fmt.Errorf("erasure_coding: profile %q is not registered", name)
	}
	return enc, nil
}

// Names returns the registered profile names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.encoders))
	for n := range r.encoders {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
