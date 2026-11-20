package ccleft

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

// credential is what identify() resolves from local files, with no network.
type credential struct {
	token   string
	account string // fingerprint
	plan    string
	extra   map[string]string
}

type impl struct {
	identify func(p *Prober, src Source) (credential, *Reading)
	fetch    func(ctx context.Context, p *Prober, src Source, c credential) Reading
}

type providerAdapter struct {
	name     Provider
	identify func(p *Prober, src Source) (credential, *Reading)
	fetch    func(ctx context.Context, p *Prober, src Source, c credential) Reading
}

func (a providerAdapter) Name() Provider { return a.name }

func (a providerAdapter) Identify(p *Prober, src Source) (ProviderIdentity, *Reading) {
	c, terminal := a.identify(p, src)
	return ProviderIdentity{Account: c.account, Plan: c.plan, value: c}, terminal
}

func (a providerAdapter) Probe(ctx context.Context, p *Prober, src Source, identity ProviderIdentity) Reading {
	c, ok := identity.value.(credential)
	if !ok {
		return fail(src.Provider, StateError, "provider_identity", errors.New("built-in provider received an invalid identity"))
	}
	return a.fetch(ctx, p, src, c)
}

func register(p Provider, i impl) {
	provider := providerAdapter{name: p, identify: i.identify, fetch: i.fetch}
	if err := defaultProviderRegistry.Register(string(p), provider); err != nil {
		panic(err)
	}
	Providers = RegisteredProviders()
}

// RegisteredProviders returns a sorted list of all registered built-in
// providers. Custom per-Prober registries do not alter the package defaults.
func RegisteredProviders() []Provider {
	registered := defaultProviderRegistry.All()
	providers := make([]Provider, 0, len(registered))
	for name := range registered {
		providers = append(providers, Provider(name))
	}
	sort.Slice(providers, func(i, j int) bool {
		return providers[i] < providers[j]
	})
	return providers
}
