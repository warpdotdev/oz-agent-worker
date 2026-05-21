package common

import (
	"reflect"
	"testing"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func strPtr(v string) *string                          { return &v }
func intPtr(v int) *int                                { return &v }
func boolPtr(v bool) *bool                             { return &v }
func accessPtr(v types.AccessLevel) *types.AccessLevel { return &v }

func TestAugmentArgsForTask_IdleOnCompletePrecedence(t *testing.T) {
	baseArgs := []string{"agent", "run"}

	tests := []struct {
		name           string
		task           *types.Task
		executionInput *types.WorkerExecutionInput
		opts           TaskAugmentOptions
		expected       []string
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
		{
			name: "passes --bedrock-inference-role when inference_providers.aws.role_arn is set",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ModelID: strPtr("claude-sonnet-4"),
					InferenceProviders: &types.InferenceProviders{
						Aws: &types.AwsInferenceProvider{
							RoleARN: "arn:aws:iam::123456789012:role/BedrockInference",
						},
					},
				},
			},
			opts: TaskAugmentOptions{},
			expected: []string{
				"agent",
				"run",
				"--model",
				"claude-sonnet-4",
				"--bedrock-inference-role",
				"arn:aws:iam::123456789012:role/BedrockInference",
				"--idle-on-complete",
			},
		},
		{
			name: "pairs --bedrock-role-region with --bedrock-inference-role when region is set",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					InferenceProviders: &types.InferenceProviders{
						Aws: &types.AwsInferenceProvider{
							RoleARN: "arn:aws:iam::123456789012:role/BedrockInference",
							Region:  "  us-east-1  ",
						},
					},
				},
			},
			opts: TaskAugmentOptions{},
			expected: []string{
				"agent",
				"run",
				"--bedrock-inference-role",
				"arn:aws:iam::123456789012:role/BedrockInference",
				"--bedrock-role-region",
				"us-east-1",
				"--idle-on-complete",
			},
		},
		{
			name: "omits --bedrock-role-region when region is whitespace",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					InferenceProviders: &types.InferenceProviders{
						Aws: &types.AwsInferenceProvider{
							RoleARN: "arn:aws:iam::123456789012:role/BedrockInference",
							Region:  "   ",
						},
					},
				},
			},
			opts: TaskAugmentOptions{},
			expected: []string{
				"agent",
				"run",
				"--bedrock-inference-role",
				"arn:aws:iam::123456789012:role/BedrockInference",
				"--idle-on-complete",
			},
		},
		{
			name: "skips --bedrock-inference-role when role_arn is whitespace",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					InferenceProviders: &types.InferenceProviders{
						Aws: &types.AwsInferenceProvider{RoleARN: "   "},
					},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--idle-on-complete"},
		},
		{
			name: "skips --bedrock-inference-role when aws block is opted out",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					InferenceProviders: &types.InferenceProviders{
						Aws: &types.AwsInferenceProvider{
							Disabled: true,
							RoleARN:  "arn:aws:iam::123456789012:role/BedrockInference",
						},
					},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--idle-on-complete"},
		},
		{
			name: "adds --share public:view when session_sharing.public_access is VIEWER",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					SessionSharing: &types.SessionSharingConfig{
						PublicAccess: accessPtr(types.AccessLevelViewer),
					},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--share", "public:view", "--idle-on-complete"},
		},
		{
			name: "adds --share public:edit when session_sharing.public_access is EDITOR",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					SessionSharing: &types.SessionSharingConfig{
						PublicAccess: accessPtr(types.AccessLevelEditor),
					},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--share", "public:edit", "--idle-on-complete"},
		},
		{
			name: "skips --share public when session_sharing is absent",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--idle-on-complete"},
		},
		{
			name: "does not forward --conversation even when AgentConversationID is set; the embedded warp CLI reads it off task metadata",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
				AgentConversationID: strPtr("abc-123"),
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--idle-on-complete"},
		},
		{
			name: "skips --share public when public_access is nil",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					SessionSharing: &types.SessionSharingConfig{},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--idle-on-complete"},
		},
		{
			name: "silently omits --share public for unsupported access levels (defensive: FULL rejected earlier)",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					SessionSharing: &types.SessionSharingConfig{
						PublicAccess: accessPtr(types.AccessLevel("FULL")),
					},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--idle-on-complete"},
		},
		{
			name: "adds snapshot controls when configured",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					SnapshotDisabled:          boolPtr(true),
					SnapshotUploadTimeoutSecs: intPtr(90),
					SnapshotScriptTimeoutSecs: intPtr(45),
				},
			},
			opts: TaskAugmentOptions{},
			expected: []string{
				"agent", "run",
				"--no-snapshot",
				"--snapshot-upload-timeout", "90s",
				"--snapshot-script-timeout", "45s",
				"--idle-on-complete",
			},
		},
		{
			name: "emits --skip-initial-turn when execution prompt is empty and no snapshot token",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			executionInput: &types.WorkerExecutionInput{
				Prompt:               "",
				InitialSnapshotToken: nil,
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--skip-initial-turn", "--idle-on-complete"},
		},
		{
			name: "does not emit --skip-initial-turn when execution prompt is empty but snapshot token is set",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			executionInput: &types.WorkerExecutionInput{
				Prompt:               "",
				InitialSnapshotToken: strPtr("snap-token-abc"),
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--idle-on-complete"},
		},
		{
			name: "does not emit --skip-initial-turn when execution prompt is non-empty",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			executionInput: &types.WorkerExecutionInput{
				Prompt:               "do the thing",
				InitialSnapshotToken: nil,
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--idle-on-complete"},
		},
		{
			// Cloud->cloud-follow-up regression: same task, follow-up execution
			// arrives with a real prompt. The derivation must reflect the live
			// execution, not any flag the original empty-prompt execution may have
			// carried. Worker derives fresh per execution, so the follow-up never
			// emits --skip-initial-turn even though the first execution would have.
			name: "cloud->cloud follow-up: follow-up execution with prompt does not emit --skip-initial-turn",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			executionInput: &types.WorkerExecutionInput{
				Prompt:               "follow up question",
				InitialSnapshotToken: nil,
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--idle-on-complete"},
		},
		{
			name: "does not emit --skip-initial-turn when execution input is nil",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			executionInput: nil,
			opts:           TaskAugmentOptions{},
			expected:       []string{"agent", "run", "--idle-on-complete"},
		},
		{
			name: "treats empty-string snapshot token the same as nil",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			executionInput: &types.WorkerExecutionInput{
				Prompt:               "",
				InitialSnapshotToken: strPtr(""),
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--skip-initial-turn", "--idle-on-complete"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AugmentArgsForTask(tt.task, tt.executionInput, append([]string{}, baseArgs...), tt.opts)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Fatalf("args mismatch\n got: %#v\nwant: %#v", got, tt.expected)
			}
		})
	}
}
