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

func TestDirectBackendSetupCommandDoesNotReadHostGlobalGitConfig(t *testing.T) {
	requireGit(t)

	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)

	testDir := t.TempDir()
	originPath := filepath.Join(testDir, "origin.git")
	runGit(t, nil, "init", "--bare", originPath)

	rewriteURL := "rewrite-test://repo"
	fileURL := gitFileURL(originPath)
	runGit(t, []string{"HOME=" + hostHome}, "config", "--global", "--add", "url."+fileURL+".insteadOf", rewriteURL)

	ozPath := filepath.Join(testDir, "oz")
	if err := os.WriteFile(ozPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("failed to write fake oz script: %v", err)
	}

	workspaceRoot := filepath.Join(testDir, "workspaces")
	backend, err := NewDirectBackend(context.Background(), DirectBackendConfig{
		WorkspaceRoot: workspaceRoot,
		OzPath:        ozPath,
		SetupCommand:  `git ls-remote "$TEST_REWRITE_URL"`,
		Env: map[string]string{
			"TEST_REWRITE_URL": rewriteURL,
		},
	})
	if err != nil {
		t.Fatalf("failed to create direct backend: %v", err)
	}

	if result := backend.ExecuteTask(context.Background(), &TaskParams{TaskID: "task-unseeded"}); result.Error == nil {
		t.Fatal("expected setup git command to fail because host global git config is hidden")
	}

	backend, err = NewDirectBackend(context.Background(), DirectBackendConfig{
		WorkspaceRoot: workspaceRoot,
		OzPath:        ozPath,
		NoCleanup:     true,
		SetupCommand: strings.Join([]string{
			`git config --global --add url."$TEST_FILE_REPO_URL".insteadOf "$TEST_REWRITE_URL"`,
			`git ls-remote "$TEST_REWRITE_URL"`,
		}, "\n"),
		Env: map[string]string{
			"TEST_FILE_REPO_URL": fileURL,
			"TEST_REWRITE_URL":   rewriteURL,
		},
	})
	if err != nil {
		t.Fatalf("failed to create seeded direct backend: %v", err)
	}
	if result := backend.ExecuteTask(context.Background(), &TaskParams{TaskID: "task-seeded"}); result.Error != nil {
		t.Fatalf("expected setup git command to succeed after seeding isolated git config: %v", result.Error)
	}
}

func TestDirectBackendSetupCommandReceivesIsolatedGitConfig(t *testing.T) {
	requireGit(t)

	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)

	testDir := t.TempDir()
	cfgCapture := filepath.Join(testDir, "setup_git_config_global.txt")
	ozPath := filepath.Join(testDir, "oz")
	if err := os.WriteFile(ozPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("failed to write fake oz script: %v", err)
	}

	workspaceRoot := filepath.Join(testDir, "workspaces")
	backend, err := NewDirectBackend(context.Background(), DirectBackendConfig{
		WorkspaceRoot: workspaceRoot,
		OzPath:        ozPath,
		NoCleanup:     true,
		SetupCommand: strings.Join([]string{
			`printf '%s' "$GIT_CONFIG_GLOBAL" > "$SETUP_CFG_CAPTURE"`,
			`git config --global user.email setup@example.com`,
		}, "\n"),
		Env: map[string]string{
			"SETUP_CFG_CAPTURE": cfgCapture,
		},
	})
	if err != nil {
		t.Fatalf("failed to create direct backend: %v", err)
	}
	if result := backend.ExecuteTask(context.Background(), &TaskParams{TaskID: "task-setup"}); result.Error != nil {
		t.Fatalf("failed to execute task: %v", result.Error)
	}

	wantCfg := filepath.Join(workspaceRoot, "task-setup", ".gitconfig")
	if got, err := os.ReadFile(cfgCapture); err != nil {
		t.Fatalf("failed to read captured setup GIT_CONFIG_GLOBAL: %v", err)
	} else if string(got) != wantCfg {
		t.Fatalf("setup GIT_CONFIG_GLOBAL = %q, want %q", string(got), wantCfg)
	}
	data, err := os.ReadFile(wantCfg)
	if err != nil {
		t.Fatalf("isolated git config was not written by setup: %v", err)
	}
	if !strings.Contains(string(data), "setup@example.com") {
		t.Fatalf("isolated git config missing setup user.email:\\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(hostHome, ".gitconfig")); !os.IsNotExist(err) {
		t.Fatalf("host ~/.gitconfig should not exist; stat err = %v", err)
	}
}

