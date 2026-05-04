package adapter

import (
	"fmt"
	"sync"
)

// Registry is a thread-safe map of provider ID to PaymentProvider.
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
	r.adapters[p.Info().ID] = p
}

// Get returns the adapter for the given provider ID.
func (r *Registry) Get(providerID string) (PaymentProvider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.adapters[providerID]
	if !ok {
		return nil, fmt.Errorf("unknown payment provider: %q", providerID)
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

func (r *Registry) Infos() []ProviderInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	infos := make([]ProviderInfo, 0, len(r.adapters))
	for _, p := range r.adapters {
		infos = append(infos, p.Info())
	}
	return infos
}
