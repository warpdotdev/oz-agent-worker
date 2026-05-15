package worker

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func TestDirectHarnessEnv(t *testing.T) {
	workspaceDir := filepath.Join(string(filepath.Separator), "tmp", "workspace")

	tests := []struct {
		name    string
		harness *string
		want    map[string]string
	}{
		{
			name:    "claude",
			harness: stringPtr("claude"),
			want: map[string]string{
				"CLAUDE_CONFIG_DIR": filepath.Join(workspaceDir, ".claude"),
			},
		},
		{
			name:    "codex",
			harness: stringPtr("codex"),
			want: map[string]string{
				"CODEX_HOME": filepath.Join(workspaceDir, ".codex"),
			},
		},
		{
			name:    "unknown",
			harness: stringPtr("other"),
			want:    map[string]string{},
		},
		{
			name:    "missing",
			harness: nil,
			want:    map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := &TaskParams{Task: &types.Task{AgentConfigSnapshot: &types.AmbientAgentConfig{}}}
			if tt.harness != nil {
				params.Task.AgentConfigSnapshot.Harness = &types.Harness{Type: tt.harness}
			}

			got := envMap(harnessEnvVars(workspaceDir, params))
			if len(got) != len(tt.want) {
				t.Fatalf("env count = %d, want %d: %#v", len(got), len(tt.want), got)
			}
			for key, want := range tt.want {
				if got[key] != want {
					t.Fatalf("%s = %q, want %q", key, got[key], want)
				}
			}
		})
	}
}

func stringPtr(value string) *string {
	return &value
}

func envMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		key, val, _ := strings.Cut(value, "=")
		result[key] = val
	}
	return result
}
