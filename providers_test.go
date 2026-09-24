package ccleft

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- Claude

func TestClaudeUsage(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "claude_usage.json")))
	home := claudeHome(t, "acct-1")
	r := testProber(Claude, u, nil).Probe(context.Background(), Source{Provider: Claude, Home: home})

	if r.State != StateOK {
		t.Fatalf("state = %s (%s: %s); an Opus-scoped 100%% window must not limit the account", r.State, r.Cause, r.Message)
	}
	req, _ := u.last()
	if req.URL.Path != "/api/oauth/usage" || req.Header.Get("anthropic-beta") != claudeOAuthBeta ||
		req.Header.Get("Authorization") != "Bearer sk-ant-oat01-TESTTOKEN-acct-1" {
		t.Fatalf("bad request: %s %v", req.URL.Path, req.Header)
	}
	fh := findWindow(t, r, "five_hour")
	if fh.Kind != KindFiveHour || !fh.Binding || !approx(*fh.RemainingPct, 77) {
		t.Fatalf("five_hour = %+v", fh)
	}
	wk := findWindow(t, r, "seven_day")
	if wk.Kind != KindWeekly || !approx(*wk.UsedPct, 61) || wk.ResetsAt == nil || wk.ResetsAt.Format(time.RFC3339) != "2026-09-27T16:00:00Z" {
		t.Fatalf("seven_day = %+v", wk)
	}
	opus := findWindow(t, r, "seven_day_opus")
	if opus.Binding || opus.Scope != "opus" || !opus.Depleted() {
		t.Fatalf("seven_day_opus = %+v", opus)
	}
	son := findWindow(t, r, "seven_day_sonnet")
	if son.Binding || son.Scope != "sonnet" {
		t.Fatalf("seven_day_sonnet = %+v", son)
	}
	ex := findWindow(t, r, "extra_usage")
	if ex.Kind != KindCredits || ex.Binding || !approx(*ex.Limit, 50) || !approx(*ex.Used, 12.34) || ex.Unit != UnitUSD {
		t.Fatalf("extra_usage = %+v", ex)
	}
	if r.Plan != "max" || len(r.Account) != 16 || strings.Contains(r.Account, "acct") {
		t.Fatalf("plan/account = %q/%q", r.Plan, r.Account)
	}
	if b, ok := r.Binding(); !ok || b.ID != "seven_day" {
		t.Fatalf("binding window = %+v", b)
	}
}

func TestClaudeTopLevelOnlyAndLimited(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "claude_usage_toplevel_only.json")))
	r := testProber(Claude, u, nil).Probe(context.Background(), Source{Provider: Claude, Home: claudeHome(t, "a")})
	if r.State != StateLimited {
		t.Fatalf("state = %s, want limited (five_hour at 100%%)", r.State)
	}
	// "47.5" arrives as a string: schema drift must not break parsing.
	if w := findWindow(t, r, "seven_day"); !approx(*w.UsedPct, 47.5) {
		t.Fatalf("seven_day = %+v", w)
	}
	for _, w := range r.Windows {
		if w.ID == "extra_usage" {
			t.Fatal("disabled extra_usage must not produce a window")
		}
	}
}

func TestClaudeRateLimitedHonoursRetryAfter(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		serveJSON(429, fixture(t, "claude_rate_limited.json"))(w, r)
	})
	r := testProber(Claude, u, nil).Probe(context.Background(), Source{Provider: Claude, Home: claudeHome(t, "a")})
	if r.State != StateRateLimited || r.Cause != "http_429" || !r.Transient() {
		t.Fatalf("got %s/%s transient=%v", r.State, r.Cause, r.Transient())
	}
	if r.RetryAt == nil || !r.RetryAt.Equal(testNow.Add(120*time.Second)) {
		t.Fatalf("RetryAt = %v", r.RetryAt)
	}
	if !strings.Contains(r.Message, "rate_limit_error") {
		t.Fatalf("upstream error text must be preserved, got %q", r.Message)
	}
	var pe *ProbeError
	if !errors.As(r.Err(), &pe) || pe.State != StateRateLimited {
		t.Fatalf("Err() = %v", r.Err())
	}
}

