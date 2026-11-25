package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/tuna-os/ccleft"
)

func fptr(v float64) *float64 { return &v }

func TestMultiSet(t *testing.T) {
	var m multi
	if err := m.Set(" a, b ,,c"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("d"); err != nil {
		t.Fatal(err)
	}
	if got, want := m.String(), "a,b,c,d"; got != want {
		t.Fatalf("multi = %q, want %q", got, want)
	}
	var empty multi
	if err := empty.Set(" , "); err != nil || len(empty) != 0 {
		t.Fatalf("blank-only input = %v, %v; want no entries", empty, err)
	}
}

func TestOneLine(t *testing.T) {
	if got, want := oneLine("a \n\t b   c"), "a b c"; got != want {
		t.Fatalf("oneLine = %q, want %q", got, want)
	}
	long := strings.Repeat("x", 200)
	got := oneLine(long)
	if want := strings.Repeat("x", 160) + "…"; got != want {
		t.Fatalf("long message not truncated to 160 bytes plus ellipsis: %q", got)
	}
	if got := oneLine(strings.Repeat("y", 160)); strings.HasSuffix(got, "…") {
		t.Fatalf("a message of exactly 160 bytes must not be truncated: %q", got)
	}
}

func TestLeft(t *testing.T) {
	cases := []struct {
		name string
		w    ccleft.Window
		want string
	}{
		{"percent unit prefers percentage", ccleft.Window{Unit: ccleft.UnitPercent, RemainingPct: fptr(42.4), Remaining: fptr(7)}, "42%"},
		{"remaining over limit", ccleft.Window{Unit: ccleft.UnitCredits, Remaining: fptr(9.5), Limit: fptr(10)}, "9.50/10 credits"},
		{"remaining without limit", ccleft.Window{Unit: ccleft.UnitUSD, Remaining: fptr(3)}, "3.00 usd"},
		{"percentage in a non-percent unit", ccleft.Window{Unit: ccleft.UnitRequests, RemainingPct: fptr(80)}, "80%"},
		{"nothing known", ccleft.Window{Unit: ccleft.UnitCredits}, "?"},
		{"percent unit but only a used figure", ccleft.Window{Unit: ccleft.UnitPercent, Used: fptr(30)}, "?"},
	}
	for _, c := range cases {
		if got := left(c.w); got != c.want {
			t.Errorf("%s: left = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestResets(t *testing.T) {
	if got := resets(ccleft.Window{}); got != "-" {
		t.Fatalf("no reset time = %q, want -", got)
	}

	future := time.Now().Add(90 * time.Minute).UTC()
	got := resets(ccleft.Window{ResetsAt: &future})
	if !strings.HasPrefix(got, future.Format("2006-01-02 15:04Z")) || !strings.Contains(got, "(in 1h30m0s)") {
		t.Fatalf("future reset = %q, want timestamp and (in 1h30m0s)", got)
	}

	past := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	if got, want := resets(ccleft.Window{ResetsAt: &past}), past.Format(time.RFC3339); got != want {
		t.Fatalf("past reset = %q, want bare RFC3339 %q", got, want)
	}
}

func TestPrintTableEmpty(t *testing.T) {
	var out bytes.Buffer
	printTable(&out, nil)
	s := out.String()
	for _, want := range []string{"PROVIDER", "(no providers detected)", "* = informational window"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}

func TestPrintTableWindowsAndNotes(t *testing.T) {
	rs := []ccleft.Reading{
		{
			Provider: ccleft.Claude, Account: "acct1", State: ccleft.StateOK, Plan: "pro", Homes: []string{"/h1", "/h2"},
			Windows: []ccleft.Window{
				{ID: "five_hour", Binding: true, Unit: ccleft.UnitPercent, RemainingPct: fptr(75)},
				{ID: "seven_day_opus", Binding: false, Unit: ccleft.UnitPercent, RemainingPct: fptr(10)},
			},
			// Cause on an ok, non-stale reading must not produce a note.
			Cause: "ignored", Message: "ignored message",
		},
		{
			Provider: ccleft.DeepSeek, Account: "acct2", State: ccleft.StateRateLimited, Cause: "http_429",
			Message: "too many\nrequests   slow down",
		},
		{
			Provider: ccleft.Kiro, Account: "acct3", State: ccleft.StateOK, Stale: true, Cause: "timeout", Message: "deadline",
			Windows: []ccleft.Window{{ID: "plan", Binding: true, Unit: ccleft.UnitCredits, Remaining: fptr(5), Limit: fptr(10)}},
		},
	}
	var out bytes.Buffer
	printTable(&out, rs)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	s := out.String()

	// Second window of the same reading drops the repeated columns and is
	// marked informational with a trailing *.
	var opus string
	for _, l := range lines {
		if strings.Contains(l, "seven_day_opus*") {
			opus = l
		}
	}
	if opus == "" {
		t.Fatalf("informational window missing * marker in:\n%s", s)
	}
	if strings.Contains(opus, "claude") || strings.Contains(opus, "acct1") || strings.Contains(opus, "/h1") {
		t.Fatalf("continuation row repeats reading columns: %q", opus)
	}
	if !strings.Contains(s, "five_hour ") || strings.Contains(s, "five_hour*") {
		t.Fatalf("binding window must not carry the * marker in:\n%s", s)
	}
	if !strings.Contains(s, "/h1,/h2") {
		t.Fatalf("homes not joined on the first row in:\n%s", s)
	}

	// Windowless reading prints dashes and a numbered note below the table.
	if !strings.Contains(s, "rate_limited [1]") {
		t.Fatalf("rate_limited reading missing note reference [1] in:\n%s", s)
	}
	if !strings.Contains(s, "ok (stale) [2]") {
		t.Fatalf("stale reading missing '(stale)' and note reference [2] in:\n%s", s)
	}
	for _, want := range []string{
		"[1] deepseek acct2: http_429: too many requests slow down",
		"[2] kiro acct3: timeout: deadline",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing note %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "ignored message") {
		t.Fatalf("healthy reading leaked a note in:\n%s", s)
	}
	if !strings.Contains(s, "* = informational window") {
		t.Fatalf("legend missing in:\n%s", s)
	}
	// Notes follow the legend, after the table.
	if strings.Index(s, "* = informational window") > strings.Index(s, "[1] deepseek") {
		t.Fatalf("notes must follow the legend in:\n%s", s)
	}
}
