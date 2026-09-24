package ccleft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Claude: GET https://api.anthropic.com/api/oauth/usage with the Claude Code
// OAuth access token (<home>/.claude/.credentials.json, or
// $CLAUDE_CONFIG_DIR/.credentials.json) and the oauth beta header (401
// without it). The endpoint is the one Claude Code's own HUD polls; it sends
// no model prompt.
//
// It throttles hard (a 429 on most calls when many hosts poll one account),
// which is why the Client honours Retry-After, backs off and serves the
// last-good reading, and why homes sharing one account are deduped to one
// upstream call.

const (
	claudeBaseURL   = "https://api.anthropic.com"
	claudeOAuthBeta = "oauth-2025-04-20"
)

func init() { register(Claude, impl{identify: claudeIdentify, fetch: claudeFetch}) }

type claudeCreds struct {
	OAuth *struct {
		AccessToken      string `json:"accessToken"`
		RefreshToken     string `json:"refreshToken"`
		ExpiresAt        int64  `json:"expiresAt"`
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

func claudeConfigDir(src Source) string {
	if d := src.env("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return src.path(".claude")
}

func claudeIdentify(p *Prober, src Source) (credential, *Reading) {
	if src.Credentials != "" {
		return credential{token: src.Credentials, account: fingerprint(Claude, "token:"+src.Credentials)}, nil
	}
	dir := claudeConfigDir(src)
	if dir == "" {
		r := fail(Claude, StateAuthRequired, "no_credentials", fmt.Errorf("%w: no home or CLAUDE_CONFIG_DIR given", ErrNoCredentials))
		return credential{}, &r
	}
	path := filepath.Join(dir, ".credentials.json")
	var cf claudeCreds
	if err := readJSON(path, &cf); err != nil {
		r := fail(Claude, StateAuthRequired, "no_credentials", fmt.Errorf("%w: %v", ErrNoCredentials, err))
		return credential{}, &r
	}
	c := credential{account: claudeAccount(src, dir, cf)}
	if cf.OAuth == nil || strings.TrimSpace(cf.OAuth.AccessToken) == "" {
		// Claude Code zeroes the file when a refresh fails: positive evidence
		// the account cannot serve until a human runs /login.
		r := fail(Claude, StateAuthRequired, "no_credentials", fmt.Errorf("%s has no OAuth access token (run /login)", path))
		return c, &r
	}
	c.token = cf.OAuth.AccessToken
	c.plan = cf.OAuth.SubscriptionType
	if exp := cf.OAuth.ExpiresAt; exp > 0 && exp <= p.now().UnixMilli() {
		if cf.OAuth.RefreshToken != "" {
			// The CLI refreshes on its next request. ccleft never refreshes
			// (that would rewrite the file under a running agent), so this is
			// transient: serve last-good until the CLI rotates the token.
			r := fail(Claude, StateAuthRequired, "token_expired", fmt.Errorf("access token expired at %s; refresh token present, Claude Code refreshes on next use (ccleft never refreshes)", time.UnixMilli(exp).UTC().Format(time.RFC3339)))
			r.transient = true
			return c, &r
		}
		r := fail(Claude, StateAuthRequired, "login_expired", fmt.Errorf("access token expired and no refresh token: run /login"))
		return c, &r
	}
	return c, nil
}

// claudeAccount fingerprints the account from .claude.json's oauthAccount
// (account + organization uuid), falling back to the refresh token, which is
// stable between refreshes.
func claudeAccount(src Source, dir string, cf claudeCreds) string {
	var cj struct {
		OAuthAccount *struct {
			AccountUUID      string `json:"accountUuid"`
			OrganizationUUID string `json:"organizationUuid"`
		} `json:"oauthAccount"`
	}
	candidates := []string{filepath.Join(dir, ".claude.json")}
	if src.env("CLAUDE_CONFIG_DIR") == "" && src.Home != "" {
		candidates = append([]string{src.path(".claude.json")}, candidates...)
	}
	for _, c := range candidates {
		if err := readJSON(c, &cj); err == nil && cj.OAuthAccount != nil && cj.OAuthAccount.AccountUUID != "" {
			return fingerprint(Claude, "acct:"+cj.OAuthAccount.AccountUUID+"/"+cj.OAuthAccount.OrganizationUUID)
		}
	}
	if cf.OAuth != nil && cf.OAuth.RefreshToken != "" {
		return fingerprint(Claude, "refresh:"+cf.OAuth.RefreshToken)
	}
	if cf.OAuth != nil && cf.OAuth.AccessToken != "" {
		return fingerprint(Claude, "token:"+cf.OAuth.AccessToken)
	}
	return fingerprint(Claude, "home:"+dir)
}

func claudeFetch(ctx context.Context, p *Prober, src Source, c credential) Reading {
	req, _ := http.NewRequest(http.MethodGet, p.endpoint(Claude, claudeBaseURL)+"/api/oauth/usage", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	var raw map[string]json.RawMessage
	if r := p.httpJSON(ctx, Claude, req, &raw); r != nil {
		if r.Cause == "http_401" {
			r.Message += " (token rejected; if Claude Code is running it will refresh it)"
		}
		return *r
	}
	ws, err := parseClaudeUsage(raw)
	if err != nil {
		return fail(Claude, StateError, "schema", err)
	}
	return Reading{Windows: ws}
}

type claudeLimit struct {
	Kind        string  `json:"kind"`
	ID          string  `json:"id"`
	Percent     flexNum `json:"percent"`
	Utilization flexNum `json:"utilization"`
	ResetsAt    any     `json:"resets_at"`
	Scope       *struct {
		Model *struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type claudeBucket struct {
	Utilization flexNum `json:"utilization"`
	Percent     flexNum `json:"percent"`
	ResetsAt    any     `json:"resets_at"`
}

type claudeExtra struct {
	IsEnabled    bool    `json:"is_enabled"`
	MonthlyLimit flexNum `json:"monthly_limit"`
	UsedCredits  flexNum `json:"used_credits"`
	Utilization  flexNum `json:"utilization"`
}

// claudeClassify maps a provider window name onto (kind, scope).
func claudeClassify(name string) (Kind, string) {
	n := strings.ToLower(name)
	switch {
	case n == "session" || strings.Contains(n, "five_hour") || n == "5h":
		return KindFiveHour, ""
	case n == "weekly_all" || n == "seven_day" || n == "weekly":
		return KindWeekly, ""
	case strings.HasPrefix(n, "seven_day_") || strings.HasPrefix(n, "weekly_"):
		scope := n
		for _, p := range []string{"seven_day_scoped_", "weekly_scoped_", "seven_day_", "weekly_"} {
			if strings.HasPrefix(scope, p) {
				scope = strings.TrimPrefix(scope, p)
				break
			}
		}
		return KindWeekly, scope
	case strings.Contains(n, "daily") || strings.Contains(n, "one_day"):
		return KindDaily, ""
	case strings.Contains(n, "month"):
		return KindMonthly, ""
	}
	// Unknown kinds pass through verbatim and stay binding: an unfamiliar
	// window must still be enforced, not dropped.
	return Kind(n), ""
}

func parseClaudeUsage(raw map[string]json.RawMessage) ([]Window, error) {
	var ws []Window
	seen := map[string]bool{}
	add := func(w Window) {
		if seen[w.ID] {
			return
		}
		seen[w.ID] = true
		ws = append(ws, w)
	}

	// Newer payloads carry a limits[] array ({kind, percent, resets_at,
	// scope}); older/parallel ones top-level buckets ({utilization,
	// resets_at}). Read both; limits[] wins on collisions.
	if lr, ok := raw["limits"]; ok && string(lr) != "null" {
		var limits []claudeLimit
		if err := json.Unmarshal(lr, &limits); err != nil {
			return nil, fmt.Errorf("claude usage: limits[]: %w", err)
		}
		for _, l := range limits {
			pct := l.Percent
			if !pct.OK {
				pct = l.Utilization
			}
			if !pct.OK {
				continue // no reading; must not count as 0% used
			}
			name := l.Kind
			if name == "" {
				name = l.ID
			}
			kind, scope := claudeClassify(name)
			if l.Scope != nil && l.Scope.Model != nil {
				scope = strings.ToLower(firstNonEmpty(l.Scope.Model.DisplayName, l.Scope.Model.ID))
			}
			id := map[Kind]string{KindFiveHour: "five_hour", KindWeekly: "seven_day"}[kind]
			if id == "" {
				id = strings.ToLower(name)
			}
			if scope != "" {
				id = "seven_day_" + scope
				if kind != KindWeekly {
					id = strings.ToLower(name) + "_" + scope
				}
			}
			w := pctWindow(id, kind, pct.V, parseTime(l.ResetsAt))
			w.Scope = scope
			w.Binding = scope == ""
			add(w)
		}
	}

	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "limits" || k == "extra_usage" {
			continue
		}
		v := raw[k]
		if len(v) == 0 || v[0] != '{' {
			continue // null or scalar metadata
		}
		kind, scope := claudeClassify(k)
		if kind != KindFiveHour && kind != KindWeekly && kind != KindDaily && kind != KindMonthly {
			continue // top-level objects we do not know are not windows
		}
		var b claudeBucket
		if err := json.Unmarshal(v, &b); err != nil {
			return nil, fmt.Errorf("claude usage: %s: %w", k, err)
		}
		pct := b.Utilization
		if !pct.OK {
			pct = b.Percent
		}
		if !pct.OK {
			continue
		}
		w := pctWindow(k, kind, pct.V, parseTime(b.ResetsAt))
		w.Scope = scope
		w.Binding = scope == ""
		add(w)
	}

	if len(ws) == 0 {
		return nil, errors.New("claude usage: no window carried a percentage (unrecognized schema)")
	}

	if er, ok := raw["extra_usage"]; ok && len(er) > 0 && er[0] == '{' {
		var e claudeExtra
		if err := json.Unmarshal(er, &e); err != nil {
			return nil, fmt.Errorf("claude usage: extra_usage: %w", err)
		}
		if e.IsEnabled && e.MonthlyLimit.OK {
			used := 0.0
			if e.UsedCredits.OK {
				used = e.UsedCredits.V
			}
			// Amounts are in cents.
			w := amountWindow("extra_usage", KindCredits, used/100, e.MonthlyLimit.V/100, UnitUSD, nil)
			w.Binding = false // paid overflow, not the subscription quota
			add(w)
		}
	}
	sort.SliceStable(ws, func(i, j int) bool { return windowOrder(ws[i]) < windowOrder(ws[j]) })
	return ws, nil
}

func windowOrder(w Window) string {
	o := map[Kind]string{KindFiveHour: "0", KindDaily: "1", KindWeekly: "2", KindMonthly: "3", KindCredits: "4", KindBalance: "5"}[w.Kind]
	if o == "" {
		o = "6"
	}
	b := "0"
	if !w.Binding {
		b = "1"
	}
	return b + o + w.ID
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