func TestClaudeExpiredTokenNoNetwork(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "claude_usage.json")))
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": "tok", "refreshToken": "ref", "expiresAt": testNow.Add(-time.Minute).UnixMilli()},
	})
	r := testProber(Claude, u, nil).Probe(context.Background(), Source{Provider: Claude, Home: home})
	if r.State != StateAuthRequired || r.Cause != "token_expired" || !r.Transient() {
		t.Fatalf("got %s/%s transient=%v", r.State, r.Cause, r.Transient())
	}
	if u.calls.Load() != 0 {
		t.Fatal("an expired token must not be sent upstream")
	}

	// no refresh token: conclusive, not transient
	writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": "tok", "expiresAt": testNow.Add(-time.Minute).UnixMilli()},
	})
	r = testProber(Claude, u, nil).Probe(context.Background(), Source{Provider: Claude, Home: home})
	if r.Cause != "login_expired" || r.Transient() {
		t.Fatalf("got %s transient=%v", r.Cause, r.Transient())
	}
}

func TestClaudeAuthStates(t *testing.T) {
	u := newUpstream(t, serveJSON(401, []byte(`{"type":"error","error":{"type":"authentication_error","message":"Invalid bearer token"}}`)))
	p := testProber(Claude, u, nil)

	// missing file
	r := p.Probe(context.Background(), Source{Provider: Claude, Home: t.TempDir()})
	if r.State != StateAuthRequired || r.Cause != "no_credentials" || !errors.Is(r.Err(), ErrNoCredentials) {
		t.Fatalf("missing: %s/%s %v", r.State, r.Cause, r.Err())
	}
	// zeroed file (Claude Code does this after a failed refresh)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`)
	r = p.Probe(context.Background(), Source{Provider: Claude, Home: home})
	if r.State != StateAuthRequired || r.Cause != "no_credentials" {
		t.Fatalf("zeroed: %s/%s", r.State, r.Cause)
	}
	// 401 upstream
	r = p.Probe(context.Background(), Source{Provider: Claude, Home: claudeHome(t, "a")})
	if r.State != StateAuthRequired || r.Cause != "http_401" || r.Transient() {
		t.Fatalf("401: %s/%s", r.State, r.Cause)
	}
}

func TestClaudeSchemaDriftIsAnErrorNotHeadroom(t *testing.T) {
	for _, body := range []string{`{"foo":{"bar":1}}`, `{"limits":[]}`, `{"five_hour":{"resets_at":"2026-09-24T22:00:00Z"}}`, `not json`} {
		u := newUpstream(t, serveJSON(200, []byte(body)))
		r := testProber(Claude, u, nil).Probe(context.Background(), Source{Provider: Claude, Home: claudeHome(t, "a")})
		if r.State != StateError || r.Cause != "schema" || r.Message == "" {
			t.Fatalf("%s: got %s/%s %q", body, r.State, r.Cause, r.Message)
		}
	}
}

func TestClaudeConfigDirEnv(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "claude_usage.json")))
	src := claudeHome(t, "a")
	custom := t.TempDir()
	b, _ := os.ReadFile(filepath.Join(src, ".claude", ".credentials.json"))
	writeFile(t, filepath.Join(custom, ".credentials.json"), b)
	r := testProber(Claude, u, nil).Probe(context.Background(), Source{Provider: Claude, Home: t.TempDir(), Env: map[string]string{"CLAUDE_CONFIG_DIR": custom}})
	if r.State != StateOK {
		t.Fatalf("CLAUDE_CONFIG_DIR not honoured: %s %s", r.State, r.Message)
	}
}

// Read-only guarantee: probing must not touch anything under the home.
func TestProbeNeverWritesCredentials(t *testing.T) {
	u := newUpstream(t, serveJSON(401, []byte(`{}`)))
	home := claudeHome(t, "a")
	writeFile(t, filepath.Join(home, ".codex", "auth.json"), map[string]any{"tokens": map[string]any{"access_token": fakeJWT(map[string]any{"exp": testNow.Add(time.Hour).Unix()}), "account_id": "x"}})
	snap := snapshotTree(t, home)
	p := &Prober{Endpoints: map[Provider]string{Claude: u.URL, Codex: u.URL}, Now: func() time.Time { return testNow }}
	for _, pr := range []Provider{Claude, Codex} {
		_ = p.Probe(context.Background(), Source{Provider: pr, Home: home})
	}
	if got := snapshotTree(t, home); got != snap {
		t.Fatalf("home tree changed:\nbefore %s\nafter  %s", snap, got)
	}
}

