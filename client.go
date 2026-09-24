package ccleft

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

// DefaultMinInterval is the minimum time between two upstream calls for the
// same account. Claude's usage endpoint 429s aggressively when polled by
// many hosts, so it gets the longest interval.
var DefaultMinInterval = map[Provider]time.Duration{
	Claude:   3 * time.Minute,
	Codex:    time.Minute,
	Agy:      2 * time.Minute,
	Kiro:     time.Minute,
	Copilot:  time.Minute,
	DeepSeek: time.Minute,
}

// Client wraps a Prober with the machinery a fleet needs:
//
//   - per-account keying: sources are keyed by (provider, account
//     fingerprint), so N homes sharing one account make ONE upstream call;
//   - single-flight: concurrent Get calls for one account share one call;
//   - per-account rate limiting: at most one upstream call per MinInterval;
//   - Retry-After + exponential backoff on 429 / network / 5xx;
//   - last-good cache: during a transient failure the last good reading is
//     served with Stale=true and Cause/Message describing the failure.
//
// A Client is safe for concurrent use. The zero value is usable.
type Client struct {
	Prober *Prober
	// MinInterval overrides DefaultMinInterval per provider.
	MinInterval map[Provider]time.Duration
	// BaseBackoff is the first retry delay after a transient failure
	// (default 30s); it doubles per consecutive failure up to MaxBackoff
	// (default 30m). A longer Retry-After always wins.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// Jitter spreads retries by ±Jitter (fraction, e.g. 0.1) so a fleet
	// does not retry in lockstep. Default 0.
	Jitter float64

	mu      sync.Mutex
	entries map[string]*entry
	calls   map[string]*call
}

type entry struct {
	last        Reading
	lastGood    *Reading
	failures    int
	nextAllowed time.Time
}

type call struct {
	done chan struct{}
	r    Reading
}

// NewClient returns a Client using p (DefaultProber when nil).
func NewClient(p *Prober) *Client { return &Client{Prober: p} }

func (c *Client) prober() *Prober {
	if c.Prober != nil {
		return c.Prober
	}
	return DefaultProber
}

func (c *Client) minInterval(p Provider) time.Duration {
	if d, ok := c.MinInterval[p]; ok {
		return d
	}
	if d, ok := DefaultMinInterval[p]; ok {
		return d
	}
	return time.Minute
}

func (c *Client) backoff(failures int, retryAfter time.Duration) time.Duration {
	base, max := c.BaseBackoff, c.MaxBackoff
	if base <= 0 {
		base = 30 * time.Second
	}
	if max <= 0 {
		max = 30 * time.Minute
	}
	d := base
	for i := 1; i < failures && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	if c.Jitter > 0 {
		d = time.Duration(float64(d) * (1 + c.Jitter*(2*rand.Float64()-1)))
	}
	if retryAfter > d {
		d = retryAfter
	}
	return d
}

// Get returns the current reading for src, probing upstream only when the
// account's rate limit / backoff allows it.
func (c *Client) Get(ctx context.Context, src Source) Reading {
	p := c.prober()
	acct, terminal := p.Identify(src)
	key := string(src.Provider) + "/" + acct
	if terminal != nil {
		// Answered from local files (no credential, unsupported login):
		// nothing upstream to protect, so no caching — except a transient
		// local failure (an expired-but-refreshable token), which serves the
		// account's last-good reading until the CLI rotates the token.
		if terminal.transient && acct != "" {
			c.mu.Lock()
			defer c.mu.Unlock()
			if e := c.entries[key]; e != nil {
				return c.staleOr(e, *terminal)
			}
		}
		return *terminal
	}

	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*entry{}
		c.calls = map[string]*call{}
	}
	if e := c.entries[key]; e != nil && p.now().Before(e.nextAllowed) {
		r := c.cached(e)
		c.mu.Unlock()
		return r
	}
	if cl, ok := c.calls[key]; ok {
		c.mu.Unlock()
		select {
		case <-cl.done:
			return cloneReading(cl.r)
		case <-ctx.Done():
			r := fail(src.Provider, StateError, "timeout", fmt.Errorf("waiting for in-flight probe: %w", ctx.Err()))
			r.Account = acct
			return r
		}
	}
	cl := &call{done: make(chan struct{})}
	c.calls[key] = cl
	c.mu.Unlock()

	r := p.Probe(ctx, src)

	c.mu.Lock()
	e := c.entries[key]
	if e == nil {
		e = &entry{}
		c.entries[key] = e
	}
	out := c.record(e, r, src.Provider, p.now())
	cl.r = out
	delete(c.calls, key)
	c.mu.Unlock()
	close(cl.done)
	return cloneReading(out)
}

