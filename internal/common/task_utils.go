package common

import (
	"encoding/json"
	"strings"

	"github.com/warpdotdev/warp-agent-worker/internal/sources"
	"github.com/warpdotdev/warp-agent-worker/internal/types"
)

// EffectivePromptForTask returns the prompt that should be passed to `warp agent run --prompt`.
//
// If the task has a BasePrompt in AgentConfigSnapshot, we prepend it to the task prompt.
// This is a best-effort approximation of a separate "system/base prompt" channel, since the
// CLI only accepts a single prompt string.
func EffectivePromptForTask(task *types.Task) string {
	if task == nil {
		return ""
	}

	taskPrompt := task.Definition.Prompt
	if task.AgentConfigSnapshot == nil || task.AgentConfigSnapshot.BasePrompt == nil {
		return taskPrompt
	}

	basePrompt := strings.TrimSpace(*task.AgentConfigSnapshot.BasePrompt)
	if basePrompt == "" {
		return taskPrompt
	}

	// If the task prompt is empty, the base prompt becomes the whole prompt.
	if strings.TrimSpace(taskPrompt) == "" {
		return basePrompt
	}

	// Avoid duplicating the base prompt for scheduled tasks where taskPrompt is already basePrompt.
	if strings.TrimSpace(taskPrompt) == basePrompt {
		return taskPrompt
	}

	return basePrompt + "\n\n" + taskPrompt
}

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

		// Prefer emitting a single JSON blob for all MCP servers.
		if len(task.AgentConfigSnapshot.MCPServers) > 0 {
			b, err := json.Marshal(task.AgentConfigSnapshot.MCPServers)
			if err == nil {
				args = append(args, "--mcp", string(b))
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
	// This is disabled for scheduled agents, since they may run frequently,
	// and we do not expect a user to be present.
	if _, ok := task.TaskSource.(*sources.ScheduledAgentTaskSource); !ok {
		args = append(args, "--idle-on-complete")
	}

	return args
}
