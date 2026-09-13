package common

import (
	"encoding/json"
	"reflect"
	"strings"
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
