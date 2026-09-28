package ccleft

import (
	"context"
)

// legacyProviderAdapter wraps old-style function-based providers
// into the new ProviderImpl interface.
type legacyProviderAdapter struct {
	identify func(p *Prober, src Source) (credential, *Reading)
	fetch    func(ctx context.Context, p *Prober, src Source, c credential) Reading
}

// Identify implements ProviderImpl.Identify.
func (a *legacyProviderAdapter) Identify(p *Prober, src Source) (credential, *Reading) {
	return a.identify(p, src)
}

// Fetch implements ProviderImpl.Fetch.
func (a *legacyProviderAdapter) Fetch(ctx context.Context, p *Prober, src Source, c credential) Reading {
	return a.fetch(ctx, p, src, c)
}

// registerLegacy registers a function-based provider using the adapter.
func registerLegacy(name Provider, identify func(p *Prober, src Source) (credential, *Reading), fetch func(ctx context.Context, p *Prober, src Source, c credential) Reading) {
	RegisterProvider(name, &legacyProviderAdapter{
		identify: identify,
		fetch:    fetch,
	})
}
