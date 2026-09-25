package ccleft

// Tests pinned to payloads captured live on 2026-09-25 from the tuna-os Hive
// (AWS Talos cluster) during ccleft's first production deployment. See
// testdata/README.md for provenance and redaction.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// agyTokenHome writes the real agy 1.2.x token-file shape (synthetic values)
// into a fresh home, optionally overriding fields.
func agyTokenHome(t *testing.T, mutate func(m map[string]any)) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(fixture(t, "agy_oauth_token.json"), &m); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(m)
	}
	home := t.TempDir()
	writeFile(t, agyTokenPath(Source{Home: home}), m)
	return home
}

func countingAgy(t *testing.T, calls *atomic.Int32) *Prober {
	return &Prober{
		LookPath: func(string) (string, error) { return "/usr/local/bin/agy", nil },
		Exec: func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
			calls.Add(1)
			return fixture(t, "agy_usage_1.2.10.json"), nil
		},
	}
}

// Live finding: agy 1.2.x nests refresh_token under "token" and carries the
// identity in id_token, so ccleft fell back to the HOME PATH as the account
// key: 13 agent homes sharing one Google login (a symlinked ~/.gemini) became
// 13 readings and 13 CLI runs.
func TestAgyDedupesHomesSharingOneLogin(t *testing.T) {
	a := agyTokenHome(t, nil)
	// Same Google account, token refreshed since (new access + id token time).
	b := agyTokenHome(t, func(m map[string]any) {
		m["token"].(map[string]any)["access_token"] = "ya29.OTHER-ACCESS-TOKEN"
	})
	// An agent home whose ~/.gemini is a symlink to a's (the Hive layout).
	c := t.TempDir()
	if err := os.Symlink(filepath.Join(a, ".gemini"), filepath.Join(c, ".gemini")); err != nil {
		t.Fatal(err)
	}
	// A different Google account.
	other := agyTokenHome(t, func(m map[string]any) {
		m["id_token"] = fakeJWT(map[string]any{"sub": "100000000000000000002", "email": "other@example.invalid"})
	})
	var calls atomic.Int32
	cl := NewClient(countingAgy(t, &calls))
	rs := cl.GetAll(context.Background(), []Source{
		{Provider: Agy, Home: a}, {Provider: Agy, Home: b}, {Provider: Agy, Home: c}, {Provider: Agy, Home: other},
	})
	if len(rs) != 2 {
		t.Fatalf("readings = %d, want 2 (one per Google account): %+v", len(rs), rs)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("agy runs = %d, want 2", n)
	}
	var merged Reading
	for _, r := range rs {
		if len(r.Homes) == 3 {
			merged = r
		}
	}
	if merged.Account == "" {
		t.Fatalf("no reading merged the three shared homes: %+v", rs)
	}
}

func TestAgyAccountFallbacks(t *testing.T) {
	// No id_token: the nested refresh token still identifies the account.
	noID := func(m map[string]any) { delete(m, "id_token") }
	a, b := agyTokenHome(t, noID), agyTokenHome(t, noID)
	if agyAccount(agyTokenPath(Source{Home: a}), a) != agyAccount(agyTokenPath(Source{Home: b}), b) {
		t.Fatal("nested refresh_token must identify the account")
	}
	// Legacy top-level refresh_token keeps working.
	h := t.TempDir()
	writeFile(t, agyTokenPath(Source{Home: h}), `{"refresh_token":"1//test"}`)
	if got := agyAccount(agyTokenPath(Source{Home: h}), h); got != fingerprint(Agy, "refresh_token:1//test") {
		t.Fatalf("legacy refresh_token: %s", got)
	}
	// Unreadable token: the home path is the (non-deduping) last resort.
	u := t.TempDir()
	writeFile(t, agyTokenPath(Source{Home: u}), `not json`)
	if got := agyAccount(agyTokenPath(Source{Home: u}), u); got != fingerprint(Agy, "home:"+filepath.Clean(u)) {
		t.Fatalf("fallback: %s", got)
	}
	// The fingerprint never contains token material.
	acct := agyAccount(agyTokenPath(Source{Home: a}), a)
	if len(acct) != 16 || strings.Contains(acct, "SYNTHETIC") {
		t.Fatalf("account %q", acct)
	}
}

