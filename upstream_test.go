package ccleft

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Every upstream call is counted (and reported to OnUpstream) exactly once;
// answers served from the rate limit, backoff or cache are not.
func TestUpstreamCountsAndHook(t *testing.T) {
	var mode atomic.Value
	mode.Store("ok")
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == "429" {
			serveJSON(429, fixture(t, "claude_rate_limited.json"))(w, r)
			return
		}
		serveJSON(200, fixture(t, "claude_usage.json"))(w, r)
	})
	clk := &clock{t: testNow}
	var hooked []Reading
	c := &Client{Prober: testProber(Claude, u, clk), MinInterval: map[Provider]time.Duration{Claude: 5 * time.Minute}}
	c.OnUpstream = func(src Source, r Reading, d time.Duration) { hooked = append(hooked, r) }
	src := Source{Provider: Claude, Home: claudeHome(t, "a")}

	c.Get(context.Background(), src) // upstream: ok
	clk.Advance(time.Minute)
	c.Get(context.Background(), src) // cached (min interval)
	mode.Store("429")
	clk.Advance(5 * time.Minute)
	r := c.Get(context.Background(), src) // upstream: 429 → stale last-good
	if !r.Stale || r.State != StateOK {
		t.Fatalf("429 must serve last-good stale: %s stale=%v", r.State, r.Stale)
	}
	clk.Advance(time.Minute)
	c.Get(context.Background(), src) // inside backoff: no call

	if n := u.calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2", n)
	}
	got := c.UpstreamCounts()
	want := map[UpstreamKey]uint64{
		{Provider: Claude, State: StateOK}:                             1,
		{Provider: Claude, State: StateRateLimited, Cause: "http_429"}: 1,
	}
	if len(got) != len(want) {
		t.Fatalf("counts = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("counts[%v] = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	if len(hooked) != 2 || hooked[1].State != StateRateLimited || hooked[1].Stale || hooked[1].RetryAt == nil {
		t.Fatalf("hook must see each raw upstream result once, with the effective retry time: %+v", hooked)
	}

	var buf bytes.Buffer
	if err := WriteUpstreamMetrics(&buf, got); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"# TYPE ccleft_upstream_requests_total counter",
		`ccleft_upstream_requests_total{provider="claude",state="ok",cause=""} 1`,
		`ccleft_upstream_requests_total{provider="claude",state="rate_limited",cause="http_429"} 1`,
	} {
		if !strings.Contains(buf.String(), line+"\n") {
			t.Fatalf("missing %q in:\n%s", line, buf.String())
		}
	}
}

// Terminal local answers (no credentials) never reach upstream or the counter.
func TestUpstreamCountsIgnoreLocalAnswers(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "claude_usage.json")))
	c := NewClient(testProber(Claude, u, nil))
	c.Get(context.Background(), Source{Provider: Claude, Home: t.TempDir()})
	if len(c.UpstreamCounts()) != 0 || u.calls.Load() != 0 {
		t.Fatalf("local answer counted: %v", c.UpstreamCounts())
	}
}