func TestDirectBackendSetupEnvFileCannotOverrideGitConfigGlobal(t *testing.T) {
	testDir := t.TempDir()
	mainCfgCapture := filepath.Join(testDir, "main_git_config_global.txt")
	overrideCfg := filepath.Join(testDir, "override.gitconfig")
	ozPath := filepath.Join(testDir, "oz")
	script := `#!/bin/sh
set -eu
printf '%s' "$GIT_CONFIG_GLOBAL" > "$MAIN_CFG_CAPTURE"
git config --global user.email main@example.com
`
	if err := os.WriteFile(ozPath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake oz script: %v", err)
	}

	workspaceRoot := filepath.Join(testDir, "workspaces")
	backend, err := NewDirectBackend(context.Background(), DirectBackendConfig{
		WorkspaceRoot: workspaceRoot,
		OzPath:        ozPath,
		NoCleanup:     true,
		SetupCommand:  `printf 'GIT_CONFIG_GLOBAL=%s\n' "$OVERRIDE_CFG" > "$OZ_ENVIRONMENT_FILE"`,
		Env: map[string]string{
			"MAIN_CFG_CAPTURE": mainCfgCapture,
			"OVERRIDE_CFG":     overrideCfg,
		},
	})
	if err != nil {
		t.Fatalf("failed to create direct backend: %v", err)
	}
	if result := backend.ExecuteTask(context.Background(), &TaskParams{TaskID: "task-envfile"}); result.Error != nil {
		t.Fatalf("failed to execute task: %v", result.Error)
	}

	wantCfg := filepath.Join(workspaceRoot, "task-envfile", ".gitconfig")
	if got, err := os.ReadFile(mainCfgCapture); err != nil {
		t.Fatalf("failed to read captured main GIT_CONFIG_GLOBAL: %v", err)
	} else if string(got) != wantCfg {
		t.Fatalf("main GIT_CONFIG_GLOBAL = %q, want %q", string(got), wantCfg)
	}
	data, err := os.ReadFile(wantCfg)
	if err != nil {
		t.Fatalf("isolated git config was not written by main oz process: %v", err)
	}
	if !strings.Contains(string(data), "main@example.com") {
		t.Fatalf("isolated git config missing main user.email:\\n%s", data)
	}
	if _, err := os.Stat(overrideCfg); !os.IsNotExist(err) {
		t.Fatalf("setup-provided GIT_CONFIG_GLOBAL override should not be used; stat err = %v", err)
	}
}

func TestDirectBackendGitConfigIsolationSmoke(t *testing.T) {
	requireGit(t)

	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)

	testDir := t.TempDir()
	originPath := filepath.Join(testDir, "origin.git")
	runGit(t, nil, "init", "--bare", originPath)

	rewriteURL := "rewrite-smoke://repo"
	fileURL := gitFileURL(originPath)
	ozPath := filepath.Join(testDir, "oz")
	script := `#!/bin/sh
set -eu
git ls-remote "$TEST_REWRITE_URL"
`
	if err := os.WriteFile(ozPath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake oz script: %v", err)
	}

	workspaceRoot := filepath.Join(testDir, "workspaces")
	backend, err := NewDirectBackend(context.Background(), DirectBackendConfig{
		WorkspaceRoot: workspaceRoot,
		OzPath:        ozPath,
		NoCleanup:     true,
		SetupCommand:  `git config --global --add url."$TEST_FILE_REPO_URL".insteadOf "$TEST_REWRITE_URL"`,
		Env: map[string]string{
			"TEST_FILE_REPO_URL": fileURL,
			"TEST_REWRITE_URL":   rewriteURL,
		},
	})
	if err != nil {
		t.Fatalf("failed to create direct backend: %v", err)
	}
	if result := backend.ExecuteTask(context.Background(), &TaskParams{TaskID: "task-smoke"}); result.Error != nil {
		t.Fatalf("expected main oz git command to use setup-seeded isolated git config: %v", result.Error)
	}

	wantCfg := filepath.Join(workspaceRoot, "task-smoke", ".gitconfig")
	data, err := os.ReadFile(wantCfg)
	if err != nil {
		t.Fatalf("isolated git config was not written: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(data)), "insteadof") {
		t.Fatalf("isolated git config missing rewrite seeded by setup:\\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(hostHome, ".gitconfig")); !os.IsNotExist(err) {
		t.Fatalf("host ~/.gitconfig should not exist; stat err = %v", err)
	}
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

	if result := backend.ExecuteTask(context.Background(), &TaskParams{TaskID: "task-1"}); result.Error != nil {
		t.Fatalf("failed to execute task: %v", result.Error)
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
		if result := backend.ExecuteTask(context.Background(), &TaskParams{TaskID: badID}); result.Error == nil {
			t.Fatalf("expected error for unsafe task ID %q, got nil", badID)
		}
	}
	if _, err := os.Stat(filepath.Join(testDir, "escape")); !os.IsNotExist(err) {
		t.Fatalf("traversal path should not exist; stat err = %v", err)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
}

func gitFileURL(path string) string {
	return "file://" + filepath.ToSlash(path)
}

func runGit(t *testing.T, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), env...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
}
