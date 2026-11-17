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
//   - per-account rate limiting: at most one upstream call per MinInterval
//     (less a 5%, ≤30 s tolerance for scheduling jitter);
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

	// OnUpstream, when set, is called after every UPSTREAM probe (an HTTP
	// request or an agy run) — never for answers served from the cache,
	// backoff or local files. r is the raw probe result (before last-good
	// substitution); d is how long the probe took. Use it for logging.
	OnUpstream func(src Source, r Reading, d time.Duration)

	mu       sync.Mutex
	entries  map[string]*entry
	calls    map[string]*call
	upstream map[UpstreamKey]uint64
}

// UpstreamKey labels one bucket of upstream probe counts.
type UpstreamKey struct {
	Provider Provider
	State    State
	Cause    string
}

// UpstreamCounts returns how many upstream probes (HTTP requests / agy runs)
// this Client has made since it was created, by provider and outcome. It is
// the ground truth for "how hard are we polling the quota endpoints".
func (c *Client) UpstreamCounts() map[UpstreamKey]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[UpstreamKey]uint64, len(c.upstream))
	for k, v := range c.upstream {
		out[k] = v
	}
	return out
}

type entry struct {
	last        Reading
	lastGood    *Reading
	failures    int
	nextAllowed time.Time
	// slack lets a call through slightly before nextAllowed. Set only when
	// nextAllowed comes from MinInterval (see minIntervalSlack); zero after a
	// failure, so backoff and Retry-After are honoured exactly.
	slack time.Duration
	// rejected holds the credentials the provider refused. A refusal belongs
	// to one credential, not to the account: the account identity is read
	// from each home's own files, so a home with a dead or made-up token
	// must not decide what the other homes of that account are told.
	rejected map[string]rejection
}

// rejection is one refused credential: the failure, and the time until which
// it is answered from here instead of asking the provider again.
type rejection struct {
	r     Reading
	until time.Time
}

// minIntervalSlack is how early a call may pass the MinInterval gate: 5% of
// the interval, at most 30 s. A caller polling at exactly MinInterval (ccleft
// serve refreshes every 5m with a 5m interval) otherwise loses a race of a few
// hundred milliseconds — the previous probe finished later in its cycle than
// the next cycle starts — and is served from cache every OTHER time, halving
// the effective refresh rate.
func minIntervalSlack(d time.Duration) time.Duration {
	s := d / 20
	if s > 30*time.Second {
		s = 30 * time.Second
	}
	return s
}

type call struct {
	done chan struct{}
	r    Reading
	cred string
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
	acct, cred, terminal := p.identify(src)
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
	var cl *call
	for cl == nil {
		if e := c.entries[key]; e != nil {
			if rj, ok := e.rejected[cred]; ok && p.now().Before(rj.until) {
				c.mu.Unlock()
				return cloneReading(rj.r)
			}
			if p.now().Add(e.slack).Before(e.nextAllowed) {
				r := c.cached(e)
				c.mu.Unlock()
				return r
			}
		}
		inflight, ok := c.calls[key]
		if !ok {
			cl = &call{done: make(chan struct{}), cred: cred}
			c.calls[key] = cl
			continue
		}
		c.mu.Unlock()
		select {
		case <-inflight.done:
		case <-ctx.Done():
			r := fail(src.Provider, StateError, "timeout", fmt.Errorf("waiting for in-flight probe: %w", ctx.Err()))
			r.Account = acct
			return r
		}
		if !refusedOther(inflight.r, inflight.cred, cred) {
			return cloneReading(inflight.r)
		}
		// The provider refused that home's credential, not this one: go
		// round again and measure with this one.
		c.mu.Lock()
	}
	c.mu.Unlock()

	start := p.now()
	r := p.Probe(ctx, src)
	took := p.now().Sub(start)

	c.mu.Lock()
	if c.upstream == nil {
		c.upstream = map[UpstreamKey]uint64{}
	}
	c.upstream[UpstreamKey{Provider: src.Provider, State: r.State, Cause: r.Cause}]++
	e := c.entries[key]
	if e == nil {
		e = &entry{}
		c.entries[key] = e
	}
	now := p.now()
	for k, rj := range e.rejected {
		if !now.Before(rj.until) {
			delete(e.rejected, k)
		}
	}
	var out Reading
	if r.State == StateAuthRequired && cred != "" {
		// Kept against the credential and out of e.last, which the other
		// homes of this account are served from.
		if e.rejected == nil {
			e.rejected = map[string]rejection{}
		}
		e.rejected[cred] = rejection{r: r, until: now.Add(c.minInterval(src.Provider))}
		out = r
	} else {
		delete(e.rejected, cred)
		out = c.record(e, r, src.Provider, now)
	}
	cl.r = out
	delete(c.calls, key)
	c.mu.Unlock()
	close(cl.done)
	if c.OnUpstream != nil {
		lr := cloneReading(r)
		if out.RetryAt != nil {
			lr.RetryAt = out.RetryAt // the effective next attempt, after backoff
		}
		c.OnUpstream(src, lr, took)
	}
	return cloneReading(out)
}

// refusedOther reports whether r is a provider refusing a credential other
// than cred. Unknown credentials (custom providers) count as the same one.
func refusedOther(r Reading, by, cred string) bool {
	return r.State == StateAuthRequired && by != "" && cred != "" && by != cred
}

// record stores a fresh probe result and returns what the caller should see.
func (c *Client) record(e *entry, r Reading, prov Provider, now time.Time) Reading {
	if !r.transient {
		e.failures = 0
		e.last = r
		e.nextAllowed = now.Add(c.minInterval(prov))
		e.slack = minIntervalSlack(c.minInterval(prov))
		if r.State.HasQuota() {
			g := cloneReading(r)
			e.lastGood = &g
		}
		return r
	}
	e.failures++
	e.slack = 0
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
// When the homes of one account disagree, the reading that says most about
// the account is kept (see Merge).
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
//
// Homes of one account can hold different credentials, so their readings can
// differ. The one kept is the one that says most about the account: a
// measurement before a failure, and any other failure before auth_required,
// which only says that one home's credential does not work. Among equals the
// first source wins.
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
			homes := out[j].Homes
			if !contains(homes, where) {
				homes = append(homes, where)
			}
			if mergeRank(r) > mergeRank(out[j]) {
				out[j] = r
			}
			out[j].Homes = homes
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

// mergeRank orders the readings of one account for Merge.
func mergeRank(r Reading) int {
	switch {
	case r.State.HasQuota() && !r.Stale:
		return 3
	case r.State.HasQuota():
		return 2
	case r.State != StateAuthRequired:
		return 1
	}
	return 0
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
