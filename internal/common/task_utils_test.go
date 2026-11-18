package common

import (
	"encoding/json"
	"reflect"
	"strings"
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
			name: "task zero requests immediate shutdown instead of falling back to worker value",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					IdleTimeoutMinutes: intPtr(0),
				},
			},
			opts:     TaskAugmentOptions{IdleOnComplete: "20m"},
			expected: []string{"agent", "run", "--computer-use", "--idle-on-complete", "0m"},
		},
		{
			name: "ignores negative task idle_timeout_minutes and falls back to worker value",
			task: &types.Task{
				AgentConfigSnapshot: &types.AmbientAgentConfig{
					IdleTimeoutMinutes: intPtr(-1),
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

func TestAugmentArgsForTask_ComputerUseModel(t *testing.T) {
	task := &types.Task{
		AgentConfigSnapshot: &types.AmbientAgentConfig{
			ComputerUseEnabled: boolPtr(true),
			ComputerUseModelID: strPtr("claude-4-5-haiku"),
		},
	}

	got := AugmentArgsForTask(task, []string{"agent", "run"}, TaskAugmentOptions{})
	expected := []string{"agent", "run", "--computer-use", "--idle-on-complete"}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("args mismatch\n got: %#v\nwant: %#v", got, expected)
	}
}

func TestRepositoryHeadOverrideArgsForTask(t *testing.T) {
	marshal := func(t *testing.T, override types.RepositoryHeadOverride) string {
		t.Helper()
		b, err := json.Marshal(override)
		if err != nil {
			t.Fatalf("failed to marshal fixture override: %v", err)
		}
		return string(b)
	}

	t.Run("no overrides adds nothing", func(t *testing.T) {
		task := &types.Task{AgentConfigSnapshot: &types.AmbientAgentConfig{}}
		got := repositoryHeadOverrideArgsForTask(task)
		if got != nil {
			t.Fatalf("expected nil args, got: %#v", got)
		}
	})

	t.Run("single override without clone_from", func(t *testing.T) {
		override := types.RepositoryHeadOverride{
			CodeForge: "GITHUB",
			RepoOwner: "warpdotdev",
			RepoName:  "warp-server",
			Head:      types.RepositoryHeadRef{Type: types.RepositoryHeadTypeBranch, Value: "develop"},
		}
		task := &types.Task{
			AgentConfigSnapshot: &types.AmbientAgentConfig{
				RepositoryHeadOverrides: []types.RepositoryHeadOverride{override},
			},
		}
		expected := []string{
			"--repository-head-override-json", marshal(t, override),
			"--remove-repository-origins",
		}
		got := repositoryHeadOverrideArgsForTask(task)
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("args mismatch\n got: %#v\nwant: %#v", got, expected)
		}
	})

	t.Run("override with clone_from marshals the substitution fields", func(t *testing.T) {
		override := types.RepositoryHeadOverride{
			CodeForge: "GITHUB",
			RepoOwner: "warpdotdev",
			RepoName:  "warp",
			Head: types.RepositoryHeadRef{
				Type:  types.RepositoryHeadTypeCommitSHA,
				Value: "0123456789abcdef0123456789abcdef01234567",
			},
			CloneFrom: &types.RepositoryIdentity{
				CodeForge: "GITHUB",
				Owner:     "warpdotdev",
				Repo:      "warp-for-benchmarks",
			},
			PreserveOrigin: true,
		}
		task := &types.Task{
			AgentConfigSnapshot: &types.AmbientAgentConfig{
				RepositoryHeadOverrides: []types.RepositoryHeadOverride{override},
			},
		}
		got := repositoryHeadOverrideArgsForTask(task)
		jsonArg := marshal(t, override)
		if !strings.Contains(jsonArg, `"clone_from":{"code_forge":"GITHUB","owner":"warpdotdev","repo":"warp-for-benchmarks"}`) {
			t.Fatalf("fixture JSON missing expected clone_from shape: %s", jsonArg)
		}
		expected := []string{"--repository-head-override-json", jsonArg, "--remove-repository-origins"}
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("args mismatch\n got: %#v\nwant: %#v", got, expected)
		}
	})

	t.Run("multiple overrides emit one flag each and a single trailing remove-origins flag", func(t *testing.T) {
		first := types.RepositoryHeadOverride{
			CodeForge: "GITHUB", RepoOwner: "warpdotdev", RepoName: "warp",
			Head: types.RepositoryHeadRef{Type: types.RepositoryHeadTypeBranch, Value: "develop"},
		}
		second := types.RepositoryHeadOverride{
			CodeForge: "GITHUB", RepoOwner: "warpdotdev", RepoName: "warp-server",
			Head: types.RepositoryHeadRef{Type: types.RepositoryHeadTypeBranch, Value: "develop"},
		}
		task := &types.Task{
			AgentConfigSnapshot: &types.AmbientAgentConfig{
				RepositoryHeadOverrides: []types.RepositoryHeadOverride{first, second},
			},
		}
		expected := []string{
			"--repository-head-override-json", marshal(t, first),
			"--repository-head-override-json", marshal(t, second),
			"--remove-repository-origins",
		}
		got := repositoryHeadOverrideArgsForTask(task)
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("args mismatch\n got: %#v\nwant: %#v", got, expected)
		}
	})
}
