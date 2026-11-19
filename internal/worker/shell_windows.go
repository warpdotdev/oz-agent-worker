//go:build windows

package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

func newShellCommand(ctx context.Context, command string) *exec.Cmd {
	shell := os.Getenv("ComSpec")
	if shell == "" {
		shell = "cmd.exe"
	}
	cmd := exec.CommandContext(ctx, shell) // #nosec G204 -- hooks and the host shell are explicit operator configuration.
	// Killing only cmd.exe leaves its hook processes running.
	cmd.Cancel = func() error {
		if err := exec.Command("taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
			return errors.Join(err, cmd.Process.Kill())
		}
		return nil
	}
	// cmd.exe does not use Go's argv quoting rules. Keep the operator's command
	// intact inside an outer quote pair, including commands with quoted paths.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: syscall.EscapeArg(shell) + ` /C "` + command + `"`,
	}
	return cmd
}
