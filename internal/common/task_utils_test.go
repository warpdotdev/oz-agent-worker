package common

import (
	"reflect"
	"testing"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func strPtr(v string) *string { return &v }
func intPtr(v int) *int       { return &v }

func TestAugmentArgsForTask_IdleOnCompletePrecedence(t *testing.T) {
	baseArgs := []string{"agent", "run"}

	tests := []struct {
		name     string
		task     *types.Task
		opts     TaskAugmentOptions
		expected []string
	}{
		{
			name: "uses task idle_timeout_minutes when set",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					IdleTimeoutMinutes: intPtr(15),
				},
			},
			opts:     TaskAugmentOptions{IdleOnComplete: "30m"},
			expected: []string{"agent", "run", "--idle-on-complete", "15m"},
		},
		{
			name: "falls back to worker idle_on_complete when task timeout not set",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			opts:     TaskAugmentOptions{IdleOnComplete: "30m"},
			expected: []string{"agent", "run", "--idle-on-complete", "30m"},
		},
		{
			name: "uses oz cli default when neither task nor worker timeout is set",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--idle-on-complete"},
		},
		{
			name: "ignores non-positive task idle_timeout_minutes and falls back to worker value",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					IdleTimeoutMinutes: intPtr(0),
				},
			},
			opts:     TaskAugmentOptions{IdleOnComplete: "20m"},
			expected: []string{"agent", "run", "--idle-on-complete", "20m"},
		},
		{
			name: "adds --harness when harness type is set",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					Harness: &types.Harness{Type: strPtr("claude")},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--harness", "claude", "--idle-on-complete"},
		},
		{
			name: "skips --harness when harness type is nil",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					Harness: &types.Harness{},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--idle-on-complete"},
		},
		{
			name: "still appends other config-derived args before idle timeout",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ModelID:            strPtr("claude-sonnet-4"),
					IdleTimeoutMinutes: intPtr(12),
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--model", "claude-sonnet-4", "--idle-on-complete", "12m"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AugmentArgsForTask(tt.task, append([]string{}, baseArgs...), tt.opts)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Fatalf("args mismatch\n got: %#v\nwant: %#v", got, tt.expected)
			}
		})
	}
}
