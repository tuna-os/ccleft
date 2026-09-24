package ccleft

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// Codex: GET https://chatgpt.com/backend-api/wham/usage with the ChatGPT
// access token from $CODEX_HOME/auth.json (default <home>/.codex/auth.json)
// and the ChatGPT-Account-Id header.
//
// ccleft deliberately never runs `codex app-server` (or any codex command):
// the CLI may refresh and REWRITE auth.json, racing a running agent. An
// expired access token is reported as transient auth_required instead.

const codexBaseURL = "https://chatgpt.com/backend-api"

func init() { register(Codex, impl{identify: codexIdentify, fetch: codexFetch}) }

type codexAuth struct {
	APIKey *string `json:"OPENAI_API_KEY"`
	Tokens *struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

func codexHome(src Source) string {
	if d := src.env("CODEX_HOME"); d != "" {
		return d
	}
	return src.path(".codex")
}

// jwtClaims decodes (without verifying) a JWT payload. Used only to read
// exp / plan / account claims for display and dedupe.
func jwtClaims(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func codexAuthClaim(claims map[string]any, key string) string {
	if claims == nil {
		return ""
	}
	if a, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if s, ok := a[key].(string); ok {
			return s
		}
	}
	return ""
}

func codexIdentify(p *Prober, src Source) (credential, *Reading) {
	if src.Credentials != "" {
		c := credential{token: src.Credentials, extra: map[string]string{"account_id": src.env("CHATGPT_ACCOUNT_ID")}}
		c.account = fingerprint(Codex, "acct:"+firstNonEmpty(c.extra["account_id"], codexAuthClaim(jwtClaims(src.Credentials), "chatgpt_account_id"), "token:"+src.Credentials))
		return c, nil
	}
	dir := codexHome(src)
	if dir == "" {
		r := fail(Codex, StateAuthRequired, "no_credentials", fmt.Errorf("%w: no home or CODEX_HOME given", ErrNoCredentials))
		return credential{}, &r
	}
	path := filepath.Join(dir, "auth.json")
	var a codexAuth
	if err := readJSON(path, &a); err != nil {
		r := fail(Codex, StateAuthRequired, "no_credentials", fmt.Errorf("%w: %v", ErrNoCredentials, err))
		return credential{}, &r
	}
	if a.Tokens == nil || strings.TrimSpace(a.Tokens.AccessToken) == "" {
		if a.APIKey != nil && strings.TrimSpace(*a.APIKey) != "" {
			r := fail(Codex, StateUnsupported, "api_key_login", errors.New("codex is logged in with an OpenAI API key: pay-as-you-go keys have no subscription windows to report"))
			r.Account = fingerprint(Codex, "apikey:"+*a.APIKey)
			return credential{account: r.Account}, &r
		}
		r := fail(Codex, StateAuthRequired, "no_credentials", fmt.Errorf("%s has no ChatGPT tokens (run codex login)", path))
		return credential{}, &r
	}
	claims := jwtClaims(a.Tokens.AccessToken)
	idClaims := jwtClaims(a.Tokens.IDToken)
	acct := firstNonEmpty(a.Tokens.AccountID, codexAuthClaim(claims, "chatgpt_account_id"), codexAuthClaim(idClaims, "chatgpt_account_id"))
	c := credential{
		token: a.Tokens.AccessToken,
		plan:  firstNonEmpty(codexAuthClaim(idClaims, "chatgpt_plan_type"), codexAuthClaim(claims, "chatgpt_plan_type")),
		extra: map[string]string{"account_id": acct},
	}
	if acct != "" {
		c.account = fingerprint(Codex, "acct:"+acct)
	} else {
		c.account = fingerprint(Codex, "token:"+a.Tokens.AccessToken)
	}
	if exp, ok := claims["exp"].(float64); ok && int64(exp) <= p.now().Unix() {
		r := fail(Codex, StateAuthRequired, "token_expired", fmt.Errorf("access token expired at %s; codex refreshes it on next use (ccleft never refreshes or runs codex)", time.Unix(int64(exp), 0).UTC().Format(time.RFC3339)))
		r.transient = true
		return c, &r
	}
	return c, nil
}

type codexWin struct {
	UsedPercent        flexNum `json:"used_percent"`
	RemainingPercent   flexNum `json:"remaining_percent"`
	LimitWindowSeconds flexNum `json:"limit_window_seconds"`
	WindowMinutes      flexNum `json:"window_minutes"`
	ResetAt            flexNum `json:"reset_at"`
	ResetsAt           flexNum `json:"resets_at"`
	ResetAfterSeconds  flexNum `json:"reset_after_seconds"`
}

type codexLimit struct {
	Allowed         *bool     `json:"allowed"`
	LimitReached    *bool     `json:"limit_reached"`
	PrimaryWindow   *codexWin `json:"primary_window"`
	SecondaryWindow *codexWin `json:"secondary_window"`
	Primary         *codexWin `json:"primary"`
	Secondary       *codexWin `json:"secondary"`
}

type codexUsage struct {
	PlanType             string      `json:"plan_type"`
	RateLimit            *codexLimit `json:"rate_limit"`
	CodeReviewRateLimit  *codexLimit `json:"code_review_rate_limit"`
	AdditionalRateLimits []struct {
		LimitName      string      `json:"limit_name"`
		MeteredFeature string      `json:"metered_feature"`
		RateLimit      *codexLimit `json:"rate_limit"`
	} `json:"additional_rate_limits"`
	Credits *struct {
		HasCredits bool    `json:"has_credits"`
		Unlimited  bool    `json:"unlimited"`
		Balance    flexNum `json:"balance"`
	} `json:"credits"`
}

func codexFetch(ctx context.Context, p *Prober, src Source, c credential) Reading {
	req, _ := http.NewRequest(http.MethodGet, p.endpoint(Codex, codexBaseURL)+"/wham/usage", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if id := c.extra["account_id"]; id != "" {
		req.Header.Set("ChatGPT-Account-Id", id)
	}
	req.Header.Set("User-Agent", "codex-cli (ccleft/"+Version+")")
	var u codexUsage
	if r := p.httpJSON(ctx, Codex, req, &u); r != nil {
		return *r
	}
	return parseCodexUsage(u, p.now())
}

// codexKind bands a window by its provider-stated duration.
func codexKind(mins float64, fallback Kind) Kind {
	switch {
	case mins <= 0:
		return fallback
	case mins <= 360:
		return KindFiveHour
	case mins <= 2880:
		return KindDaily
	case mins <= 20160:
		return KindWeekly
	default:
		return KindMonthly
	}
}

func codexWindow(w *codexWin, scope string, fallback Kind, now time.Time) (Window, bool) {
	if w == nil {
		return Window{}, false
	}
	var used float64
	switch {
	case w.UsedPercent.OK:
		used = w.UsedPercent.V
	case w.RemainingPercent.OK:
		used = 100 - w.RemainingPercent.V
	default:
		return Window{}, false
	}
	mins := w.WindowMinutes.V
	if w.LimitWindowSeconds.OK {
		mins = w.LimitWindowSeconds.V / 60
	}
	kind := codexKind(mins, fallback)
	var reset *time.Time
	switch {
	case w.ResetAt.OK:
		reset = parseTime(w.ResetAt)
	case w.ResetsAt.OK:
		reset = parseTime(w.ResetsAt)
	case w.ResetAfterSeconds.OK:
		t := now.Add(time.Duration(w.ResetAfterSeconds.V * float64(time.Second))).UTC().Truncate(time.Second)
		reset = &t
	}
	id := string(kind)
	if scope != "" {
		id = scope + ":" + id
	}
	out := pctWindow(id, kind, used, reset)
	out.Scope = scope
	out.Binding = scope == ""
	return out, true
}

func parseCodexUsage(u codexUsage, now time.Time) Reading {
	var ws []Window
	addLimit := func(l *codexLimit, scope string) {
		if l == nil {
			return
		}
		pw, sw := l.PrimaryWindow, l.SecondaryWindow
		if pw == nil {
			pw = l.Primary
		}
		if sw == nil {
			sw = l.Secondary
		}
		if w, ok := codexWindow(pw, scope, KindFiveHour, now); ok {
			ws = append(ws, w)
		}
		if w, ok := codexWindow(sw, scope, KindWeekly, now); ok {
			ws = append(ws, w)
		}
	}
	addLimit(u.RateLimit, "")
	addLimit(u.CodeReviewRateLimit, "code_review")
	for _, a := range u.AdditionalRateLimits {
		addLimit(a.RateLimit, strings.ToLower(firstNonEmpty(a.MeteredFeature, a.LimitName, "additional")))
	}
	if len(ws) == 0 {
		return fail(Codex, StateError, "schema", errors.New("codex usage: no rate_limit window carried a percentage (unrecognized schema)"))
	}
	r := Reading{Plan: u.PlanType}
	var notes []string
	if u.Credits != nil {
		switch {
		case u.Credits.Unlimited:
			notes = append(notes, "credits: unlimited")
		case u.Credits.Balance.OK:
			// Balance arrives as "0" (string) or 0 (number) depending on the
			// day; flexNum takes both. Credits are paid overflow, not the
			// subscription windows, so they never bind.
			ws = append(ws, Window{ID: "credits", Kind: KindCredits, Remaining: f64(u.Credits.Balance.V), Unit: UnitCredits})
		}
	}
	r.Windows = ws
	r.State = deriveState(ws)
	if l := u.RateLimit; l != nil && ((l.LimitReached != nil && *l.LimitReached) || (l.Allowed != nil && !*l.Allowed)) {
		r.State = StateLimited
		notes = append(notes, "provider reports limit_reached")
	}
	r.Message = strings.Join(notes, "; ")
	return r
}