func snapshotTree(t *testing.T, root string) string {
	var b strings.Builder
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		c, _ := os.ReadFile(p)
		b.WriteString(p + "|" + info.Mode().String() + "|" + info.ModTime().String() + "|" + string(c) + "\n")
		return nil
	})
	return b.String()
}

// ---------------------------------------------------------------- Codex

func writeCodexAuth(t *testing.T, dir string, exp time.Time) {
	writeFile(t, filepath.Join(dir, "auth.json"), map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":     fakeJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "prolite"}}),
			"access_token": fakeJWT(map[string]any{"exp": exp.Unix(), "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "00000000-0000-4000-8000-000000000001"}}),
			"account_id":   "00000000-0000-4000-8000-000000000001",
		},
	})
}

func TestCodexUsageLimitedStringBalance(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "codex_wham_usage.json")))
	dir := t.TempDir()
	writeCodexAuth(t, dir, testNow.Add(time.Hour))
	// CODEX_HOME honoured; Home has no .codex at all.
	r := testProber(Codex, u, nil).Probe(context.Background(), Source{Provider: Codex, Home: t.TempDir(), Env: map[string]string{"CODEX_HOME": dir}})
	if r.State != StateLimited {
		t.Fatalf("state = %s (%s)", r.State, r.Message)
	}
	req, _ := u.last()
	if req.URL.Path != "/wham/usage" || req.Header.Get("ChatGPT-Account-Id") != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("bad request %s %v", req.URL.Path, req.Header)
	}
	if w := findWindow(t, r, "five_hour"); !approx(*w.RemainingPct, 77) || w.ResetsAt.Unix() != 1790380800 {
		t.Fatalf("five_hour = %+v", w)
	}
	if w := findWindow(t, r, "weekly"); !w.Depleted() || !w.Binding {
		t.Fatalf("weekly = %+v", w)
	}
	if w := findWindow(t, r, "code_review:weekly"); w.Binding || w.Scope != "code_review" {
		t.Fatalf("code review = %+v", w)
	}
	if w := findWindow(t, r, "credits"); *w.Remaining != 0 || w.Binding {
		t.Fatalf("credits (string \"0\") = %+v", w)
	}
	if r.Plan != "prolite" {
		t.Fatalf("plan %q", r.Plan)
	}
}

func TestCodexNumericBalanceOK(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "codex_wham_usage_numeric.json")))
	home := t.TempDir()
	writeCodexAuth(t, filepath.Join(home, ".codex"), testNow.Add(time.Hour))
	r := testProber(Codex, u, nil).Probe(context.Background(), Source{Provider: Codex, Home: home})
	if r.State != StateOK || !approx(*findWindow(t, r, "credits").Remaining, 125.5) || r.Plan != "pro" {
		t.Fatalf("got %+v", r)
	}
}

func TestCodexAuthStates(t *testing.T) {
	u := newUpstream(t, serveJSON(401, []byte(`{"detail":"token expired"}`)))
	p := testProber(Codex, u, nil)
	home := t.TempDir()

	writeCodexAuth(t, filepath.Join(home, ".codex"), testNow.Add(-time.Minute))
	r := p.Probe(context.Background(), Source{Provider: Codex, Home: home})
	if r.State != StateAuthRequired || r.Cause != "token_expired" || !r.Transient() || u.calls.Load() != 0 {
		t.Fatalf("expired: %s/%s calls=%d", r.State, r.Cause, u.calls.Load())
	}

	writeCodexAuth(t, filepath.Join(home, ".codex"), testNow.Add(time.Hour))
	r = p.Probe(context.Background(), Source{Provider: Codex, Home: home})
	if r.State != StateAuthRequired || r.Cause != "http_401" {
		t.Fatalf("401: %s/%s", r.State, r.Cause)
	}

	writeFile(t, filepath.Join(home, ".codex", "auth.json"), `{"OPENAI_API_KEY":"sk-proj-TEST","tokens":null}`)
	r = p.Probe(context.Background(), Source{Provider: Codex, Home: home})
	if r.State != StateUnsupported || r.Cause != "api_key_login" || r.Account == "" {
		t.Fatalf("api key: %s/%s", r.State, r.Cause)
	}
}

