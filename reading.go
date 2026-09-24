// Package ccleft reports how much usage is LEFT on AI coding-agent accounts.
//
// ccusage answers "what did I spend?"; ccleft answers "what can I still
// spend, and when does it come back?". It reads each provider's own quota
// endpoint (or CLI, where no endpoint exists) using credentials that already
// sit in an agent's home directory, and normalizes the answer into a Reading:
// a state plus a list of Windows (five-hour, weekly, monthly, credits,
// balance, ...) with used / limit / remaining and reset times.
//
// ccleft is strictly read-only: it never refreshes, rewrites or creates a
// credential file, and every path it reads is derived from Source.Home (or an
// explicit env such as CODEX_HOME) — never from the process's own $HOME.
//
// Use Probe for a one-shot measurement, or a Client for fleet use: it adds a
// last-good cache, single-flight, Retry-After-aware exponential backoff and
// per-account rate limiting, and dedupes many homes that share one account.
package ccleft

import (
	"errors"
	"fmt"
	"time"
)

// Provider names a quota source.
type Provider string

// Supported providers.
const (
	Claude   Provider = "claude"   // Anthropic Claude Code subscription (OAuth)
	Codex    Provider = "codex"    // OpenAI Codex / ChatGPT subscription
	Agy      Provider = "agy"      // Google Antigravity CLI
	Gemini   Provider = "gemini"   // Google Gemini CLI (personal OAuth)
	Kiro     Provider = "kiro"     // Kiro (AWS CodeWhisperer backend) API key
	Copilot  Provider = "copilot"  // GitHub Copilot
	DeepSeek Provider = "deepseek" // DeepSeek API prepaid balance
	Muse     Provider = "muse"     // Meta Muse Spark
)

// Providers lists every provider ccleft knows, in display order.
var Providers = []Provider{Claude, Codex, Agy, Gemini, Kiro, Copilot, DeepSeek, Muse}

// ParseProvider validates a provider name.
func ParseProvider(s string) (Provider, error) {
	for _, p := range Providers {
		if string(p) == s {
			return p, nil
		}
	}
	return "", fmt.Errorf("ccleft: unknown provider %q", s)
}

// State is the account-level verdict of a Reading.
type State string

const (
	// StateOK: every binding window has headroom.
	StateOK State = "ok"
	// StateLimited: a binding window that RESETS on its own (5h, weekly,
	// monthly...) is used up. Work resumes at the window's resets_at.
	StateLimited State = "limited"
	// StateExhausted: a non-resetting pool (prepaid balance) is used up. Only
	// a top-up brings it back.
	StateExhausted State = "exhausted"
	// StateRateLimited: the QUOTA ENDPOINT itself throttled us (HTTP 429).
	// Says nothing about the account's quota; see RetryAt.
	StateRateLimited State = "rate_limited"
	// StateAuthRequired: credentials are missing, expired or rejected.
	StateAuthRequired State = "auth_required"
	// StateUnsupported: this login type cannot report quota at all (API-key
	// logins, shut-down endpoints). Not an error; nothing to retry.
	StateUnsupported State = "unsupported"
	// StateError: the measurement failed (network, timeout, schema drift, 5xx).
	StateError State = "error"
)

// States lists every state (used for one-hot metrics).
var States = []State{StateOK, StateLimited, StateExhausted, StateRateLimited, StateAuthRequired, StateUnsupported, StateError}

// HasQuota reports whether the state carries a measured quota verdict.
func (s State) HasQuota() bool {
	return s == StateOK || s == StateLimited || s == StateExhausted
}

// Kind is the shape of a quota window.
type Kind string

const (
	KindFiveHour Kind = "five_hour"
	KindDaily    Kind = "daily"
	KindWeekly   Kind = "weekly"
	KindMonthly  Kind = "monthly"
	KindCredits  Kind = "credits" // a credit pool (bonus, overage, extra usage)
	KindBalance  Kind = "balance" // prepaid money; does not reset
)

// Units used in Window.Unit.
const (
	UnitPercent  = "percent"
	UnitCredits  = "credits"
	UnitRequests = "requests"
	UnitUSD      = "usd"
)

