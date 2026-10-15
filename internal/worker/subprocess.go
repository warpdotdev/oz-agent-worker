package worker

import (
	"errors"
	"os/exec"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/tasklogs"
)

func runTaskCommand(cmd *exec.Cmd, reporter *tasklogs.Reporter) error {
	if reporter != nil {
		// Descendants may keep output pipes open after the task's subprocess exits.
		cmd.WaitDelay = 100 * time.Millisecond
	}
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}
