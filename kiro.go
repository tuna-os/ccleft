package ccleft

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Kiro: POST https://q.us-east-1.amazonaws.com/ (AWS JSON 1.0,
// AmazonCodeWhispererService.GetUsageLimits) with a Kiro API key (ksk_...)
// as a bearer token and `tokentype: API_KEY`.
//
// The plan's own usage is currentUsage − currentOverages (currentUsage
// includes overage). nextDateReset is authoritative; daysUntilReset comes
// back 0 and is ignored. Bonus credits and the overage allowance are
// separate windows.

const kiroBaseURL = "https://q.us-east-1.amazonaws.com"

func init() { register(Kiro, impl{identify: kiroIdentify, fetch: kiroFetch}) }

func kiroIdentify(p *Prober, src Source) (credential, *Reading) {
	key := firstNonEmpty(src.Credentials, src.env("KIRO_API_KEY"))
	if key == "" {
		r := fail(Kiro, StateAuthRequired, "no_credentials", fmt.Errorf("%w: KIRO_API_KEY not set", ErrNoCredentials))
		return credential{}, &r
	}
	return credential{token: strings.TrimSpace(key), account: fingerprint(Kiro, "key:"+strings.TrimSpace(key))}, nil
}

type kiroCredit struct {
	ResourceType                 string  `json:"resourceType"`
	DisplayName                  string  `json:"displayName"`
	Unit                         string  `json:"unit"`
	CurrentUsage                 flexNum `json:"currentUsage"`
	CurrentUsageWithPrecision    flexNum `json:"currentUsageWithPrecision"`
	UsageLimit                   flexNum `json:"usageLimit"`
	UsageLimitWithPrecision      flexNum `json:"usageLimitWithPrecision"`
	CurrentOverages              flexNum `json:"currentOverages"`
	CurrentOveragesWithPrecision flexNum `json:"currentOveragesWithPrecision"`
	OverageCap                   flexNum `json:"overageCap"`
	OverageCapWithPrecision      flexNum `json:"overageCapWithPrecision"`
	NextDateReset                flexNum `json:"nextDateReset"`
	FreeTrialInfo                *struct {
		FreeTrialStatus           string  `json:"freeTrialStatus"`
		UsageLimit                flexNum `json:"usageLimit"`
		UsageLimitWithPrecision   flexNum `json:"usageLimitWithPrecision"`
		CurrentUsage              flexNum `json:"currentUsage"`
		CurrentUsageWithPrecision flexNum `json:"currentUsageWithPrecision"`
		FreeTrialExpiry           any     `json:"freeTrialExpiry"`
	} `json:"freeTrialInfo"`
	Bonuses []struct {
		BonusCode                 string  `json:"bonusCode"`
		DisplayName               string  `json:"displayName"`
		Status                    string  `json:"status"`
		UsageLimit                flexNum `json:"usageLimit"`
		UsageLimitWithPrecision   flexNum `json:"usageLimitWithPrecision"`
		CurrentUsage              flexNum `json:"currentUsage"`
		CurrentUsageWithPrecision flexNum `json:"currentUsageWithPrecision"`
		ExpiresAt                 any     `json:"expiresAt"`
	} `json:"bonuses"`
}

type kiroResp struct {
	NextDateReset    flexNum `json:"nextDateReset"`
	SubscriptionInfo *struct {
		SubscriptionTitle string `json:"subscriptionTitle"`
		Type              string `json:"type"`
	} `json:"subscriptionInfo"`
	OverageConfiguration *struct {
		OverageStatus string `json:"overageStatus"`
	} `json:"overageConfiguration"`
	UsageBreakdownList []kiroCredit `json:"usageBreakdownList"`
}

func prec(a, b flexNum) flexNum {
	if a.OK {
		return a
	}
	return b
}