// Window is one normalized quota window.
//
// For percent-only providers (Claude, Codex, Agy) Used/Limit/Remaining are
// expressed in percent points with Unit "percent" and Limit 100, so every
// window can be graphed on the same axes.
type Window struct {
	// ID is stable per provider (e.g. "five_hour", "seven_day_opus",
	// "premium_interactions", "plan", "bonus:WELCOME").
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`
	// Scope narrows the window to a model class, pool or feature ("opus",
	// "code_review", agy's "gemini"/"3p" pools).
	Scope string `json:"scope,omitempty"`
	// Binding marks windows that decide the account State. Model-scoped
	// windows are normally NOT binding (an Opus cap is not an account cap);
	// paid overflow (credits, extra usage, overage) never binds. agy, whose
	// entire quota is split into pools, marks every pool binding.
	Binding      bool       `json:"binding"`
	Used         *float64   `json:"used,omitempty"`
	Limit        *float64   `json:"limit,omitempty"`
	Remaining    *float64   `json:"remaining,omitempty"`
	UsedPct      *float64   `json:"used_pct,omitempty"`
	RemainingPct *float64   `json:"remaining_pct,omitempty"`
	ResetsAt     *time.Time `json:"resets_at,omitempty"`
	Unit         string     `json:"unit,omitempty"`
}

// Depleted reports whether the window has no headroom left.
func (w Window) Depleted() bool {
	if w.Remaining != nil {
		return *w.Remaining <= 0
	}
	if w.RemainingPct != nil {
		return *w.RemainingPct <= 0
	}
	return false
}

// Reading is the result of one probe.
type Reading struct {
	Provider Provider `json:"provider"`
	// Account is a stable, non-secret fingerprint (16 hex chars) of the
	// account behind the credential, so N homes sharing one account collapse
	// to one reading. Empty when no credential was found.
	Account string `json:"account,omitempty"`
	// Homes lists every source home that resolved to this account (filled by
	// Client.GetAll).
	Homes []string `json:"homes,omitempty"`
	State State    `json:"state"`
	// Cause is a short machine-readable reason ("http_429", "no_credentials",
	// "token_expired", "schema", "api_key_login", ...). Set on every non-ok
	// state and on stale readings; free of secrets.
	Cause string `json:"cause,omitempty"`
	// Message is the human-readable detail, including the underlying error
	// text. Errors are preserved here, never swallowed.
	Message string   `json:"message,omitempty"`
	Plan    string   `json:"plan,omitempty"`
	Windows []Window `json:"windows"`
	// FetchedAt is when the windows were measured. On a stale reading it is
	// the time of the last good measurement, not of the failed attempt.
	FetchedAt time.Time `json:"fetched_at"`
	// Stale means the windows are a cached last-good measurement served
	// because the latest attempt failed (Cause/Message describe the failure).
	Stale bool `json:"stale"`
	// RetryAt is when the next upstream attempt is allowed (429 Retry-After
	// or backoff).
	RetryAt *time.Time `json:"retry_at,omitempty"`

	// retryAfter carries a provider-supplied Retry-After to the Client.
	retryAfter time.Duration
	// transient marks failures worth serving last-good over (429, network,
	// 5xx, a refreshable expired token).
	transient bool
	// err is the underlying error, for errors.Is/As by library callers.
	err error
}

// Err returns the underlying error of a failed probe (nil when ok).
func (r Reading) Err() error { return r.err }

// Transient reports whether the failure is expected to clear on its own.
func (r Reading) Transient() bool { return r.transient }

// Binding returns the binding window with the least remaining percentage.
func (r Reading) Binding() (Window, bool) {
	var best Window
	found := false
	for _, w := range r.Windows {
		if !w.Binding || w.RemainingPct == nil {
			continue
		}
		if !found || *w.RemainingPct < *best.RemainingPct {
			best, found = w, true
		}
	}
	return best, found
}

// ProbeError is the error type carried by failed readings.
type ProbeError struct {
	Provider Provider
	State    State
	Cause    string
	Err      error
}

func (e *ProbeError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("%s: %s (%s)", e.Provider, e.State, e.Cause)
	}
	return fmt.Sprintf("%s: %s (%s): %v", e.Provider, e.State, e.Cause, e.Err)
}

func (e *ProbeError) Unwrap() error { return e.Err }

// ErrNoCredentials is wrapped by readings whose credential could not be found.
var ErrNoCredentials = errors.New("no credentials found")

func fail(p Provider, st State, cause string, err error) Reading {
	r := Reading{Provider: p, State: st, Cause: cause, Windows: []Window{}}
	pe := &ProbeError{Provider: p, State: st, Cause: cause, Err: err}
	r.err = pe
	if err != nil {
		r.Message = err.Error()
	}
	r.transient = st == StateRateLimited || st == StateError
	return r
}

// deriveState computes the account state from binding windows.
func deriveState(ws []Window) State {
	st := StateOK
	for _, w := range ws {
		if !w.Binding || !w.Depleted() {
			continue
		}
		if w.Kind == KindBalance {
			return StateExhausted
		}
		st = StateLimited
	}
	return st
}

func f64(v float64) *float64 { return &v }

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// pctWindow builds a percent-only window from a used percentage.
func pctWindow(id string, kind Kind, usedPct float64, reset *time.Time) Window {
	u := clampPct(usedPct)
	return Window{
		ID: id, Kind: kind, Binding: true,
		Used: f64(u), Limit: f64(100), Remaining: f64(100 - u),
		UsedPct: f64(u), RemainingPct: f64(100 - u),
		ResetsAt: reset, Unit: UnitPercent,
	}
}

// amountWindow builds a window from absolute used/limit amounts.
func amountWindow(id string, kind Kind, used, limit float64, unit string, reset *time.Time) Window {
	w := Window{ID: id, Kind: kind, Binding: true, Used: f64(used), Limit: f64(limit), Remaining: f64(limit - used), ResetsAt: reset, Unit: unit}
	if limit > 0 {
		up := clampPct(used / limit * 100)
		w.UsedPct, w.RemainingPct = f64(up), f64(100-up)
	}
	return w
}
