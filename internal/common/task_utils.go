package common

import (
	"encoding/json"
	"strings"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// AugmentArgsForTask allows different task sources to add CLI args in a centralized place.
// Uses task.AgentConfigSnapshot as the source of truth when available.
func AugmentArgsForTask(task *types.Task, args []string) []string {
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
	}

	if task.AgentConfigSnapshot != nil && task.AgentConfigSnapshot.EnvironmentID != nil {
		env := strings.TrimSpace(*task.AgentConfigSnapshot.EnvironmentID)
		if env != "" {
			args = append(args, "--environment", env)
		}
	}

	// Keep the agent alive after task completion to allow followups.
	args = append(args, "--idle-on-complete")

	return args
}
