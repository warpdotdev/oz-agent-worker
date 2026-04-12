package common

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// TaskAugmentOptions contains worker-level settings that are translated into oz CLI flags
// for every task. Add new per-worker CLI overrides here rather than as extra parameters.
type TaskAugmentOptions struct {
	// IdleOnComplete is passed to --idle-on-complete. Empty string uses the oz CLI default
	// (45m). Use "0s" to exit immediately after the conversation finishes.
	// Task-level config.idle_timeout_minutes takes precedence when set.
	IdleOnComplete string
}

// AugmentArgsForTask allows different task sources to add CLI args in a centralized place.
// Uses task.AgentConfigSnapshot as the source of truth when available.
func AugmentArgsForTask(task *types.Task, args []string, opts TaskAugmentOptions) []string {
	if task == nil {
		return args
	}

	if task.AgentConfigSnapshot != nil {
		if task.AgentConfigSnapshot.ModelID != nil {
			if modelID := strings.TrimSpace(*task.AgentConfigSnapshot.ModelID); modelID != "" {
				args = append(args, "--model", modelID)
			}
		}

		if task.AgentConfigSnapshot.ProfileID != nil {
			if profileID := strings.TrimSpace(*task.AgentConfigSnapshot.ProfileID); profileID != "" {
				args = append(args, "--profile", profileID)
			}
		}

		if task.AgentConfigSnapshot.SkillSpec != nil {
			if skillSpec := strings.TrimSpace(*task.AgentConfigSnapshot.SkillSpec); skillSpec != "" {
				args = append(args, "--skill", skillSpec)
			}
		}

		if len(task.AgentConfigSnapshot.MCPServers) > 0 {
			b, err := json.Marshal(task.AgentConfigSnapshot.MCPServers)
			if err == nil {
				args = append(args, "--mcp", string(b))
			}
		}

		// Pass computer use setting if explicitly configured.
		if task.AgentConfigSnapshot.ComputerUseEnabled != nil {
			if *task.AgentConfigSnapshot.ComputerUseEnabled {
				args = append(args, "--computer-use")
			} else {
				args = append(args, "--no-computer-use")
			}
		}

		if task.AgentConfigSnapshot.UseAwsBedrockInference != nil &&
			*task.AgentConfigSnapshot.UseAwsBedrockInference {
			args = append(args, "--use-aws-bedrock-inference")
		}
	}

	if task.AgentConfigSnapshot != nil && task.AgentConfigSnapshot.EnvironmentID != nil {
		env := strings.TrimSpace(*task.AgentConfigSnapshot.EnvironmentID)
		if env != "" {
			args = append(args, "--environment", env)
		}
	}

	// Keep the agent alive after task completion to allow follow-ups.
	// Priority: task config idle_timeout_minutes > worker IdleOnComplete > oz CLI default (45m).
	idleOnComplete, hasIdleOnCompleteValue := resolveIdleOnComplete(task, opts)
	if !hasIdleOnCompleteValue {
		args = append(args, "--idle-on-complete")
	} else {
		args = append(args, "--idle-on-complete", idleOnComplete)
	}

	return args
}

func resolveIdleOnComplete(task *types.Task, opts TaskAugmentOptions) (string, bool) {
	if task != nil &&
		task.AgentConfigSnapshot != nil &&
		task.AgentConfigSnapshot.IdleTimeoutMinutes != nil &&
		*task.AgentConfigSnapshot.IdleTimeoutMinutes > 0 {
		return fmt.Sprintf("%dm", *task.AgentConfigSnapshot.IdleTimeoutMinutes), true
	}

	if opts.IdleOnComplete != "" {
		return opts.IdleOnComplete, true
	}

	return "", false
}
