package ccleft

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// WritePrometheus renders readings in the Prometheus text exposition format
// (no client library needed). Series:
//
//	ccleft_remaining_ratio{provider,account,window,kind,scope,binding}  0..1
//	ccleft_used / ccleft_limit / ccleft_remaining{...,unit}
//	ccleft_reset_timestamp_seconds{provider,account,window}
//	ccleft_state{provider,account,state}   one-hot (1 for the current state)
//	ccleft_stale{provider,account}          1 when serving last-good
//	ccleft_fetched_timestamp_seconds{provider,account}
//	ccleft_info{provider,account,plan,cause} 1
func WritePrometheus(w io.Writer, rs []Reading) error {
	bw := bufio.NewWriter(w)
	type series struct {
		name, help, typ string
		lines           []string
	}
	all := []*series{
		{name: "ccleft_remaining_ratio", help: "Remaining fraction (0..1) of a quota window.", typ: "gauge"},
		{name: "ccleft_used", help: "Used amount of a quota window, in the window's unit.", typ: "gauge"},
		{name: "ccleft_limit", help: "Limit of a quota window, in the window's unit.", typ: "gauge"},
		{name: "ccleft_remaining", help: "Remaining amount of a quota window, in the window's unit.", typ: "gauge"},
		{name: "ccleft_reset_timestamp_seconds", help: "Unix time at which a quota window resets.", typ: "gauge"},
		{name: "ccleft_state", help: "Account state (one-hot over ok, limited, exhausted, rate_limited, auth_required, unsupported, error).", typ: "gauge"},
		{name: "ccleft_stale", help: "1 when the reading is a cached last-good value served during an upstream failure.", typ: "gauge"},
		{name: "ccleft_fetched_timestamp_seconds", help: "Unix time the reading's windows were measured.", typ: "gauge"},
		{name: "ccleft_info", help: "Reading metadata.", typ: "gauge"},
	}
	by := map[string]*series{}
	for _, s := range all {
		by[s.name] = s
	}
	add := func(name string, labels [][2]string, v float64) {
		by[name].lines = append(by[name].lines, name+fmtLabels(labels)+" "+strconv.FormatFloat(v, 'f', -1, 64))
	}
	for _, r := range rs {
		acct := [][2]string{{"provider", string(r.Provider)}, {"account", r.Account}}
		for _, st := range States {
			v := 0.0
			if r.State == st {
				v = 1
			}
			add("ccleft_state", append(clone2(acct), [2]string{"state", string(st)}), v)
		}
		stale := 0.0
		if r.Stale {
			stale = 1
		}
		add("ccleft_stale", acct, stale)
		if !r.FetchedAt.IsZero() {
			add("ccleft_fetched_timestamp_seconds", acct, float64(r.FetchedAt.Unix()))
		}
		add("ccleft_info", append(clone2(acct), [2]string{"plan", r.Plan}, [2]string{"cause", r.Cause}), 1)
		for _, win := range r.Windows {
			wl := append(clone2(acct), [2]string{"window", win.ID})
			full := append(clone2(wl), [2]string{"kind", string(win.Kind)}, [2]string{"scope", win.Scope}, [2]string{"binding", strconv.FormatBool(win.Binding)})
			withUnit := append(clone2(full), [2]string{"unit", win.Unit})
			if win.RemainingPct != nil {
				add("ccleft_remaining_ratio", full, *win.RemainingPct/100)
			}
			if win.Used != nil {
				add("ccleft_used", withUnit, *win.Used)
			}
			if win.Limit != nil {
				add("ccleft_limit", withUnit, *win.Limit)
			}
			if win.Remaining != nil {
				add("ccleft_remaining", withUnit, *win.Remaining)
			}
			if win.ResetsAt != nil {
				add("ccleft_reset_timestamp_seconds", wl, float64(win.ResetsAt.Unix()))
			}
		}
	}
	for _, s := range all {
		if len(s.lines) == 0 {
			continue
		}
		fmt.Fprintf(bw, "# HELP %s %s\n# TYPE %s %s\n", s.name, s.help, s.name, s.typ)
		for _, l := range s.lines {
			bw.WriteString(l)
			bw.WriteByte('\n')
		}
	}
	return bw.Flush()
}

func clone2(l [][2]string) [][2]string { return append([][2]string(nil), l...) }

func fmtLabels(l [][2]string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, kv := range l {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[0])
		b.WriteString(`="`)
		b.WriteString(escapeLabel(kv[1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// WriteUpstreamMetrics renders Client.UpstreamCounts as the counter
//
//	ccleft_upstream_requests_total{provider,state,cause}
//
// so the load ccleft puts on each quota endpoint (and how often it is
// throttled: state="rate_limited",cause="http_429") is observable.
func WriteUpstreamMetrics(w io.Writer, counts map[UpstreamKey]uint64) error {
	keys := make([]UpstreamKey, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.State != b.State {
			return a.State < b.State
		}
		return a.Cause < b.Cause
	})
	bw := bufio.NewWriter(w)
	fmt.Fprint(bw, "# HELP ccleft_upstream_requests_total Upstream quota probes (HTTP requests or agy runs) made, by outcome.\n# TYPE ccleft_upstream_requests_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(bw, "ccleft_upstream_requests_total%s %d\n",
			fmtLabels([][2]string{{"provider", string(k.Provider)}, {"state", string(k.State)}, {"cause", k.Cause}}), counts[k])
	}
	return bw.Flush()
}