// ---------------------------------------------------------------- Agy

func TestAgyRunsWithSourceHomeAndBoundedEnv(t *testing.T) {
	home := t.TempDir()
	writeFile(t, agyTokenPath(Source{Home: home}), `{"refresh_token":"1//test"}`)
	var gotEnv, gotArgs []string
	p := &Prober{
		LookPath: func(string) (string, error) { return "/opt/agy/bin/agy", nil },
		Exec: func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
			gotEnv, gotArgs = env, append([]string{name}, args...)
			if _, ok := ctx.Deadline(); !ok {
				t.Error("agy must run under a deadline")
			}
			return append([]byte("banner line\n"), fixture(t, "agy_usage.json")...), nil
		},
	}
	t.Setenv("CCLEFT_SHOULD_NOT_LEAK", "1")
	r := p.Probe(context.Background(), Source{Provider: Agy, Home: home, Env: map[string]string{"PATH": "/usr/bin"}})
	if r.State != StateOK || len(r.Windows) != 4 {
		t.Fatalf("got %s %s %+v", r.State, r.Message, r.Windows)
	}
	if strings.Join(gotArgs, " ") != "/opt/agy/bin/agy --print /usage --output-format json" {
		t.Fatalf("args %v", gotArgs)
	}
	env := strings.Join(gotEnv, "\n")
	if !strings.Contains(env, "HOME="+home+"\n") || strings.Contains(env, "CCLEFT_SHOULD_NOT_LEAK") || !strings.Contains(env, "PATH=/opt/agy/bin:/usr/bin") {
		t.Fatalf("env = %v", gotEnv)
	}
	w := findWindow(t, r, "3p-weekly")
	if w.Scope != "3p" || w.Kind != KindWeekly || !approx(*w.RemainingPct, 9.068053215742111) {
		t.Fatalf("3p-weekly = %+v", w)
	}
	if DefaultTimeouts[Agy] < 60*time.Second {
		t.Fatal("agy timeout must stay generous (the CLI boots slowly)")
	}
}

func TestAgyLoginPromptAndTimeoutAndMissing(t *testing.T) {
	home := t.TempDir()
	writeFile(t, agyTokenPath(Source{Home: home}), `x`)
	look := func(string) (string, error) { return "/bin/agy", nil }

	p := &Prober{LookPath: look, Exec: func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
		return fixture(t, "agy_login.txt"), errors.New("exit status 1")
	}}
	if r := p.Probe(context.Background(), Source{Provider: Agy, Home: home}); r.State != StateAuthRequired || r.Cause != "login_required" {
		t.Fatalf("login: %s/%s", r.State, r.Cause)
	}

	p = &Prober{LookPath: look, Timeouts: map[Provider]time.Duration{Agy: 30 * time.Millisecond},
		Exec: func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
	if r := p.Probe(context.Background(), Source{Provider: Agy, Home: home}); r.State != StateError || r.Cause != "timeout" || !r.Transient() {
		t.Fatalf("timeout: %s/%s", r.State, r.Cause)
	}

	p = &Prober{LookPath: func(string) (string, error) { return "", exec.ErrNotFound }}
	if r := p.Probe(context.Background(), Source{Provider: Agy, Home: home}); r.Cause != "not_installed" {
		t.Fatalf("missing: %s", r.Cause)
	}

	// no token file: never run the CLI
	ran := false
	p = &Prober{LookPath: look, Exec: func(context.Context, []string, string, ...string) ([]byte, error) { ran = true; return nil, nil }}
	if r := p.Probe(context.Background(), Source{Provider: Agy, Home: t.TempDir()}); r.State != StateAuthRequired || ran {
		t.Fatalf("no creds: %s ran=%v", r.State, ran)
	}
}

func TestAgyNonSuccessEnvelope(t *testing.T) {
	r := parseAgyUsage([]byte(`{"status":"ERROR","response":"Please sign in to continue"}`))
	if r.State != StateAuthRequired {
		t.Fatalf("got %s", r.State)
	}
	r = parseAgyUsage([]byte(`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"buckets":[{"id":"x-weekly","window":"weekly"}]}]}}}`))
	if r.Cause != "schema" {
		t.Fatalf("bucket without remaining_fraction must be schema error, got %s", r.Cause)
	}
}

