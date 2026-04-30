package adapter

import (
	"fmt"
	"sync"
)

// Registry is a thread-safe map of provider name → PaymentProvider.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]PaymentProvider
}

func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]PaymentProvider)}
}

func (r *Registry) Register(p PaymentProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[p.Name()] = p
}

// Get returns the adapter for the given provider name.
// For Generic adapters, name is "generic_{custom_provider_name}".
func (r *Registry) Get(name string) (PaymentProvider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("unknown payment provider: %q", name)
	}
	return p, nil
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.adapters))
	for n := range r.adapters {
		names = append(names, n)
	}
	return names
}
