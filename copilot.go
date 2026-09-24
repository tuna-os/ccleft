package ccleft

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Copilot: GET https://api.github.com/copilot_internal/user with a GitHub
// OAuth token. quota_snapshots carries the monthly premium_interactions /
// chat / completions quotas (entitlement, remaining, percent_remaining,
// unlimited); free-tier accounts report limited_user_quotas (remaining) +
// monthly_quotas (limits) instead. A quota with remaining ≤ 0 makes the
// account limited until quota_reset_date.
//
// Token discovery (read-only): Source.Credentials, then COPILOT_GITHUB_TOKEN /
// GH_TOKEN / GITHUB_TOKEN from Source.Env, then the Copilot editor token
// (<home>/.config/github-copilot/apps.json or hosts.json), then the gh CLI's
// plain-text hosts.yml. Keyring-stored gh tokens are not readable headlessly.

const copilotBaseURL = "https://api.github.com"

func init() { register(Copilot, impl{identify: copilotIdentify, fetch: copilotFetch}) }

func copilotConfigDir(src Source) string {
	if x := src.env("XDG_CONFIG_HOME"); x != "" {
		return x
	}
	return src.path(".config")
}

func copilotToken(src Source) (string, string) {
	if src.Credentials != "" {
		return src.Credentials, "override"
	}
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := src.env(k); v != "" {
			return v, k
		}
	}
	cfg := copilotConfigDir(src)
	if cfg == "" {
		return "", ""
	}
	for _, f := range []string{"apps.json", "hosts.json"} {
		var m map[string]struct {
			OAuthToken string `json:"oauth_token"`
		}
		path := filepath.Join(cfg, "github-copilot", f)
		if readJSON(path, &m) != nil {
			continue
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if strings.HasPrefix(k, "github.com") && m[k].OAuthToken != "" {
				return m[k].OAuthToken, path
			}
		}
	}
	ghDir := firstNonEmpty(src.env("GH_CONFIG_DIR"), filepath.Join(cfg, "gh"))
	hostsPath := filepath.Join(ghDir, "hosts.yml")
	if b, err := readFile(hostsPath); err == nil {
		var hosts map[string]struct {
			OAuthToken string `yaml:"oauth_token"`
		}
		if yaml.Unmarshal(b, &hosts) == nil && hosts["github.com"].OAuthToken != "" {
			return hosts["github.com"].OAuthToken, hostsPath
		}
	}
	return "", ""
}

func copilotIdentify(p *Prober, src Source) (credential, *Reading) {
	tok, from := copilotToken(src)
	if tok == "" {
		r := fail(Copilot, StateAuthRequired, "no_credentials", fmt.Errorf("%w: no GitHub token in env, github-copilot/apps.json or gh hosts.yml (keyring-stored gh tokens are not readable headlessly; export GH_TOKEN)", ErrNoCredentials))
		return credential{}, &r
	}
	return credential{token: tok, account: fingerprint(Copilot, "token:"+tok), extra: map[string]string{"from": from}}, nil
}

type copilotSnapshot struct {
	Entitlement      flexNum `json:"entitlement"`
	Remaining        flexNum `json:"remaining"`
	QuotaRemaining   flexNum `json:"quota_remaining"`
	PercentRemaining flexNum `json:"percent_remaining"`
	Unlimited        bool    `json:"unlimited"`
	OveragePermitted bool    `json:"overage_permitted"`
	OverageCount     flexNum `json:"overage_count"`
}

type copilotUser struct {
	Login                string                      `json:"login"`
	CopilotPlan          string                      `json:"copilot_plan"`
	QuotaResetDate       string                      `json:"quota_reset_date"`
	QuotaResetDateUTC    string                      `json:"quota_reset_date_utc"`
	LimitedUserResetDate string                      `json:"limited_user_reset_date"`
	QuotaSnapshots       map[string]*copilotSnapshot `json:"quota_snapshots"`
	LimitedUserQuotas    map[string]flexNum          `json:"limited_user_quotas"`
	MonthlyQuotas        map[string]flexNum          `json:"monthly_quotas"`
}

func copilotFetch(ctx context.Context, p *Prober, src Source, c credential) Reading {
	req, _ := http.NewRequest(http.MethodGet, p.endpoint(Copilot, copilotBaseURL)+"/copilot_internal/user", nil)
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Editor-Version", "vscode/1.99.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.26.7")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("X-Github-Api-Version", "2025-04-01")
	var u copilotUser
	if r := p.httpJSON(ctx, Copilot, req, &u); r != nil {
		if r.Cause == "http_404" {
			r.State, r.Cause = StateUnsupported, "no_copilot"
			r.Message = "this GitHub account has no Copilot entitlement: " + r.Message
		}
		return *r
	}
	return parseCopilotUser(u)
}

func parseCopilotUser(u copilotUser) Reading {
	reset := parseTime(firstNonEmpty(u.QuotaResetDateUTC, u.QuotaResetDate))
	var ws []Window
	var notes []string
	keys := make([]string, 0, len(u.QuotaSnapshots))
	for k := range u.QuotaSnapshots {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := u.QuotaSnapshots[k]
		if s == nil {
			continue
		}
		if s.Unlimited {
			notes = append(notes, k+": unlimited")
			continue
		}
		rem := s.Remaining
		if !rem.OK {
			rem = s.QuotaRemaining
		}
		var w Window
		switch {
		case s.Entitlement.OK && rem.OK:
			w = amountWindow(k, KindMonthly, s.Entitlement.V-rem.V, s.Entitlement.V, UnitRequests, reset)
		case s.PercentRemaining.OK:
			w = pctWindow(k, KindMonthly, 100-s.PercentRemaining.V, reset)
		default:
			continue
		}
		if s.OveragePermitted {
			notes = append(notes, k+": overage permitted")
		}
		ws = append(ws, w)
	}
	if len(ws) == 0 && len(u.LimitedUserQuotas) > 0 {
		fr := parseTime(u.LimitedUserResetDate)
		if fr == nil {
			fr = reset
		}
		fk := make([]string, 0, len(u.LimitedUserQuotas))
		for k := range u.LimitedUserQuotas {
			fk = append(fk, k)
		}
		sort.Strings(fk)
		for _, k := range fk {
			rem, lim := u.LimitedUserQuotas[k], u.MonthlyQuotas[k]
			if !rem.OK || !lim.OK {
				continue
			}
			ws = append(ws, amountWindow(k, KindMonthly, lim.V-rem.V, lim.V, UnitRequests, fr))
		}
	}
	if len(ws) == 0 && len(notes) == 0 {
		return fail(Copilot, StateError, "schema", errors.New("copilot_internal/user: no quota_snapshots or limited_user_quotas (unrecognized schema)"))
	}
	r := Reading{Windows: ws, Plan: u.CopilotPlan, Message: strings.Join(notes, "; ")}
	return r
}