func TestAgyLive1210(t *testing.T) {
	r := parseAgyUsage(fixture(t, "agy_usage_1.2.10.json"))
	if r.State != "" && r.State != StateOK || len(r.Windows) != 4 {
		t.Fatalf("got %s %s %+v", r.State, r.Message, r.Windows)
	}
	if st := deriveState(r.Windows); st != StateOK {
		t.Fatalf("state %s", st)
	}
	gw := findWindow(t, r, "gemini-weekly")
	if gw.Scope != "gemini" || gw.Kind != KindWeekly || !approx(*gw.RemainingPct, 25.811567902565) ||
		gw.ResetsAt.Format(time.RFC3339) != "2026-09-30T03:17:08Z" {
		t.Fatalf("gemini-weekly = %+v", gw)
	}
	// Hive's bash probe for the same moment: "google 74% used resets=2026-09-30T03:17:08Z".
	if used := 100 - *gw.RemainingPct; used < 74 || used >= 75 {
		t.Fatalf("used %.2f, bash probe read 74%%", used)
	}
	if w := findWindow(t, r, "3p-5h"); *w.RemainingPct != 100 || w.Kind != KindFiveHour || w.Scope != "3p" {
		t.Fatalf("3p-5h = %+v", w)
	}
}

// Live codex shape (prolite, 2026-09-25): the ONLY window is primary with
// limit_window_seconds=604800 (weekly), secondary null, limit_reached true,
// balance "0" as a string, plus new model_usage / rate_limit_upsell /
// rate_limit_reset_credits objects that must not break decoding.
func TestCodexLiveWeeklyOnlyLimitReached(t *testing.T) {
	var u codexUsage
	if err := json.Unmarshal(fixture(t, "codex_wham_usage_live.json"), &u); err != nil {
		t.Fatal(err)
	}
	r := parseCodexUsage(u, testNow)
	if r.State != StateLimited || r.Plan != "prolite" {
		t.Fatalf("got %s plan=%q", r.State, r.Plan)
	}
	w := findWindow(t, r, "weekly")
	if !w.Binding || *w.RemainingPct != 0 || w.ResetsAt.Unix() != 1790410518 {
		t.Fatalf("weekly = %+v", w)
	}
	// Hive's bash probe: "openai 100% used weekly=100% resets=2026-09-26T08:15:18Z".
	if w.ResetsAt.Format(time.RFC3339) != "2026-09-26T08:15:18Z" {
		t.Fatalf("reset %s", w.ResetsAt)
	}
	for _, x := range r.Windows {
		if x.Kind == KindFiveHour {
			t.Fatalf("no five-hour window in this payload, got %+v", x)
		}
	}
	if c := findWindow(t, r, "credits"); c.Binding || *c.Remaining != 0 {
		t.Fatalf("credits = %+v", c)
	}
}

// Live Copilot shape (individual plan, over its premium quota). remaining is
// the rounded integer (-5); quota_remaining the precise value (-4.2).
func TestCopilotLiveOverQuota(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "copilot_user_live.json")))
	r := testProber(Copilot, u, nil).Probe(context.Background(), Source{Provider: Copilot, Credentials: "gho_TEST"})
	if r.State != StateLimited || r.Plan != "individual" {
		t.Fatalf("got %s plan=%q %s", r.State, r.Plan, r.Message)
	}
	w := findWindow(t, r, "premium_interactions")
	if !approx(*w.Remaining, -4.2) || !approx(*w.Used, 1504.2) || *w.Limit != 1500 || *w.RemainingPct != 0 ||
		w.ResetsAt.Format(time.RFC3339) != "2026-10-01T00:00:00Z" {
		t.Fatalf("premium = %+v", w)
	}
	if len(r.Windows) != 1 || !strings.Contains(r.Message, "chat: unlimited") || !strings.Contains(r.Message, "completions: unlimited") {
		t.Fatalf("unlimited quotas must be notes: %+v %q", r.Windows, r.Message)
	}
}

// Live DeepSeek: a negative topped-up balance and is_available false.
func TestDeepSeekLiveNegativeBalance(t *testing.T) {
	u := newUpstream(t, serveJSON(200, fixture(t, "deepseek_balance_live.json")))
	r := testProber(DeepSeek, u, nil).Probe(context.Background(), Source{Provider: DeepSeek, Credentials: "sk-test"})
	if r.State != StateExhausted || !approx(*findWindow(t, r, "balance:usd").Remaining, -1.23) {
		t.Fatalf("got %s %+v", r.State, r.Windows)
	}
}
