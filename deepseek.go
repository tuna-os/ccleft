package ccleft

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// DeepSeek: GET https://api.deepseek.com/user/balance with DEEPSEEK_API_KEY.
// A prepaid balance per currency; ≤ 0 (or is_available false) is exhausted —
// it does not reset, only a top-up restores it.

const deepSeekBaseURL = "https://api.deepseek.com"

func init() { register(DeepSeek, impl{identify: deepSeekIdentify, fetch: deepSeekFetch}) }

func deepSeekIdentify(p *Prober, src Source) (credential, *Reading) {
	key := firstNonEmpty(src.Credentials, src.env("DEEPSEEK_API_KEY"))
	if key == "" {
		r := fail(DeepSeek, StateAuthRequired, "no_credentials", fmt.Errorf("%w: DEEPSEEK_API_KEY not set", ErrNoCredentials))
		return credential{}, &r
	}
	key = strings.TrimSpace(key)
	return credential{token: key, account: fingerprint(DeepSeek, "key:"+key)}, nil
}

type deepSeekBalance struct {
	IsAvailable  *bool `json:"is_available"`
	BalanceInfos []struct {
		Currency        string  `json:"currency"`
		TotalBalance    flexNum `json:"total_balance"`
		GrantedBalance  flexNum `json:"granted_balance"`
		ToppedUpBalance flexNum `json:"topped_up_balance"`
	} `json:"balance_infos"`
}

func deepSeekFetch(ctx context.Context, p *Prober, src Source, c credential) Reading {
	req, _ := http.NewRequest(http.MethodGet, p.endpoint(DeepSeek, deepSeekBaseURL)+"/user/balance", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	var b deepSeekBalance
	if r := p.httpJSON(ctx, DeepSeek, req, &b); r != nil {
		return *r
	}
	return parseDeepSeekBalance(b)
}

func parseDeepSeekBalance(b deepSeekBalance) Reading {
	var ws []Window
	for _, bi := range b.BalanceInfos {
		if !bi.TotalBalance.OK {
			continue
		}
		cur := strings.ToLower(firstNonEmpty(bi.Currency, "unknown"))
		ws = append(ws, Window{ID: "balance:" + cur, Kind: KindBalance, Binding: true, Remaining: f64(bi.TotalBalance.V), Unit: cur})
	}
	if len(ws) == 0 {
		return fail(DeepSeek, StateError, "schema", errors.New("deepseek /user/balance: no balance_infos[].total_balance (unrecognized schema)"))
	}
	r := Reading{Windows: ws}
	r.State = deriveState(ws)
	// Several currencies: the account can serve while any has money.
	if len(ws) > 1 {
		r.State = StateExhausted
		for _, w := range ws {
			if *w.Remaining > 0 {
				r.State = StateOK
			}
		}
	}
	if b.IsAvailable != nil && !*b.IsAvailable {
		r.State = StateExhausted
		r.Message = "provider reports is_available=false"
	}
	return r
}
