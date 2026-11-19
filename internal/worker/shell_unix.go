//go:build !windows

package worker

import (
	"context"
	"os/exec"
)

func newShellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-c", command) // #nosec G204 -- hooks are explicit operator configuration.
}
