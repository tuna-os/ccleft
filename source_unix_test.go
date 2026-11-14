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
	// Sparse: one byte over the limit without writing 32 MiB to disk.
	if err := os.Truncate(path, maxCredentialFile+1); err != nil {
		t.Fatal(err)
	}
	r := (&Prober{}).Probe(context.Background(), Source{Provider: Codex, Home: home})
	if r.State != StateAuthRequired || r.Cause != "no_credentials" || !strings.Contains(r.Message, "larger than") {
		t.Fatalf("reading = %s/%s: %s", r.State, r.Cause, r.Message)
	}
}