func kiroFetch(ctx context.Context, p *Prober, src Source, c credential) Reading {
	body := []byte(`{"origin":"AI_EDITOR","resourceType":"AGENTIC_REQUEST"}`)
	req, _ := http.NewRequest(http.MethodPost, p.endpoint(Kiro, kiroBaseURL)+"/", bytes.NewReader(body))
	var inv [16]byte
	_, _ = rand.Read(inv[:])
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonCodeWhispererService.GetUsageLimits")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("tokentype", "API_KEY")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("amz-sdk-invocation-id", hex.EncodeToString(inv[:]))
	var kr kiroResp
	if r := p.httpJSON(ctx, Kiro, req, &kr); r != nil {
		// AWS JSON errors come back as HTTP 400 with an exception __type.
		switch {
		case strings.Contains(r.Message, "Throttling"):
			r.State, r.Cause, r.transient = StateRateLimited, "throttled", true
		case strings.Contains(r.Message, "AccessDenied"), strings.Contains(r.Message, "UnrecognizedClient"),
			strings.Contains(r.Message, "ExpiredToken"), strings.Contains(r.Message, "InvalidToken"),
			strings.Contains(r.Message, "Unauthorized"):
			r.State, r.transient = StateAuthRequired, false
		}
		return *r
	}
	return parseKiroUsage(kr)
}

func parseKiroUsage(kr kiroResp) Reading {
	if len(kr.UsageBreakdownList) == 0 {
		return fail(Kiro, StateError, "schema", errors.New("kiro GetUsageLimits: empty usageBreakdownList (unrecognized schema)"))
	}
	overageOn := kr.OverageConfiguration != nil && kr.OverageConfiguration.OverageStatus != "" &&
		!strings.EqualFold(kr.OverageConfiguration.OverageStatus, "DISABLED")
	var ws []Window
	headroom := 0.0
	for i, b := range kr.UsageBreakdownList {
		used, limit := prec(b.CurrentUsageWithPrecision, b.CurrentUsage), prec(b.UsageLimitWithPrecision, b.UsageLimit)
		if !used.OK || !limit.OK {
			return fail(Kiro, StateError, "schema", fmt.Errorf("kiro usageBreakdownList[%d]: missing currentUsage/usageLimit", i))
		}
		over := prec(b.CurrentOveragesWithPrecision, b.CurrentOverages).V
		reset := parseTime(b.NextDateReset)
		if reset == nil {
			reset = parseTime(kr.NextDateReset)
		}
		suffix := ""
		if len(kr.UsageBreakdownList) > 1 {
			suffix = ":" + strings.ToLower(b.ResourceType)
		}
		plan := amountWindow("plan"+suffix, KindMonthly, used.V-over, limit.V, UnitCredits, reset)
		ws = append(ws, plan)
		headroom += max(*plan.Remaining, 0)

		if ft := b.FreeTrialInfo; ft != nil && strings.EqualFold(ft.FreeTrialStatus, "ACTIVE") {
			w := amountWindow("free_trial"+suffix, KindCredits, prec(ft.CurrentUsageWithPrecision, ft.CurrentUsage).V, prec(ft.UsageLimitWithPrecision, ft.UsageLimit).V, UnitCredits, parseTime(ft.FreeTrialExpiry))
			w.Binding = false
			ws = append(ws, w)
			headroom += max(*w.Remaining, 0)
		}
		sort.SliceStable(b.Bonuses, func(i, j int) bool { return b.Bonuses[i].BonusCode < b.Bonuses[j].BonusCode })
		for _, bn := range b.Bonuses {
			if bn.Status != "" && !strings.EqualFold(bn.Status, "ACTIVE") {
				continue
			}
			id := "bonus:" + strings.ToLower(firstNonEmpty(bn.BonusCode, bn.DisplayName, "unnamed"))
			w := amountWindow(id, KindCredits, prec(bn.CurrentUsageWithPrecision, bn.CurrentUsage).V, prec(bn.UsageLimitWithPrecision, bn.UsageLimit).V, UnitCredits, parseTime(bn.ExpiresAt))
			w.Binding = false
			ws = append(ws, w)
			headroom += max(*w.Remaining, 0)
		}
		if oc := prec(b.OverageCapWithPrecision, b.OverageCap); overageOn && oc.OK && oc.V > 0 {
			w := amountWindow("overage"+suffix, KindCredits, over, oc.V, UnitCredits, reset)
			w.Binding = false
			ws = append(ws, w)
			headroom += max(*w.Remaining, 0)
		}
	}
	r := Reading{Windows: ws, State: StateOK}
	if kr.SubscriptionInfo != nil {
		r.Plan = firstNonEmpty(kr.SubscriptionInfo.SubscriptionTitle, kr.SubscriptionInfo.Type)
	}
	// Bonus, free-trial and (enabled) overage credits all keep work going
	// after the plan allowance runs out, so the account is limited only when
	// all of them are gone.
	if headroom <= 0 {
		r.State = StateLimited
		if !overageOn {
			r.Message = "plan credits used up and overage is disabled"
		}
	}
	return r
}
