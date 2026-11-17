//go:build unix

package ccleft

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A home is written by its agent, so a credential path can hold anything the
// agent puts there. Whatever it holds, the probe has to come back.

func TestProbeDoesNotBlockOnFIFOCredential(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, ".credentials.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DetectHome(home, nil); len(got) != 1 || got[0].Provider != Claude {
		t.Fatalf("DetectHome = %+v, want the claude source so the home still reports", got)
	}

	done := make(chan Reading, 1)
	go func() {
		done <- (&Prober{}).Probe(context.Background(), Source{Provider: Claude, Home: home})
	}()
	select {
	case r := <-done:
		if r.State != StateAuthRequired || r.Cause != "no_credentials" || !strings.Contains(r.Message, "not a regular file") {
			t.Fatalf("reading = %s/%s: %s", r.State, r.Cause, r.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe is still blocked opening a FIFO in the agent home")
	}
}

func TestProbeRefusesOversizedCredential(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "auth.json")
	writeFile(t, path, "")
	// Sparse: one byte over the limit without writing it to disk.
	if err := os.Truncate(path, maxCredentialFile+1); err != nil {
		t.Fatal(err)
	}
	r := (&Prober{}).Probe(context.Background(), Source{Provider: Codex, Home: home})
	if r.State != StateAuthRequired || r.Cause != "no_credentials" || !strings.Contains(r.Message, "larger than") {
		t.Fatalf("reading = %s/%s: %s", r.State, r.Cause, r.Message)
	}
}

// A token file is a few kilobytes, and parsing one made of many small values
// takes 40 to 70 times its size in memory, so the limit for token files is far
// below the one .claude.json needs.
func TestProbeRefusesTokenFileOverOneMebibyte(t *testing.T) {
	home := t.TempDir()
	hosts := "github.com:\n    oauth_token: gho_FROMHOSTS\n"
	hosts += strings.Repeat("#\n", (maxCredentialFile-len(hosts))/2+1)
	writeFile(t, filepath.Join(home, ".config", "gh", "hosts.yml"), hosts)

	_, terminal := (&Prober{}).Identify(Source{Provider: Copilot, Home: home})
	if terminal == nil || terminal.State != StateAuthRequired || terminal.Cause != "no_credentials" {
		t.Fatalf("a %d byte hosts.yml was parsed: terminal = %+v", len(hosts), terminal)
	}
}

// .claude.json holds per-project state next to the account and is megabytes
// on a busy home; the token-file limit must not stop the account being read.
func TestClaudeAccountReadFromLargeStateFile(t *testing.T) {
	small, large := claudeHome(t, "same"), claudeHome(t, "same")
	writeFile(t, filepath.Join(large, ".claude.json"), map[string]any{
		"projects":     strings.Repeat("x", maxCredentialFile+1),
		"oauthAccount": map[string]any{"accountUuid": "same", "organizationUuid": "org-same"},
	})
	p := &Prober{Now: func() time.Time { return testNow }}
	want, _ := p.Identify(Source{Provider: Claude, Home: small})
	got, terminal := p.Identify(Source{Provider: Claude, Home: large})
	if terminal != nil || got != want {
		t.Fatalf("account = %q (terminal %+v), want %q read from the large .claude.json", got, terminal, want)
	}
}