// ---------------------------------------------------------------- Gemini / Muse

func TestUnsupportedProviders(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".gemini", "oauth_creds.json"), `{"refresh_token":"1//x"}`)
	r := Probe(context.Background(), Source{Provider: Gemini, Home: home})
	if r.State != StateUnsupported || r.Cause != "api_shutdown" || !strings.Contains(r.Message, "UNSUPPORTED_CLIENT") || r.Account == "" {
		t.Fatalf("gemini: %+v", r)
	}
	r = Probe(context.Background(), Source{Provider: Muse, Env: map[string]string{"META_API_KEY": "mk-test"}})
	if r.State != StateUnsupported || r.Cause != "api_key_login" {
		t.Fatalf("muse: %+v", r)
	}
	r = Probe(context.Background(), Source{Provider: Muse})
	if r.State != StateAuthRequired {
		t.Fatalf("muse no key: %+v", r)
	}
}

// ---------------------------------------------------------------- Kiro

func TestKiroUsage(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "kiro_usage.json")))
	r := testProber(Kiro, u, nil).Probe(context.Background(), Source{Provider: Kiro, Env: map[string]string{"KIRO_API_KEY": " ksk_TESTKEY \n"}})
	if r.State != StateOK || r.Plan != "KIRO POWER" {
		t.Fatalf("got %s %q %s", r.State, r.Plan, r.Message)
	}
	req, body := u.last()
	if req.Method != "POST" || req.Header.Get("X-Amz-Target") != "AmazonCodeWhispererService.GetUsageLimits" ||
		req.Header.Get("tokentype") != "API_KEY" || req.Header.Get("Content-Type") != "application/x-amz-json-1.0" ||
		req.Header.Get("Authorization") != "Bearer ksk_TESTKEY" {
		t.Fatalf("headers %v", req.Header)
	}
	if !bytes.Contains(body, []byte(`"resourceType":"AGENTIC_REQUEST"`)) || !bytes.Contains(body, []byte(`"origin":"AI_EDITOR"`)) {
		t.Fatalf("body %s", body)
	}
	w := findWindow(t, r, "plan")
	if w.Kind != KindMonthly || !approx(*w.Used, 289.22) || !approx(*w.Limit, 10000) || w.ResetsAt.Unix() != 1790812800 || w.Unit != UnitCredits {
		t.Fatalf("plan = %+v", w)
	}
	for _, w := range r.Windows {
		if w.ID == "overage" {
			t.Fatal("overage window must not appear when overage is DISABLED")
		}
	}
}

func TestKiroBonusAndOverage(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "kiro_usage_bonus_overage.json")))
	r := testProber(Kiro, u, nil).Probe(context.Background(), Source{Provider: Kiro, Credentials: "ksk_x"})
	plan := findWindow(t, r, "plan")
	// plan usage = currentUsage − currentOverages = 1000 → depleted
	if !approx(*plan.Used, 1000) || !plan.Depleted() {
		t.Fatalf("plan = %+v", plan)
	}
	bonus := findWindow(t, r, "bonus:welcome500")
	if bonus.Binding || !bonus.Depleted() || bonus.ResetsAt.Unix() != 1791417600 {
		t.Fatalf("bonus = %+v", bonus)
	}
	ov := findWindow(t, r, "overage")
	if !approx(*ov.Used, 120.5) || !approx(*ov.Limit, 500) {
		t.Fatalf("overage = %+v", ov)
	}
	// plan + bonus used, but enabled overage still has 379.5 credits
	if r.State != StateOK {
		t.Fatalf("state = %s", r.State)
	}
	for _, w := range r.Windows {
		if w.ID == "bonus:old" {
			t.Fatal("expired bonus must be skipped")
		}
	}
}

func TestKiroLimitedAndAuth(t *testing.T) {
	kr := kiroResp{UsageBreakdownList: []kiroCredit{{UsageLimit: flexNum{1000, true}, CurrentUsage: flexNum{1000, true}}}}
	if r := parseKiroUsage(kr); r.State != StateLimited {
		t.Fatalf("got %s", r.State)
	}
	u := newUpstream(t, serveJSON(400, fixture(t, "kiro_access_denied.json")))
	r := testProber(Kiro, u, nil).Probe(context.Background(), Source{Provider: Kiro, Credentials: "ksk_bad"})
	if r.State != StateAuthRequired || !strings.Contains(r.Message, "AccessDeniedException") {
		t.Fatalf("got %s %s", r.State, r.Message)
	}
	if r := Probe(context.Background(), Source{Provider: Kiro}); r.Cause != "no_credentials" {
		t.Fatalf("no key: %s", r.Cause)
	}
}

