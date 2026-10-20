package ccleft

import (
	"context"
	"testing"
	"time"
)

type registryTestProvider struct {
	probes int
}

func (*registryTestProvider) Name() Provider { return Provider("custom") }

func (*registryTestProvider) Identify(*Prober, Source) (ProviderIdentity, *Reading) {
	return ProviderIdentity{Account: "custom-account", Plan: "custom-plan"}, nil
}

func (p *registryTestProvider) Probe(ctx context.Context, _ *Prober, _ Source, _ ProviderIdentity) Reading {
	if _, ok := ctx.Deadline(); ok {
		p.probes++
	}
	return Reading{
		State: StateOK,
		Windows: []Window{{
			ID:        "custom-window",
			Kind:      KindCredits,
			Binding:   true,
			Remaining: f64(1),
			Limit:     f64(2),
		}},
	}
}

func TestProberUsesRegisteredProvider(t *testing.T) {
	provider := &registryTestProvider{}
	p := &Prober{Now: func() time.Time { return testNow }}
	if err := p.Register("custom", provider); err != nil {
		t.Fatal(err)
	}

	r := p.Probe(context.Background(), Source{Provider: "custom"})
	if r.Provider != "custom" || r.Account != "custom-account" || r.Plan != "custom-plan" {
		t.Fatalf("identity was not finalized: %+v", r)
	}
	if r.State != StateOK || len(r.Windows) != 1 || r.Windows[0].ID != "custom-window" {
		t.Fatalf("custom provider result was not returned: %+v", r)
	}
	if !r.FetchedAt.Equal(testNow) || provider.probes != 1 {
		t.Fatalf("deadline or finalization missing: fetched=%s probes=%d", r.FetchedAt, provider.probes)
	}

	account, terminal := p.Identify(Source{Provider: "custom"})
	if account != "custom-account" || terminal != nil {
		t.Fatalf("Identify() = %q, %v", account, terminal)
	}
	if p.registry().Get(string(Claude)) == nil {
		t.Fatal("Register removed built-in providers")
	}
}

func TestProviderRegistryValidatesNames(t *testing.T) {
	registry := NewProviderRegistry()
	provider := &registryTestProvider{}
	for _, name := range []string{"", "other"} {
		if err := registry.Register(name, provider); err == nil {
			t.Fatalf("Register(%q) accepted an invalid name", name)
		}
	}
	if err := registry.Register("custom", provider); err != nil {
		t.Fatal(err)
	}
	if got := registry.Get("custom"); got != provider {
		t.Fatalf("Get(custom) = %T, want registered implementation", got)
	}
}
