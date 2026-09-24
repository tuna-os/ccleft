package ccleft

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSingleFlight(t *testing.T) {
	release := make(chan struct{})
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		serveJSON(200, fixture(t, "claude_usage.json"))(w, r)
	})
	c := NewClient(testProber(Claude, u, nil))
	src := Source{Provider: Claude, Home: claudeHome(t, "shared")}
	var wg sync.WaitGroup
	results := make([]Reading, 20)
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i] = c.Get(context.Background(), src) }(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := u.calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
	for _, r := range results {
		if r.State != StateOK || len(r.Windows) == 0 {
			t.Fatalf("caller got %s", r.State)
		}
	}
	// results must not share window slices (callers may mutate)
	results[0].Windows[0].ID = "mutated"
	if results[1].Windows[0].ID == "mutated" {
		t.Fatal("readings share backing arrays")
	}
}

func TestDedupeAcrossHomes(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "claude_usage.json")))
	c := NewClient(testProber(Claude, u, nil))
	// three homes, one account (same accountUuid, different token files)
	a, b, cc := claudeHome(t, "same"), claudeHome(t, "same"), claudeHome(t, "same")
	other := claudeHome(t, "other")
	srcs := []Source{
		{Provider: Claude, Home: a}, {Provider: Claude, Home: b}, {Provider: Claude, Home: cc},
		{Provider: Claude, Home: other},
		{Provider: Claude, Home: t.TempDir()}, // no creds
		{Provider: Claude, Home: t.TempDir()}, // no creds, must stay separate
	}
	rs := c.GetAll(context.Background(), srcs)
	if len(rs) != 4 {
		t.Fatalf("readings = %d, want 4 (1 shared + 1 other + 2 no-creds)", len(rs))
	}
	if n := u.calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one per account)", n)
	}
	var shared *Reading
	for i := range rs {
		if len(rs[i].Homes) == 3 {
			shared = &rs[i]
		}
	}
	if shared == nil {
		t.Fatalf("no reading merged 3 homes: %+v", rs)
	}
	if !contains(shared.Homes, a) || !contains(shared.Homes, cc) {
		t.Fatalf("homes = %v", shared.Homes)
	}
}

func TestLastGoodServedStaleOn429AndBackoff(t *testing.T) {
	var mode atomic.Value
	mode.Store("ok")
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case "ok":
			serveJSON(200, fixture(t, "claude_usage.json"))(w, r)
		case "429":
			w.Header().Set("Retry-After", "600")
			serveJSON(429, fixture(t, "claude_rate_limited.json"))(w, r)
		case "down":
			serveJSON(503, []byte(`upstream connect error`))(w, r)
		}
	})
	clk := &clock{t: testNow}
	c := &Client{Prober: testProber(Claude, u, clk), MinInterval: map[Provider]time.Duration{Claude: time.Minute}, BaseBackoff: 30 * time.Second, MaxBackoff: 10 * time.Minute}
	src := Source{Provider: Claude, Home: claudeHome(t, "a")}

	good := c.Get(context.Background(), src)
	if good.State != StateOK || good.Stale {
		t.Fatalf("first: %s stale=%v", good.State, good.Stale)
	}
	// within MinInterval: served from cache, no call
	clk.Advance(30 * time.Second)
	if r := c.Get(context.Background(), src); r.Stale || u.calls.Load() != 1 {
		t.Fatalf("rate limit not applied: calls=%d", u.calls.Load())
	}

	mode.Store("429")
	clk.Advance(time.Minute)
	r := c.Get(context.Background(), src)
	if !r.Stale || r.State != StateOK || r.Cause != "http_429" || len(r.Windows) != len(good.Windows) {
		t.Fatalf("429 should serve last-good stale: %+v", r)
	}
	if !r.FetchedAt.Equal(good.FetchedAt) {
		t.Fatalf("stale FetchedAt must be the last good measurement time")
	}
	if r.RetryAt == nil || !r.RetryAt.Equal(clk.Now().Add(600*time.Second)) {
		t.Fatalf("Retry-After not honoured: RetryAt=%v", r.RetryAt)
	}
	if !strings.Contains(r.Message, "last-good") || !strings.Contains(r.Message, "rate_limit_error") {
		t.Fatalf("message %q", r.Message)
	}
	calls := u.calls.Load()
	clk.Advance(5 * time.Minute) // still before Retry-After
	if r := c.Get(context.Background(), src); !r.Stale || u.calls.Load() != calls {
		t.Fatal("must not call upstream before Retry-After")
	}

	// 5xx: exponential backoff doubles per consecutive failure (no Retry-After)
	mode.Store("down")
	clk.Advance(10 * time.Minute)
	var waits []time.Duration
	for i := 0; i < 3; i++ {
		before := clk.Now()
		r := c.Get(context.Background(), src)
		if !r.Stale || r.Cause != "http_503" {
			t.Fatalf("5xx: %+v", r)
		}
		waits = append(waits, r.RetryAt.Sub(before))
		clk.Advance(r.RetryAt.Sub(before))
	}
	// failures 2,3,4 in a row (the 429 was failure 1): 60s, 120s, 240s
	if waits[0] != 60*time.Second || waits[1] != 120*time.Second || waits[2] != 240*time.Second {
		t.Fatalf("backoff = %v", waits)
	}

	// recovery resets the failure count and clears staleness
	mode.Store("ok")
	if r := c.Get(context.Background(), src); r.Stale || r.State != StateOK || r.Cause != "" {
		t.Fatalf("recovery: %+v", r)
	}
	if c.backoff(1, 0) != 30*time.Second {
		t.Fatal("base backoff")
	}
	if c.backoff(20, 0) != 10*time.Minute {
		t.Fatal("max backoff cap")
	}
}

