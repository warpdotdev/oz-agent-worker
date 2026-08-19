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
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete", "15m"},
		},
		{
			name: "falls back to worker idle_on_complete when task timeout not set",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			opts:     TaskAugmentOptions{IdleOnComplete: "30m"},
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete", "30m"},
		},
		{
			name: "uses oz cli default when neither task nor worker timeout is set",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete"},
		},
		{
			name: "ignores non-positive task idle_timeout_minutes and falls back to worker value",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					IdleTimeoutMinutes: intPtr(0),
				},
			},
			opts:     TaskAugmentOptions{IdleOnComplete: "20m"},
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete", "20m"},
		},
		{
			name: "adds --harness when harness type is set",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					Harness: &types.Harness{Type: strPtr("claude")},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--harness", "claude", "--idle-on-complete"},
		},
		{
			name: "skips --harness when harness type is nil",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					Harness: &types.Harness{},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete"},
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
			expected: []string{"agent", "run", "--model", "claude-sonnet-4", "--computer-use", "--idle-on-complete", "12m"},
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
				"--computer-use",
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
				"--computer-use",
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
				"--computer-use",
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
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete"},
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
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete"},
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
			expected: []string{"agent", "run", "--computer-use", "--share", "public:view", "--idle-on-complete"},
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
			expected: []string{"agent", "run", "--computer-use", "--share", "public:edit", "--idle-on-complete"},
		},
		{
			name: "skips --share public when session_sharing is absent",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete"},
		},
		{
			name: "does not forward --conversation even when AgentConversationID is set; the embedded warp CLI reads it off task metadata",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
				AgentConversationID: strPtr("abc-123"),
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete"},
		},
		{
			name: "skips --share public when public_access is nil",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					SessionSharing: &types.SessionSharingConfig{},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete"},
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
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete"},
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
				"--computer-use",
				"--no-snapshot",
				"--snapshot-upload-timeout", "90s",
				"--snapshot-script-timeout", "45s",
				"--idle-on-complete",
			},
		},
		{
			name: "appends supplemental oz args before idle timeout",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			opts:     TaskAugmentOptions{AdditionalOzArgs: []string{"--skip-initial-turn"}},
			expected: []string{"agent", "run", "--computer-use", "--skip-initial-turn", "--idle-on-complete"},
		},
		{
			name: "does not emit supplemental oz args when none are provided",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete"},
		},
		{
			name: "adds --computer-use by default when computer_use_enabled is unset",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete"},
		},
		{
			name: "adds --computer-use when computer_use_enabled is true",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ComputerUseEnabled: boolPtr(true),
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete"},
		},
		{
			name: "adds --no-computer-use when computer_use_enabled is false",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ComputerUseEnabled: boolPtr(false),
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--no-computer-use", "--idle-on-complete"},
		},
		{
			name: "adds --computer-use by default for a third-party harness",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					Harness: &types.Harness{Type: strPtr("codex")},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--harness", "codex", "--idle-on-complete"},
		},
		{
			name: "adds --computer-use for a third-party harness when explicitly enabled",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ComputerUseEnabled: boolPtr(true),
					Harness:            &types.Harness{Type: strPtr("codex")},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--harness", "codex", "--idle-on-complete"},
		},
		{
			name: "adds --no-computer-use for a third-party harness when explicitly disabled",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ComputerUseEnabled: boolPtr(false),
					Harness:            &types.Harness{Type: strPtr("codex")},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--no-computer-use", "--harness", "codex", "--idle-on-complete"},
		},
		{
			// The top-level model_id targets the Oz harness. Third-party harnesses
			// resolve their model from the task snapshot's harness config, so leaking
			// the Oz model id via --model would cause them to reject the run.
			name: "does not forward Oz top-level model_id to a third-party harness",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ModelID: strPtr("claude-4-8-opus-high"),
					Harness: &types.Harness{Type: strPtr("claude")},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--harness", "claude", "--idle-on-complete"},
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

// TestAugmentArgsForTask_ComputerUseModel covers the emission gating for
// --computer-use-model. The flag is newer than the agent CLI some pinned
// workers run, and an unknown flag fails a run at startup, so it is emitted
// only when the run actually configures a model: computer use on, Oz harness,
// non-empty value. Every other case must produce the same args as before the
// field existed.
func TestAugmentArgsForTask_ComputerUseModel(t *testing.T) {
	baseArgs := []string{"agent", "run"}

	tests := []struct {
		name     string
		task     *types.Task
		opts     TaskAugmentOptions
		expected []string
	}{
		{
			name: "emits --computer-use-model for an Oz run that pinned a model",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ComputerUseModelID: strPtr("claude-4-5-haiku"),
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--computer-use-model", "claude-4-5-haiku", "--idle-on-complete"},
		},
		{
			name: "emits --computer-use-model when computer use is explicitly enabled",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ComputerUseEnabled: boolPtr(true),
					ComputerUseModelID: strPtr("claude-4-5-haiku"),
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--computer-use-model", "claude-4-5-haiku", "--idle-on-complete"},
		},
		{
			// An absent harness is the Oz harness, which is the common case for a
			// factory-configured run; IsOz treats nil as Oz.
			name: "emits --computer-use-model for an explicit oz harness",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ComputerUseModelID: strPtr("claude-4-5-haiku"),
					Harness:            &types.Harness{Type: strPtr("oz")},
				},
			},
			opts: TaskAugmentOptions{},
			expected: []string{
				"agent", "run",
				"--computer-use",
				"--computer-use-model", "claude-4-5-haiku",
				"--harness", "oz",
				"--idle-on-complete",
			},
		},
		{
			// IsOz treats four shapes as Oz: a nil Harness, a Harness with a nil
			// Type, a Type that is the empty string, and an explicit "oz". This case
			// and the next pin the two middle forms, which a real snapshot can carry
			// and which the other cases here do not reach. Were either to stop
			// counting as Oz, the pin would silently vanish and the run would fall
			// back to the automatic computer use model — the bug this emission fixes.
			name: "emits --computer-use-model when the harness block carries no type",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ComputerUseModelID: strPtr("claude-4-5-haiku"),
					Harness:            &types.Harness{},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--computer-use-model", "claude-4-5-haiku", "--idle-on-complete"},
		},
		{
			name: "emits --computer-use-model when the harness type is empty",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ComputerUseModelID: strPtr("claude-4-5-haiku"),
					Harness:            &types.Harness{Type: strPtr("")},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--computer-use-model", "claude-4-5-haiku", "--idle-on-complete"},
		},
		{
			name: "emits --computer-use-model alongside the Oz --model",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ModelID:            strPtr("auto"),
					ComputerUseModelID: strPtr("claude-4-5-haiku"),
				},
			},
			opts: TaskAugmentOptions{},
			expected: []string{
				"agent", "run",
				"--model", "auto",
				"--computer-use",
				"--computer-use-model", "claude-4-5-haiku",
				"--idle-on-complete",
			},
		},
		{
			name: "trims the pinned model before emitting it",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ComputerUseModelID: strPtr("  claude-4-5-haiku  "),
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--computer-use-model", "claude-4-5-haiku", "--idle-on-complete"},
		},
		{
			// The byte-identical baseline: an unconfigured snapshot must produce
			// exactly the args it produced before this field existed, which is what
			// keeps older pinned agent CLIs working.
			name: "omits --computer-use-model when no model is pinned",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete"},
		},
		{
			name: "omits --computer-use-model when the pinned model is whitespace",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ComputerUseModelID: strPtr("   "),
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete"},
		},
		{
			// The subagent the model configures never runs, so emitting the flag
			// would only risk an unknown-argument failure for no behavior change.
			name: "omits --computer-use-model when computer use is disabled",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ComputerUseEnabled: boolPtr(false),
					ComputerUseModelID: strPtr("claude-4-5-haiku"),
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--no-computer-use", "--idle-on-complete"},
		},
		{
			name: "omits --computer-use-model under a third-party harness",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					ComputerUseModelID: strPtr("claude-4-5-haiku"),
					Harness:            &types.Harness{Type: strPtr("codex")},
				},
			},
			opts:     TaskAugmentOptions{},
			expected: []string{"agent", "run", "--computer-use", "--harness", "codex", "--idle-on-complete"},
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
