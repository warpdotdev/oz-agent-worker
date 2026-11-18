package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func privateHomeAssignment(t *testing.T) *types.TaskAssignmentMessage {
	t.Helper()
	var assignment types.TaskAssignmentMessage
	err := json.Unmarshal([]byte(`{
		"task_id":"child", "execution_id":"12", "task":{"id":"child"},
		"use_private_restricted_git_home":true,
		"env_vars":{"WARP_GITHUB_CREDENTIAL_MODE":"restricted_installation",
			"WARP_API_KEY":"fixture-api", "WARP_WORKLOAD_TOKEN":"fixture-workload", "WARP_EXECUTION_ID":"12"}
	}`), &assignment)
	if err != nil {
		t.Fatal(err)
	}
	return &assignment
}

func privateHomeWorker(t *testing.T, script string, cfg DirectBackendConfig) (*Worker, *DirectBackend) {
	t.Helper()
	var err error
	cfg.WorkspaceRoot, err = filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.OzPath = filepath.Join(t.TempDir(), "oz")
	if err := os.WriteFile(cfg.OzPath, []byte("#!/bin/sh\nset -eu\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := NewDirectBackend(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &Worker{ctx: t.Context(), config: Config{BackendType: "direct", Direct: &cfg}}, b
}

func TestDirectPrivateHomeWireToLaunch(t *testing.T) {
	requireGit(t)
	personal := t.TempDir()
	t.Setenv("HOME", personal)
	t.Setenv("GH_TOKEN", "fixture-host")
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(personal, "fixture.sock"))
	t.Setenv("WARP_API_KEY", "fixture-worker")
	t.Setenv("GIT_CONFIG_PARAMETERS", "host-only")
	marker := filepath.Join(personal, ".gitconfig")
	original := "[user]\n\tname = Fixture Developer\n"
	if err := os.WriteFile(marker, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "home")
	const checkEnv = `
test "$GIT_CONFIG_GLOBAL" = "$HOME/.gitconfig"
test "$GH_CONFIG_DIR" = "$HOME/.config/gh"
test "$GIT_SSH_COMMAND" = false
test -z "${GH_TOKEN+x}${GITHUB_TOKEN+x}${SSH_AUTH_SOCK+x}${GIT_CONFIG_PARAMETERS+x}"
test "$WARP_GITHUB_CREDENTIAL_MODE" = restricted_installation
`
	const checkTask = `
test "$WARP_API_KEY" = fixture-api && test "$WARP_WORKLOAD_TOKEN" = fixture-workload
test "$WARP_EXECUTION_ID" = 12
`
	w, b := privateHomeWorker(t, checkEnv+checkTask+`
test "$1 $2" = "agent run"
test "$5" = "--sandboxed"
test "$SETUP_OK" = yes
git init -q
test "$(git config user.name)" = "Fixture Bot"
git -c user.email=bot@example.invalid commit -q --allow-empty -m example
printf '%s' "$HOME" > "$RECORD"
`, DirectBackendConfig{
		Env: map[string]string{"RECORD": record, "GH_TOKEN": "fixture-operator", "GH_CONFIG_DIR": personal},
		SetupCommand: "set -eu\n" + checkEnv + checkTask + `
test ! -e "$GIT_CONFIG_GLOBAL"
git config --global user.name "Fixture Bot"
printf 'SETUP_OK=yes\n' > "$OZ_ENVIRONMENT_FILE"
`,
		TeardownCommand: "set -eu\n" + checkEnv + `
test -z "${WARP_API_KEY+x}"
test "$(git config user.name)" = "Fixture Bot"
printf 'ok' > "` + record + `.teardown"
`,
	})
	assignment := privateHomeAssignment(t)
	assignment.EnvVars["GITHUB_TOKEN"] = "fixture-assignment"
	if result := b.ExecuteTask(t.Context(), w.prepareTaskParams(assignment)); result.Error != nil {
		t.Fatal(result.Error)
	}
	data, err := os.ReadFile(record)
	if err != nil || string(data) == personal || filepath.Dir(string(data)) != b.config.WorkspaceRoot {
		t.Fatalf("task did not use a separate worker-owned home: %q (%v)", data, err)
	}
	if _, err := os.Stat(string(data)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private home was not removed: %v", err)
	}
	if _, err := os.Stat(record + ".teardown"); err != nil {
		t.Fatalf("teardown did not see private config: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != original {
		t.Fatal("developer config changed")
	}
}

func TestDirectPrivateHomeRejectsUnsafeAssignments(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name   string
		change func(*TaskParams, *DirectBackendConfig)
	}{
		{"legacy execution", func(p *TaskParams, _ *DirectBackendConfig) { p.ExecutionID = "" }},
		{"standby", func(p *TaskParams, _ *DirectBackendConfig) { p.StandbyID = "standby" }},
		{"shared target", func(_ *TaskParams, cfg *DirectBackendConfig) { cfg.TargetDir = cfg.WorkspaceRoot }},
		{"absent assignment mode", func(p *TaskParams, cfg *DirectBackendConfig) {
			p.EnvVars = nil
			cfg.Env = map[string]string{"WARP_GITHUB_CREDENTIAL_MODE": "restricted_installation"}
		}},
		{"task HOME", func(p *TaskParams, _ *DirectBackendConfig) { p.EnvVars = append(p.EnvVars, "HOME=/invalid") }},
		{"worker global path", func(_ *TaskParams, cfg *DirectBackendConfig) {
			cfg.Env = map[string]string{"GIT_CONFIG_GLOBAL": "/invalid"}
		}},
		{"worker mode", func(_ *TaskParams, cfg *DirectBackendConfig) {
			cfg.Env = map[string]string{"WARP_GITHUB_CREDENTIAL_MODE": "legacy"}
		}},
		{"setup config", func(_ *TaskParams, cfg *DirectBackendConfig) {
			cfg.SetupCommand = `printf 'GIT_CONFIG_COUNT=1\n' > "$OZ_ENVIRONMENT_FILE"`
		}},
		{"setup mode", func(_ *TaskParams, cfg *DirectBackendConfig) {
			cfg.SetupCommand = `printf 'WARP_GITHUB_CREDENTIAL_MODE=legacy\n' > "$OZ_ENVIRONMENT_FILE"`
		}},
		{"setup authentication", func(_ *TaskParams, cfg *DirectBackendConfig) {
			cfg.SetupCommand = `printf 'WARP_API_KEY=fixture-other\n' > "$OZ_ENVIRONMENT_FILE"`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, b := privateHomeWorker(t, "exit 0\n", DirectBackendConfig{})
			params := w.prepareTaskParams(privateHomeAssignment(t))
			tc.change(params, &b.config)
			if result := b.ExecuteTask(t.Context(), params); result.Error == nil {
				t.Fatal("unsafe assignment ran")
			}
			entries, err := os.ReadDir(b.config.WorkspaceRoot)
			if err != nil || len(entries) != 0 {
				t.Fatalf("private directory left after rejection: %v %v", entries, err)
			}
		})
	}
}

func TestDirectUnmarkedAssignmentKeepsHome(t *testing.T) {
	personal := t.TempDir()
	t.Setenv("HOME", personal)
	w, b := privateHomeWorker(t, `
test "$HOME" = "$ORIGINAL_HOME"
test "$GIT_CONFIG_GLOBAL" != "$HOME/.gitconfig"
test "$GIT_CONFIG_GLOBAL" = "$PWD/.gitconfig"
`, DirectBackendConfig{Env: map[string]string{"ORIGINAL_HOME": personal}})
	var assignment types.TaskAssignmentMessage
	if err := json.Unmarshal([]byte(`{"task_id":"run","task":{"id":"run"},"execution_id":"12",
		"env_vars":{"WARP_GITHUB_CREDENTIAL_MODE":"restricted_installation","use_private_restricted_git_home":"true"}}`), &assignment); err != nil {
		t.Fatal(err)
	}
	if result := b.ExecuteTask(t.Context(), w.prepareTaskParams(&assignment)); result.Error != nil {
		t.Fatal(result.Error)
	}
}

func TestDirectPrivateHomeIndependentOfWorkspaceRetention(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	record := t.TempDir()
	w, b := privateHomeWorker(t, `
printf '%s\n%s\n' "$HOME" "$PWD" > "$RECORD/$WARP_EXECUTION_ID.tmp"
mv "$RECORD/$WARP_EXECUTION_ID.tmp" "$RECORD/$WARP_EXECUTION_ID"
case "$OUTCOME" in cancel) exec sleep 60;; failure) exit 1;; esac
`, DirectBackendConfig{Env: map[string]string{"RECORD": record}, NoCleanup: true})
	seen := map[string]bool{}
	for i, outcome := range []string{"cancel", "success", "failure"} {
		assignment := privateHomeAssignment(t)
		id := strconv.Itoa(i + 1)
		assignment.ExecutionID = id
		assignment.EnvVars["WARP_EXECUTION_ID"] = id
		assignment.EnvVars["OUTCOME"] = outcome
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		done := make(chan ExecuteResult, 1)
		go func() { done <- b.ExecuteTask(ctx, w.prepareTaskParams(assignment)) }()
		deadline := time.Now().Add(5 * time.Second)
		var data []byte
		for {
			var err error
			data, err = os.ReadFile(filepath.Join(record, id))
			if err == nil {
				break
			}
			if !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
				t.Fatalf("launch marker unavailable: %v", err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		parts := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(parts) != 2 || seen[parts[0]] || filepath.Dir(parts[0]) != filepath.Dir(parts[1]) || parts[0] == parts[1] {
			t.Fatal("expected independent sibling home and workspace directories")
		}
		seen[parts[0]] = true
		checkCleanup := func() {
			t.Helper()
			select {
			case result := <-done:
				if (result.Error != nil) != (outcome != "success") {
					t.Fatalf("unexpected %s result: %v", outcome, result.Error)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not end the execution")
			}
			if _, err := os.Stat(parts[0]); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("private home still present after %s", outcome)
			}
			if _, err := os.Stat(parts[1]); err != nil {
				t.Fatalf("workspace not retained after %s: %v", outcome, err)
			}
		}
		if outcome == "cancel" {
			fi, err := os.Stat(parts[0])
			if err != nil || fi.Mode().Perm() != 0o700 {
				t.Fatal("active home is not owner-only")
			}
			// Keep this execution running while the same logical task runs twice more.
			defer func() { cancel(); checkCleanup() }()
		} else {
			checkCleanup()
			cancel()
		}
	}
}
