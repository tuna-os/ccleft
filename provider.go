package ccleft

import (
	"context"
)

// ProviderImpl defines the contract for a quota provider implementation.
// Each provider must implement these two methods to participate in the
// probe registry.
type ProviderImpl interface {
	// Identify resolves account fingerprint and credentials from local files
	// or environment only (no network). It returns a credential and optionally
	// a terminal Reading if the provider cannot be used.
	Identify(p *Prober, src Source) (credential, *Reading)

	// Fetch performs the actual probe: it fetches current quota usage from
	// the provider's API using the credential resolved by Identify.
	Fetch(ctx context.Context, p *Prober, src Source, c credential) Reading
}

// providerRegistry holds the registered ProviderImpl for each named Provider.
var providerRegistry = map[Provider]ProviderImpl{}

// RegisterProvider registers a ProviderImpl for a named provider.
// This is called during init() by each provider module.
func RegisterProvider(name Provider, impl ProviderImpl) {
	providerRegistry[name] = impl
}

// getProvider returns the registered ProviderImpl for the named provider,
// or nil if not found.
func getProvider(name Provider) ProviderImpl {
	return providerRegistry[name]
}
