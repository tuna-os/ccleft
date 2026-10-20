package ccleft

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// ProviderIdentity contains the non-secret account metadata resolved before a
// provider probe. Implementations may leave the fields empty when they do not
// have local identity information.
//
// The private value is reserved for the built-in adapters. External
// implementations can construct an identity with the exported fields and
// ignore it.
type ProviderIdentity struct {
	Account string
	Plan    string
	value   any
}

// ProviderImplementation is the extension point for quota measurements.
// Implementations own credential discovery and response handling; Prober owns
// deadlines and common Reading finalization.
type ProviderImplementation interface {
	// Name returns the provider identifier (for example, "claude").
	Name() Provider

	// Identify resolves local credentials without network I/O. A non-nil
	// Reading is an immediate result (for example, missing credentials), while
	// the identity is still returned so the caller can use a last-good cache.
	Identify(p *Prober, src Source) (ProviderIdentity, *Reading)

	// Probe performs the network or local measurement using the resolved
	// identity. The context already carries the provider deadline.
	Probe(ctx context.Context, p *Prober, src Source, identity ProviderIdentity) Reading
}

// ProviderRegistry maps provider names to their implementations.
// Registries are replaceable so callers can add providers or inject test
// implementations without changing Prober's core dispatch.
type ProviderRegistry interface {
	// Register associates a provider name with an implementation. Registering
	// an existing name replaces that implementation.
	Register(name string, provider ProviderImplementation) error

	// Get retrieves a provider by name, returning nil if not found.
	Get(name string) ProviderImplementation

	// All returns a snapshot of the registered providers.
	All() map[string]ProviderImplementation
}

type providerRegistry struct {
	mu        sync.RWMutex
	providers map[string]ProviderImplementation
}

// NewProviderRegistry returns an empty registry for custom or test providers.
func NewProviderRegistry() ProviderRegistry {
	return &providerRegistry{providers: make(map[string]ProviderImplementation)}
}

func (r *providerRegistry) Register(name string, provider ProviderImplementation) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("ccleft: provider name must not be empty")
	}
	if provider == nil {
		return fmt.Errorf("ccleft: provider %q must not be nil", name)
	}
	if provider.Name() == "" {
		return fmt.Errorf("ccleft: provider %q returned an empty name", name)
	}
	if string(provider.Name()) != name {
		return fmt.Errorf("ccleft: provider name %q does not match implementation name %q", name, provider.Name())
	}

	r.mu.Lock()
	r.providers[name] = provider
	r.mu.Unlock()
	return nil
}

func (r *providerRegistry) Get(name string) ProviderImplementation {
	r.mu.RLock()
	provider := r.providers[name]
	r.mu.RUnlock()
	return provider
}

func (r *providerRegistry) All() map[string]ProviderImplementation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]ProviderImplementation, len(r.providers))
	for name, provider := range r.providers {
		out[name] = provider
	}
	return out
}

var defaultProviderRegistry = NewProviderRegistry()
