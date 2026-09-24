package ccleft

import (
	"context"
	"errors"
	"fmt"
)

// Gemini CLI (personal Google OAuth): Google shut the Code Assist quota
// surface for personal OAuth clients in June 2026 — loadCodeAssist answers
// UNSUPPORTED_CLIENT and retrieveUserQuota answers PERMISSION_DENIED. ccleft
// reports that as state "unsupported" without calling the API (every call
// would fail the same way). Gemini API keys have per-project rate limits
// that no endpoint exposes, so they are unsupported too.
//
// Muse (Meta Muse Spark): API-key logins cannot read the subscription quota.
// The device-code (OAuth) login exposes one; supporting it is phase-2 work.

var errGeminiShutdown = errors.New("Google shut down quota reads for Gemini CLI personal OAuth in June 2026: loadCodeAssist returns UNSUPPORTED_CLIENT and retrieveUserQuota returns PERMISSION_DENIED; no remaining-quota source exists for this login type")

func init() {
	register(Gemini, impl{identify: geminiIdentify, fetch: unreachableFetch})
	register(Muse, impl{identify: museIdentify, fetch: unreachableFetch})
}

func unreachableFetch(ctx context.Context, p *Prober, src Source, c credential) Reading {
	return fail(src.Provider, StateUnsupported, "unsupported", errors.New("no quota source"))
}

func geminiIdentify(p *Prober, src Source) (credential, *Reading) {
	oauth := src.path(".gemini", "oauth_creds.json")
	if src.env("GEMINI_CLI_HOME") != "" {
		oauth = src.env("GEMINI_CLI_HOME") + "/.gemini/oauth_creds.json"
	}
	if fileExists(oauth) {
		c := credential{account: fingerprint(Gemini, "home:"+oauth)}
		var t struct {
			RefreshToken string `json:"refresh_token"`
		}
		if readJSON(oauth, &t) == nil && t.RefreshToken != "" {
			c.account = fingerprint(Gemini, "refresh:"+t.RefreshToken)
		}
		r := fail(Gemini, StateUnsupported, "api_shutdown", errGeminiShutdown)
		return c, &r
	}
	if k := firstNonEmpty(src.Credentials, src.env("GEMINI_API_KEY"), src.env("GOOGLE_API_KEY")); k != "" {
		r := fail(Gemini, StateUnsupported, "api_key_login", errors.New("Gemini API keys have per-project rate limits that no API exposes"))
		c := credential{account: fingerprint(Gemini, "key:"+k)}
		return c, &r
	}
	r := fail(Gemini, StateAuthRequired, "no_credentials", fmt.Errorf("%w: no ~/.gemini/oauth_creds.json or GEMINI_API_KEY", ErrNoCredentials))
	return credential{}, &r
}

func museIdentify(p *Prober, src Source) (credential, *Reading) {
	if k := firstNonEmpty(src.Credentials, src.env("META_API_KEY"), src.env("MUSE_API_KEY")); k != "" {
		r := fail(Muse, StateUnsupported, "api_key_login", errors.New("Muse API-key logins cannot read the subscription quota; only the device-code (OAuth) login exposes it (not yet implemented)"))
		return credential{account: fingerprint(Muse, "key:"+k)}, &r
	}
	r := fail(Muse, StateAuthRequired, "no_credentials", fmt.Errorf("%w: no META_API_KEY", ErrNoCredentials))
	return credential{}, &r
}
