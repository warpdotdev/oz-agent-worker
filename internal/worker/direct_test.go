package worker

import (
	"context"
	"os"
	"os/exec"
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

func TestDirectBackendRedirectsGlobalGitConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}

	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)

	testDir := t.TempDir()
	homeCapture := filepath.Join(testDir, "home.txt")
	cfgCapture := filepath.Join(testDir, "git_config_global.txt")
	ozPath := filepath.Join(testDir, "oz")
	script := `#!/bin/sh
set -eu
printf '%s' "$HOME" > "$OZ_HOME_CAPTURE"
printf '%s' "$GIT_CONFIG_GLOBAL" > "$OZ_CFG_CAPTURE"
git config --global --add url."https://x-access-token:tok@github.com/".insteadOf "ssh://git@github.com/"
`
	if err := os.WriteFile(ozPath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake oz script: %v", err)
	}

	workspaceRoot := filepath.Join(testDir, "workspaces")
	backend, err := NewDirectBackend(context.Background(), DirectBackendConfig{
		WorkspaceRoot: workspaceRoot,
		OzPath:        ozPath,
		NoCleanup:     true,
		Env: map[string]string{
			"OZ_HOME_CAPTURE": homeCapture,
			"OZ_CFG_CAPTURE":  cfgCapture,
		},
	})
	if err != nil {
		t.Fatalf("failed to create direct backend: %v", err)
	}

	if err := backend.ExecuteTask(context.Background(), &TaskParams{TaskID: "task-1"}); err != nil {
		t.Fatalf("failed to execute task: %v", err)
	}

	if got, err := os.ReadFile(homeCapture); err != nil {
		t.Fatalf("failed to read captured HOME: %v", err)
	} else if string(got) != hostHome {
		t.Fatalf("HOME = %q, want host home %q (HOME must not be repointed)", string(got), hostHome)
	}

	wantCfg := filepath.Join(workspaceRoot, "task-1", ".gitconfig")
	if got, err := os.ReadFile(cfgCapture); err != nil {
		t.Fatalf("failed to read captured GIT_CONFIG_GLOBAL: %v", err)
	} else if string(got) != wantCfg {
		t.Fatalf("GIT_CONFIG_GLOBAL = %q, want %q", string(got), wantCfg)
	}
	data, err := os.ReadFile(wantCfg)
	if err != nil {
		t.Fatalf("isolated git config was not written: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(data)), "insteadof") {
		t.Fatalf("isolated git config missing insteadOf rewrite:\n%s", data)
	}

	if _, err := os.Stat(filepath.Join(hostHome, ".gitconfig")); !os.IsNotExist(err) {
		t.Fatalf("host ~/.gitconfig should not exist; stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(hostHome, ".config", "git", "config")); !os.IsNotExist(err) {
		t.Fatalf("host XDG git config should not exist; stat err = %v", err)
	}
}

func TestDirectBackendRejectsUnsafeTaskID(t *testing.T) {
	testDir := t.TempDir()
	ozPath := filepath.Join(testDir, "oz")
	if err := os.WriteFile(ozPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("failed to write fake oz script: %v", err)
	}

	workspaceRoot := filepath.Join(testDir, "workspaces")
	backend, err := NewDirectBackend(context.Background(), DirectBackendConfig{
		WorkspaceRoot: workspaceRoot,
		OzPath:        ozPath,
	})
	if err != nil {
		t.Fatalf("failed to create direct backend: %v", err)
	}

	for _, badID := range []string{"../escape", "a/../../escape", "/abs/path", "..", ""} {
		if err := backend.ExecuteTask(context.Background(), &TaskParams{TaskID: badID}); err == nil {
			t.Fatalf("expected error for unsafe task ID %q, got nil", badID)
		}
	}
	if _, err := os.Stat(filepath.Join(testDir, "escape")); !os.IsNotExist(err) {
		t.Fatalf("traversal path should not exist; stat err = %v", err)
	}
}
