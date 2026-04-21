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

		// Pass harness setting if explicitly configured.
		if task.AgentConfigSnapshot.Harness != nil && task.AgentConfigSnapshot.Harness.Type != nil {
			if harness := strings.TrimSpace(*task.AgentConfigSnapshot.Harness.Type); harness != "" {
				args = append(args, "--harness", harness)
			}
		}

		// Pass public session-sharing if configured. This is additive to the
		// collaborator-oriented --share args already set by the worker launcher
		// (team:edit). The bundled Warp client applies an anyone-with-link ACL
		// after the session bootstraps; the workspace-level
		// AnyoneWithLinkSharingEnabled setting still gates whether the ACL
		// write succeeds. FULL is rejected at the public API layer, so we only
		// expect VIEWER or EDITOR here and silently skip unsupported values
		// as a defensive fallback.
		if task.AgentConfigSnapshot.SessionSharing != nil &&
			task.AgentConfigSnapshot.SessionSharing.PublicAccess != nil {
			if level := shareAccessLevelForEmission(*task.AgentConfigSnapshot.SessionSharing.PublicAccess); level != "" {
				args = append(args, "--share", fmt.Sprintf("public:%s", level))
			}
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

// shareAccessLevelForEmission maps an internal AccessLevel to the string
// accepted by the CLI's --share flag. Returns empty string for values that
// the CLI cannot represent (e.g. FULL), in which case the caller should
// omit the emission entirely.
func shareAccessLevelForEmission(access types.AccessLevel) string {
	switch access {
	case types.AccessLevelViewer:
		return "view"
	case types.AccessLevelEditor:
		return "edit"
	default:
		return ""
	}
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