func TestNoLastGoodReturnsFailure(t *testing.T) {
	u := newUpstream(t, serveJSON(429, fixture(t, "claude_rate_limited.json")))
	c := NewClient(testProber(Claude, u, nil))
	r := c.Get(context.Background(), Source{Provider: Claude, Home: claudeHome(t, "a")})
	if r.State != StateRateLimited || r.Stale || r.RetryAt == nil {
		t.Fatalf("got %+v", r)
	}
}

func TestExpiredTokenServesLastGood(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "claude_usage.json")))
	clk := &clock{t: testNow}
	c := &Client{Prober: testProber(Claude, u, clk), MinInterval: map[Provider]time.Duration{Claude: time.Minute}}
	home := claudeHome(t, "a") // token valid for 4h
	src := Source{Provider: Claude, Home: home}
	if r := c.Get(context.Background(), src); r.State != StateOK {
		t.Fatal(r.State)
	}
	clk.Advance(5 * time.Hour) // token now expired, refresh token present
	r := c.Get(context.Background(), src)
	if !r.Stale || r.State != StateOK || r.Cause != "token_expired" {
		t.Fatalf("got %+v", r)
	}
}

func TestFlexNumAndRetryAfterAndTime(t *testing.T) {
	var v struct{ A, B, C, D flexNum }
	if err := json.Unmarshal([]byte(`{"A":"12.5","B":3,"C":null,"D":""}`), &v); err != nil {
		t.Fatal(err)
	}
	if !v.A.OK || v.A.V != 12.5 || !v.B.OK || v.B.V != 3 || v.C.OK || v.D.OK {
		t.Fatalf("%+v", v)
	}
	if err := json.Unmarshal([]byte(`{"A":"abc"}`), &v); err == nil {
		t.Fatal("non-numeric string must error")
	}
	if d := ParseRetryAfter("120", testNow); d != 120*time.Second {
		t.Fatal(d)
	}
	if d := ParseRetryAfter(testNow.Add(90*time.Second).Format(http.TimeFormat), testNow); d != 90*time.Second {
		t.Fatal(d)
	}
	if ParseRetryAfter("soon", testNow) != 0 || ParseRetryAfter("", testNow) != 0 {
		t.Fatal("garbage must be 0")
	}
	for in, want := range map[any]int64{"2026-09-27T16:00:00.412354+00:00": 1790524800, 1790812800.0: 1790812800, 1790812800000.0: 1790812800, "1790812800": 1790812800} {
		if got := parseTime(in); got == nil || got.Unix() != want {
			t.Fatalf("parseTime(%v) = %v", in, got)
		}
	}
}

func TestMaskSecrets(t *testing.T) {
	s := snippet([]byte(`{"error":"bad token ksk_ABC123 and Bearer eyJhbGci.x.y, ghp_zzz"}`))
	if strings.Contains(s, "ABC123") || strings.Contains(s, "eyJhbGci") || strings.Contains(s, "ghp_zzz") {
		t.Fatalf("secret leaked: %s", s)
	}
}

func TestWritePrometheus(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "claude_usage.json")))
	r := testProber(Claude, u, nil).Probe(context.Background(), Source{Provider: Claude, Home: claudeHome(t, "a")})
	var buf bytes.Buffer
	if err := WritePrometheus(&buf, []Reading{r}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	acct := r.Account
	for _, want := range []string{
		`ccleft_remaining_ratio{provider="claude",account="` + acct + `",window="five_hour",kind="five_hour",scope="",binding="true"} 0.77`,
		`ccleft_state{provider="claude",account="` + acct + `",state="ok"} 1`,
		`ccleft_state{provider="claude",account="` + acct + `",state="limited"} 0`,
		`ccleft_reset_timestamp_seconds{provider="claude",account="` + acct + `",window="seven_day"} 1790524800`,
		`ccleft_limit{provider="claude",account="` + acct + `",window="extra_usage",kind="credits",scope="",binding="false",unit="usd"} 50`,
		`ccleft_stale{provider="claude",account="` + acct + `"} 0`,
		"# TYPE ccleft_used gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s\n---\n%s", want, out)
		}
	}
}

func TestDetect(t *testing.T) {
	home := claudeHome(t, "a")
	writeCodexAuth(t, filepath.Join(home, ".codex"), testNow)
	writeFile(t, filepath.Join(home, ".gemini", "oauth_creds.json"), `{}`)
	writeFile(t, agyTokenPath(Source{Home: home}), `x`)
	var got []string
	for _, s := range DetectHome(home, nil) {
		got = append(got, string(s.Provider))
		if s.Home != home {
			t.Fatal("home not carried")
		}
	}
	if strings.Join(got, ",") != "claude,codex,agy,gemini" {
		t.Fatalf("detected %v", got)
	}
	if n := len(DetectHome(t.TempDir(), nil)); n != 0 {
		t.Fatalf("empty home detected %d", n)
	}
	env := DetectEnv(map[string]string{"KIRO_API_KEY": "k", "DEEPSEEK_API_KEY": "d", "GH_TOKEN": "g"})
	if len(env) != 2 || env[0].Provider != Kiro || env[1].Provider != DeepSeek {
		t.Fatalf("env detect %+v", env)
	}
}