// ---------------------------------------------------------------- Copilot

func TestCopilotLimitedWhenRemainingZero(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "copilot_user.json")))
	r := testProber(Copilot, u, nil).Probe(context.Background(), Source{Provider: Copilot, Env: map[string]string{"GH_TOKEN": "gho_TEST"}})
	if r.State != StateLimited {
		t.Fatalf("state = %s, want limited", r.State)
	}
	req, _ := u.last()
	if req.URL.Path != "/copilot_internal/user" || req.Header.Get("Authorization") != "token gho_TEST" {
		t.Fatalf("request %s %v", req.URL.Path, req.Header)
	}
	w := findWindow(t, r, "premium_interactions")
	if w.Kind != KindMonthly || *w.Limit != 300 || *w.Remaining != 0 || w.ResetsAt.Format("2006-01-02") != "2026-10-01" {
		t.Fatalf("premium = %+v", w)
	}
	if len(r.Windows) != 1 || !strings.Contains(r.Message, "chat: unlimited") || r.Plan != "individual_pro" {
		t.Fatalf("unlimited quotas should be notes, not windows: %+v %q", r.Windows, r.Message)
	}
}

func TestCopilotFreeTierAndTokenFiles(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "copilot_user_free.json")))
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".config", "gh", "hosts.yml"), "github.com:\n    oauth_token: gho_FROMHOSTS\n    user: example\n    git_protocol: https\n")
	r := testProber(Copilot, u, nil).Probe(context.Background(), Source{Provider: Copilot, Home: home})
	if r.State != StateOK {
		t.Fatalf("state %s %s", r.State, r.Message)
	}
	if req, _ := u.last(); req.Header.Get("Authorization") != "token gho_FROMHOSTS" {
		t.Fatal("hosts.yml token not used")
	}
	w := findWindow(t, r, "completions")
	if *w.Used != 267 || *w.Limit != 2000 || w.ResetsAt.Format("2006-01-02") != "2026-10-12" {
		t.Fatalf("completions = %+v", w)
	}

	// Copilot editor token takes precedence over gh.
	writeFile(t, filepath.Join(home, ".config", "github-copilot", "apps.json"), `{"github.com:Iv1.test":{"user":"x","oauth_token":"ghu_FROMAPPS"}}`)
	_ = testProber(Copilot, u, nil).Probe(context.Background(), Source{Provider: Copilot, Home: home})
	if req, _ := u.last(); req.Header.Get("Authorization") != "token ghu_FROMAPPS" {
		t.Fatal("apps.json token not preferred")
	}
}

func TestCopilotNoEntitlement(t *testing.T) {
	u := newUpstream(t, serveJSON(404, []byte(`{"message":"Not Found"}`)))
	r := testProber(Copilot, u, nil).Probe(context.Background(), Source{Provider: Copilot, Credentials: "gho_x"})
	if r.State != StateUnsupported || r.Cause != "no_copilot" {
		t.Fatalf("got %s/%s", r.State, r.Cause)
	}
}

// ---------------------------------------------------------------- DeepSeek

func TestDeepSeek(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "deepseek_balance.json")))
	r := testProber(DeepSeek, u, nil).Probe(context.Background(), Source{Provider: DeepSeek, Env: map[string]string{"DEEPSEEK_API_KEY": "sk-test"}})
	if r.State != StateOK || !approx(*findWindow(t, r, "balance:usd").Remaining, 12.47) {
		t.Fatalf("got %+v", r)
	}
	u2 := newUpstream(t, serveJSON(200, fixture(t, "deepseek_balance_empty.json")))
	r = testProber(DeepSeek, u2, nil).Probe(context.Background(), Source{Provider: DeepSeek, Credentials: "sk-test"})
	if r.State != StateExhausted {
		t.Fatalf("empty balance: %s", r.State)
	}
}
