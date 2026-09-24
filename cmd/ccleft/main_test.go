package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuna-os/ccleft"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func claudeHome(t *testing.T, acct string) string {
	home := t.TempDir()
	exp := time.Now().Add(time.Hour).UnixMilli()
	mustWrite(t, filepath.Join(home, ".claude", ".credentials.json"),
		`{"claudeAiOauth":{"accessToken":"tok-`+acct+`","refreshToken":"ref","expiresAt":`+jsonNum(exp)+`,"subscriptionType":"pro"}}`)
	mustWrite(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"accountUuid":"`+acct+`","organizationUuid":"o"}}`)
	return home
}

func jsonNum(v int64) string { b, _ := json.Marshal(v); return string(b) }

func TestProbeJSONDedupesHomes(t *testing.T) {
	body, _ := os.ReadFile("../../testdata/claude_usage.json")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	h1, h2 := claudeHome(t, "same"), claudeHome(t, "same")
	var out bytes.Buffer
	err := runProbe([]string{"--json", "--no-env", "--home", h1, "--home", h2, "--provider", "claude", "--endpoint", "claude=" + srv.URL}, &out)
	if err != nil {
		t.Fatal(err)
	}
	var o Output
	if err := json.Unmarshal(out.Bytes(), &o); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(o.Readings) != 1 || len(o.Readings[0].Homes) != 2 || o.Readings[0].State != ccleft.StateOK {
		t.Fatalf("readings: %+v", o.Readings)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
	if strings.Contains(out.String(), "tok-same") {
		t.Fatal("token leaked into output")
	}
}

func TestProbeTableAndConfigSources(t *testing.T) {
	body, _ := os.ReadFile("../../testdata/kiro_usage.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ksk_team2" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"__type":"AccessDeniedException"}`))
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	t.Setenv("CCLEFT_TEST_KIRO_TEAM2", "ksk_team2")
	cfg := filepath.Join(t.TempDir(), "ccleft.yaml")
	mustWrite(t, cfg, `
env_providers: false
sources:
  - provider: kiro
    label: team-2
    credentials_env: CCLEFT_TEST_KIRO_TEAM2
`)
	var out bytes.Buffer
	if err := runProbe([]string{"--config", cfg, "--endpoint", "kiro=" + srv.URL}, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"kiro", "ok", "KIRO POWER", "plan", "9710.78/10000 credits", "team-2"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}

	// unknown config keys are rejected (typos must not silently drop homes)
	mustWrite(t, cfg, "homez: []\n")
	if err := runProbe([]string{"--config", cfg}, &out); err == nil {
		t.Fatal("unknown key accepted")
	}
}

func TestServeRoutes(t *testing.T) {
	s := &server{}
	h := s.routes()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz before refresh = %d", rec.Code)
	}
	rem := 40.0
	s.set(Output{GeneratedAt: time.Unix(1790000000, 0).UTC(), Readings: []ccleft.Reading{
		{Provider: ccleft.DeepSeek, Account: "abc", State: ccleft.StateOK, Windows: []ccleft.Window{{ID: "balance:usd", Kind: ccleft.KindBalance, Binding: true, Remaining: &rem, Unit: "usd"}}},
		{Provider: ccleft.Claude, Account: "def", State: ccleft.StateRateLimited, Windows: []ccleft.Window{}},
	}}, nil)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readings?provider=deepseek", nil))
	var o Output
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil || len(o.Readings) != 1 || o.Readings[0].Provider != ccleft.DeepSeek {
		t.Fatalf("readings: %v %s", err, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	m := rec.Body.String()
	for _, want := range []string{
		`ccleft_remaining{provider="deepseek",account="abc",window="balance:usd",kind="balance",scope="",binding="true",unit="usd"} 40`,
		`ccleft_state{provider="claude",account="def",state="rate_limited"} 1`,
		`ccleft_last_refresh_timestamp_seconds 1790000000`,
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("missing %s in\n%s", want, m)
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz = %d", rec.Code)
	}
}