// record stores a fresh probe result and returns what the caller should see.
func (c *Client) record(e *entry, r Reading, prov Provider, now time.Time) Reading {
	if !r.transient {
		e.failures = 0
		e.last = r
		e.nextAllowed = now.Add(c.minInterval(prov))
		if r.State.HasQuota() {
			g := cloneReading(r)
			e.lastGood = &g
		}
		return r
	}
	e.failures++
	wait := c.backoff(e.failures, r.retryAfter)
	if mi := c.minInterval(prov); r.State == StateRateLimited && wait < mi {
		wait = mi
	}
	e.nextAllowed = now.Add(wait)
	next := e.nextAllowed.UTC()
	r.RetryAt = &next
	e.last = r
	return c.staleOr(e, r)
}

// cached answers from the entry without an upstream call.
func (c *Client) cached(e *entry) Reading {
	if e.last.transient {
		return c.staleOr(e, e.last)
	}
	return cloneReading(e.last)
}

// staleOr serves last-good marked stale, or the failure itself when there
// is no last-good reading.
func (c *Client) staleOr(e *entry, failed Reading) Reading {
	if e.lastGood == nil {
		return cloneReading(failed)
	}
	s := cloneReading(*e.lastGood)
	s.Stale = true
	s.Cause = failed.Cause
	s.Message = fmt.Sprintf("serving last-good reading from %s: %s", e.lastGood.FetchedAt.Format(time.RFC3339), failed.Message)
	s.RetryAt = failed.RetryAt
	s.err = failed.err
	s.transient = true
	return s
}

func cloneReading(r Reading) Reading {
	r.Windows = append([]Window(nil), r.Windows...)
	if r.Windows == nil {
		r.Windows = []Window{}
	}
	r.Homes = append([]string(nil), r.Homes...)
	return r
}

// GetAll probes every source concurrently (bounded) and returns one Reading
// per distinct (provider, account): homes sharing an account are merged
// into that reading's Homes. Sources with no credential stay separate.
func (c *Client) GetAll(ctx context.Context, srcs []Source) []Reading {
	res := make([]Reading, len(srcs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, s := range srcs {
		wg.Add(1)
		go func(i int, s Source) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res[i] = c.Get(ctx, s)
		}(i, s)
	}
	wg.Wait()
	return Merge(srcs, res)
}

// Merge collapses readings by (provider, account), recording each source's
// home (or label) in Homes. srcs[i] produced rs[i].
func Merge(srcs []Source, rs []Reading) []Reading {
	idx := map[string]int{}
	var out []Reading
	for i, r := range rs {
		where := firstNonEmpty(srcs[i].Label, srcs[i].Home, "env")
		key := string(r.Provider) + "/" + r.Account
		if r.Account == "" {
			key = fmt.Sprintf("%s//%d", r.Provider, i)
		}
		if j, ok := idx[key]; ok {
			if !contains(out[j].Homes, where) {
				out[j].Homes = append(out[j].Homes, where)
			}
			continue
		}
		r.Homes = []string{where}
		idx[key] = len(out)
		out = append(out, r)
	}
	order := map[Provider]int{}
	for i, p := range Providers {
		order[p] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return order[out[i].Provider] < order[out[j].Provider]
		}
		return out[i].Account < out[j].Account
	})
	for i := range out {
		sort.Strings(out[i].Homes)
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
