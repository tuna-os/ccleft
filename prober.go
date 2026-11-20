package ccleft

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Version is the library version, sent in the User-Agent.
var Version = "0.1.0-dev"

// Prober performs one-shot measurements. The zero value is usable; fields
// override defaults (mostly for tests and custom deployments).
type Prober struct {
	// HTTPClient is used for every HTTP probe. Default: a client with no
	// global timeout (per-provider timeouts come from Timeouts via ctx).
	HTTPClient *http.Client
	// Endpoints overrides a provider's base URL (e.g. an httptest server).
	Endpoints map[Provider]string
	// Timeouts overrides the per-provider deadline.
	Timeouts map[Provider]time.Duration
	// Exec runs CLI probes (agy). Default: os/exec with WaitDelay.
	Exec ExecFunc
	// AgyPath is the agy binary (default "agy" on PATH).
	AgyPath string
	// LookPath resolves binaries (default exec.LookPath).
	LookPath func(string) (string, error)
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Registry overrides the built-in provider registry for this Prober.
	// Leave it nil to use the built-in providers.
	Registry ProviderRegistry
}

// DefaultProber backs the package-level Probe.
var DefaultProber = &Prober{}

// Probe measures one source once with DefaultProber. It never writes to
// credential files and never panics; failures come back as a Reading whose
// State/Cause/Message say what went wrong.
func Probe(ctx context.Context, src Source) Reading { return DefaultProber.Probe(ctx, src) }

// DefaultTimeouts are the per-provider probe deadlines.
var DefaultTimeouts = map[Provider]time.Duration{
	Claude: 20 * time.Second, Codex: 20 * time.Second, Kiro: 20 * time.Second,
	Copilot: 20 * time.Second, DeepSeek: 20 * time.Second,
	// agy boots a full CLI; 10s (Hive's old value) timed out routinely.
	Agy: 90 * time.Second,
}

func (p *Prober) registry() ProviderRegistry {
	if p.Registry != nil {
		return p.Registry
	}
	return defaultProviderRegistry
}

// Register adds or replaces a provider for this Prober. The first custom
// registration copies the built-ins, so adding one provider does not remove
// the standard providers from a zero-value Prober.
func (p *Prober) Register(name string, provider ProviderImplementation) error {
	if p.Registry == nil {
		p.Registry = NewProviderRegistry()
		for registeredName, registered := range defaultProviderRegistry.All() {
			if err := p.Registry.Register(registeredName, registered); err != nil {
				return err
			}
		}
	}
	return p.Registry.Register(name, provider)
}

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Prober) timeout(pr Provider) time.Duration {
	if d, ok := p.Timeouts[pr]; ok && d > 0 {
		return d
	}
	if d, ok := DefaultTimeouts[pr]; ok {
		return d
	}
	return 20 * time.Second
}

func (p *Prober) endpoint(pr Provider, def string) string {
	if u, ok := p.Endpoints[pr]; ok && u != "" {
		return strings.TrimRight(u, "/")
	}
	return def
}

func (p *Prober) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

// Identify resolves the account fingerprint for src from local credentials
// only (no network). When no usable credential exists it returns a terminal
// Reading (auth_required / unsupported) instead.
func (p *Prober) Identify(src Source) (account string, terminal *Reading) {
	account, _, terminal = p.identify(src)
	return account, terminal
}

// identify is Identify plus a fingerprint of the credential itself. Homes
// that resolve to one account do not always hold the same token, and a
// provider refusing one token says nothing about the others, so the Client
// has to tell them apart. cred is empty when the provider exposes no token (a
// custom implementation).
func (p *Prober) identify(src Source) (account, cred string, terminal *Reading) {
	provider := p.registry().Get(string(src.Provider))
	if provider == nil {
		r := fail(src.Provider, StateError, "unknown_provider", fmt.Errorf("unknown provider %q", src.Provider))
		return "", "", &r
	}
	identity, terminal := provider.Identify(p, src)
	if c, ok := identity.value.(credential); ok && c.token != "" {
		cred = fingerprint(src.Provider, "credential:"+c.token)
	}
	if terminal != nil {
		p.finish(terminal, src, identity)
	}
	return identity.Account, cred, terminal
}

// Probe measures src once.
func (p *Prober) Probe(ctx context.Context, src Source) Reading {
	provider := p.registry().Get(string(src.Provider))
	if provider == nil {
		return fail(src.Provider, StateError, "unknown_provider", fmt.Errorf("unknown provider %q", src.Provider))
	}
	identity, terminal := provider.Identify(p, src)
	if terminal != nil {
		p.finish(terminal, src, identity)
		return *terminal
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout(src.Provider))
	defer cancel()
	r := provider.Probe(ctx, p, src, identity)
	p.finish(&r, src, identity)
	return r
}

func (p *Prober) finish(r *Reading, src Source, identity ProviderIdentity) {
	r.Provider = src.Provider
	if r.Account == "" {
		r.Account = identity.Account
	}
	if r.Plan == "" {
		r.Plan = identity.Plan
	}
	if r.Windows == nil {
		r.Windows = []Window{}
	}
	if r.FetchedAt.IsZero() {
		r.FetchedAt = p.now().UTC()
	}
	if r.State == "" {
		r.State = deriveState(r.Windows)
	}
	if r.RetryAt == nil && r.retryAfter > 0 {
		t := r.FetchedAt.Add(r.retryAfter)
		r.RetryAt = &t
	}
}
