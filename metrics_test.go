package ccleft

import (
	"bytes"
	"strings"
	"testing"
)

// escapeLabel must follow the Prometheus text-format rules for label values:
// backslash, double quote and newline are escaped, everything else is kept.
func TestEscapeLabel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"", ""},
		{`a"b`, `a\"b`},
		{`a\b`, `a\\b`},
		{"a\nb", `a\nb`},
		// The backslash is escaped first, so the backslashes added for the
		// quote and the newline are not doubled again.
		{`\"`, `\\\"`},
		{"\\\n", `\\\n`},
		{`C:\path "x"` + "\n", `C:\\path \"x\"\n`},
	} {
		if got := escapeLabel(tc.in); got != tc.want {
			t.Errorf("escapeLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A label value carrying upstream text (here the cause) must not be able to
// break out of its quotes or split a series across lines.
func TestWritePrometheusEscapesHostileLabelValues(t *testing.T) {
	var buf bytes.Buffer
	err := WritePrometheus(&buf, []Reading{{
		Provider: Claude,
		Account:  "acct",
		State:    StateError,
		Cause:    "bad \"gateway\"\nHTTP\\1.1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `ccleft_info{provider="claude",account="acct",plan="",cause="bad \"gateway\"\nHTTP\\1.1"} 1`
	if !strings.Contains(buf.String(), want+"\n") {
		t.Fatalf("missing %q in:\n%s", want, buf.String())
	}
	for _, line := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "ccleft_") {
			t.Fatalf("label value split a series across lines: %q", line)
		}
	}
}
