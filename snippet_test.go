package ccleft

import (
	"strings"
	"testing"
)

// Every prefix maskSecrets knows must hide the token that follows it. The
// existing TestMaskSecrets only covers ksk_, Bearer and ghp_.
func TestSnippetMasksEachTokenPrefix(t *testing.T) {
	for _, pfx := range []string{"Bearer ", "ksk_", "sk-", "ghp_", "gho_", "ghu_", "github_pat_"} {
		t.Run(strings.TrimSpace(pfx), func(t *testing.T) {
			got := snippet([]byte("bad credential " + pfx + "SECRET123 rejected"))
			if strings.Contains(got, "SECRET123") {
				t.Fatalf("token leaked: %q", got)
			}
			if !strings.Contains(got, "<redacted>") || !strings.HasSuffix(got, " rejected") {
				t.Fatalf("want token replaced in place, got %q", got)
			}
		})
	}
}

// A token ends at a space, quote or comma, so the text after it survives.
func TestSnippetMaskStopsAtDelimiter(t *testing.T) {
	got := snippet([]byte(`{"a":"sk-AAA","b":"ok"} tail`))
	if strings.Contains(got, "AAA") {
		t.Fatalf("token leaked: %q", got)
	}
	if !strings.Contains(got, `"b":"ok"`) {
		t.Fatalf("masking ate text past the token: %q", got)
	}
}

func TestSnippetCollapsesWhitespaceAndTruncates(t *testing.T) {
	if got := snippet([]byte("a\n\t b  \r\n c")); got != "a b c" {
		t.Fatalf("whitespace not collapsed: %q", got)
	}
	long := snippet([]byte(strings.Repeat("a", 400)))
	if want := strings.Repeat("a", 300) + "…"; long != want {
		t.Fatalf("long body = %d bytes, want 300 bytes plus an ellipsis", len(long))
	}
	if got := snippet([]byte(strings.Repeat("a", 300))); strings.HasSuffix(got, "…") {
		t.Fatal("a body of exactly 300 bytes must not be truncated")
	}
}
