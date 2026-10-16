package ccleft

import (
	"context"
)

// Provider is an interface for quota measurement implementations.
// Each quota provider (Claude, ChatGPT, Kiro, etc.) implements Provider
// to enable pluggable, testable provider implementations.
//
// Implementations should be registered with the Prober during initialization
// using the internal register() function. This allows new providers to be added
// without modifying the Prober's core dispatch logic.
type Provider interface {
	// Name returns the provider identifier (e.g., "claude", "codex").
	Name() string

	// Identify resolves credentials from local sources (environment variables,
	// config files, etc.) without network I/O. It returns the credential and
	// any reading that indicates immediate failure (missing credential, etc.).
	// If both credential and reading are non-nil, the reading indicates a
	// recoverable error state; Identify should prefer returning a reading to
	// indicate the problem.
	Identify(p *Prober, src Source) (credential, *Reading)

	// Fetch performs the actual quota measurement with the given credential.
	// It is responsible for network I/O, parsing responses, and returning
	// a Reading with the measurement result or error details.
	Fetch(ctx context.Context, p *Prober, src Source, c credential) Reading
}

// ProviderRegistry maps provider names to their implementations.
// This enables runtime provider registration and swapping for testing.
// It replaces the hardcoded switch statements that previously coupled
// Prober and Client to specific provider implementations.
type ProviderRegistry interface {
	// Register associates a provider name with an implementation.
	Register(name string, provider Provider) error

	// Get retrieves a provider by name, returning nil if not found.
	Get(name string) Provider

	// All returns a map of all registered providers.
	All() map[string]Provider
}
