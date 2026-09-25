package ccleft

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// Live finding (tuna-os Hive, 2026-09-25): serve refreshes every 5m with a 5m
// MinInterval. The next cycle started ~0.5 s before the previous probe's
// interval ran out (probes finish at different offsets inside a cycle), so
// codex/agy/kiro were served from cache every OTHER cycle: 10-minute data
// from a 5-minute configuration.
func TestMinIntervalToleratesSchedulingJitter(t *testing.T) {
	clk := &clock{t: testNow}
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		clk.Advance(2 * time.Second) // the probe takes time; nextAllowed counts from its end
		serveJSON(200, fixture(t, "claude_usage.json"))(w, r)
	})
	c := &Client{Prober: testProber(Claude, u, clk), MinInterval: map[Provider]time.Duration{Claude: 5 * time.Minute}}
	src := Source{Provider: Claude, Home: claudeHome(t, "a")}
	start := clk.Now()
	for cycle := 1; cycle <= 4; cycle++ {
		c.Get(context.Background(), src)
		if n := int(u.calls.Load()); n != cycle {
			t.Fatalf("cycle %d: upstream calls = %d — a fixed 5m refresh must probe every cycle", cycle, n)
		}
		clk.t = start.Add(time.Duration(cycle) * 5 * time.Minute) // ticker: fixed period from the first cycle
	}
	// Well inside the interval it is still the cache.
	clk.Advance(time.Minute)
	c.Get(context.Background(), src)
	if n := u.calls.Load(); n != 5 {
		t.Fatalf("calls = %d, want 5 (4 cycles + the one at 20m)", n)
	}
	clk.Advance(3 * time.Minute) // 4m after the last call: > 30 s early
	c.Get(context.Background(), src)
	if n := u.calls.Load(); n != 5 {
		t.Fatalf("calls = %d; the tolerance must stay small", n)
	}
}

// After a 429 the Retry-After is honoured exactly: no early retry.
func TestNoToleranceOnRetryAfter(t *testing.T) {
	clk := &clock{t: testNow}
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "600")
		serveJSON(429, fixture(t, "claude_rate_limited.json"))(w, r)
	})
	c := &Client{Prober: testProber(Claude, u, clk), MinInterval: map[Provider]time.Duration{Claude: 5 * time.Minute}}
	src := Source{Provider: Claude, Home: claudeHome(t, "a")}
	c.Get(context.Background(), src)
	clk.Advance(599 * time.Second)
	c.Get(context.Background(), src)
	if n := u.calls.Load(); n != 1 {
		t.Fatalf("retried %d times before Retry-After elapsed", n-1)
	}
	clk.Advance(time.Second)
	c.Get(context.Background(), src)
	if n := u.calls.Load(); n != 2 {
		t.Fatalf("calls = %d after Retry-After", n)
	}
}
