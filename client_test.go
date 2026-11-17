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

// Homes of one account can hold different tokens, and the account identity is
// read from each home's own files. A home whose login is gone must not turn
// the account into auth_required for the homes that can still be measured.
func TestDeadLoginInOneHomeDoesNotHideTheAccount(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "claude_usage.json")))
	c := NewClient(testProber(Claude, u, nil))
	dead, live := claudeHome(t, "same"), claudeHome(t, "same")
	// Claude Code zeroes the token when a refresh fails; .claude.json still
	// names the account.
	writeFile(t, filepath.Join(dead, ".claude", ".credentials.json"), map[string]any{"claudeAiOauth": map[string]any{}})
	rs := c.GetAll(context.Background(), []Source{{Provider: Claude, Home: dead}, {Provider: Claude, Home: live}})
	if len(rs) != 1 {
		t.Fatalf("readings = %d, want 1 for the shared account: %+v", len(rs), rs)
	}
	if r := rs[0]; r.State != StateOK || len(r.Windows) == 0 || len(r.Homes) != 2 {
		t.Fatalf("account reads %s (%s) with %d windows for homes %v; the first home's dead login replaced the measurement", r.State, r.Cause, len(r.Windows), r.Homes)
	}
}

func TestRefusedTokenIsNotCachedForOtherHomes(t *testing.T) {
	usage := fixture(t, "claude_usage.json")
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer junk" {
			serveJSON(401, []byte(`{"error":"invalid token"}`))(w, r)
			return
		}
		serveJSON(200, usage)(w, r)
	})
	c := NewClient(testProber(Claude, u, nil))
	junk, live := claudeHome(t, "same"), claudeHome(t, "same")
	writeFile(t, filepath.Join(junk, ".claude", ".credentials.json"), map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": "junk", "refreshToken": "junk"},
	})
	ctx := context.Background()
	if r := c.Get(ctx, Source{Provider: Claude, Home: junk}); r.State != StateAuthRequired {
		t.Fatalf("junk token: state %s, want auth_required", r.State)
	}
	if r := c.Get(ctx, Source{Provider: Claude, Home: live}); r.State != StateOK {
		t.Fatalf("the home with a working token was answered %s (%s) from the other home's refusal", r.State, r.Cause)
	}
	if n := u.calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one per token)", n)
	}
	// The refused token is not tried again inside the interval.
	if r := c.Get(ctx, Source{Provider: Claude, Home: junk}); r.State != StateAuthRequired {
		t.Fatalf("junk token again: state %s, want the cached auth_required", r.State)
	}
	if n := u.calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2: the refused token was retried inside the interval", n)
	}
	rs := c.GetAll(ctx, []Source{{Provider: Claude, Home: junk}, {Provider: Claude, Home: live}})
	if len(rs) != 1 || rs[0].State != StateOK || len(rs[0].Homes) != 2 {
		t.Fatalf("merged = %+v, want one ok reading for both homes", rs)
	}
}

// Both homes ask at once: the one with the working token waits for the other
// home's call, and must not take its refusal as the answer.
func TestRefusedTokenInFlightIsNotSharedWithOtherHomes(t *testing.T) {
	usage := fixture(t, "claude_usage.json")
	release := make(chan struct{})
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer junk" {
			<-release
			serveJSON(401, []byte(`{"error":"invalid token"}`))(w, r)
			return
		}
		serveJSON(200, usage)(w, r)
	})
	c := NewClient(testProber(Claude, u, nil))
	junk, live := claudeHome(t, "same"), claudeHome(t, "same")
	writeFile(t, filepath.Join(junk, ".claude", ".credentials.json"), map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": "junk", "refreshToken": "junk"},
	})
	ctx := context.Background()
	first := make(chan Reading, 1)
	go func() { first <- c.Get(ctx, Source{Provider: Claude, Home: junk}) }()
	for u.calls.Load() == 0 { // the junk call is in flight
		time.Sleep(time.Millisecond)
	}
	second := make(chan Reading, 1)
	go func() { second <- c.Get(ctx, Source{Provider: Claude, Home: live}) }()
	time.Sleep(20 * time.Millisecond) // let it reach the in-flight call
	close(release)
	if r := <-first; r.State != StateAuthRequired {
		t.Fatalf("junk token: state %s, want auth_required", r.State)
	}
	if r := <-second; r.State != StateOK {
		t.Fatalf("the home with a working token was answered %s (%s) from the other home's in-flight refusal", r.State, r.Cause)
	}
}

// agy has no token to send: it runs with the home, so the token file is the
// credential.
func TestAgyLoginPromptInOneHomeIsNotCachedForOtherHomes(t *testing.T) {
	stale := agyTokenHome(t, func(m map[string]any) { m["token"] = map[string]any{"refresh_token": "1//revoked"} })
	live := agyTokenHome(t, nil)
	p := &Prober{
		LookPath: func(string) (string, error) { return "/usr/local/bin/agy", nil },
		Exec: func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
			if strings.Contains(strings.Join(env, "\n"), "HOME="+stale+"\n") {
				return []byte("Please sign in to continue"), nil
			}
			return fixture(t, "agy_usage_1.2.10.json"), nil
		},
	}
	c := NewClient(p)
	ctx := context.Background()
	a, b := c.Get(ctx, Source{Provider: Agy, Home: stale}), c.Get(ctx, Source{Provider: Agy, Home: live})
	if a.Account != b.Account {
		t.Fatalf("accounts %q and %q: the homes must share one Google account for this test", a.Account, b.Account)
	}
	if a.State != StateAuthRequired || !b.State.HasQuota() {
		t.Fatalf("states = %s, %s; want auth_required for the home agy asks to log in and a measurement for the other", a.State, b.State)
	}
}

// A token that cannot be sent at all never reaches the provider, so it would
// read as a network error, which is shared by every home of the account and
// backed off. It is a bad credential like any other.
func TestUnsendableTokenIsABadCredentialNotAnEndpointError(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "claude_usage.json")))
	c := NewClient(testProber(Claude, u, nil))
	ctx := context.Background()
	for name, token := range map[string]string{
		"line break": "junk\r\nX-Injected: 1",
		"oversized":  strings.Repeat("a", maxHeaderValue),
	} {
		bad, live := claudeHome(t, name), claudeHome(t, name)
		writeFile(t, filepath.Join(bad, ".claude", ".credentials.json"), map[string]any{
			"claudeAiOauth": map[string]any{"accessToken": token, "refreshToken": "junk"},
		})
		before := u.calls.Load()
		if r := c.Get(ctx, Source{Provider: Claude, Home: bad}); r.State != StateAuthRequired || r.Cause != "invalid_credentials" {
			t.Fatalf("%s token: got %s/%s, want auth_required/invalid_credentials", name, r.State, r.Cause)
		}
		if n := u.calls.Load() - before; n != 0 {
			t.Fatalf("%s token: %d request(s) were sent with it", name, n)
		}
		if r := c.Get(ctx, Source{Provider: Claude, Home: live}); r.State != StateOK {
			t.Fatalf("%s token: the home with a working token was answered %s (%s)", name, r.State, r.Cause)
		}
	}
}
