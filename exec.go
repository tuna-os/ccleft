package ccleft

import (
	"context"
	"os/exec"
	"time"
)

// ExecFunc runs a CLI with exactly the given environment (never inheriting the
// caller's) and returns combined output.
type ExecFunc func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)

// defaultExec runs a CLI with an explicit environment and bounded waits. A
// grandchild holding the output pipe cannot wedge the caller: WaitDelay
// force-closes the pipes after the context ends.
func defaultExec(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	return cmd.CombinedOutput()
}
