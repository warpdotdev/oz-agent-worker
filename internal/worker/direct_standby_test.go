package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
)

func writeStandbyOzScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oz")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func newStandbyDirectBackend(t *testing.T, config DirectBackendConfig) *DirectBackend {
	t.Helper()
	backend, err := NewDirectBackend(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func standbyTaskFailureReason(t *testing.T, err error) metrics.TaskFailureReason {
	t.Helper()
	var failure *TaskFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error %v is not a TaskFailure", err)
	}
	return failure.metricsReason
}

func clearRunIdentityEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"OZ_RUN_ID", "WARP_RUN_ID", "OZ_EXECUTION_ID", "WARP_EXECUTION_ID"} {
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}
}

func waitForStandbyFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not created", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDirectStandbyWorkspaceNameIsPerRun(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(root, "cwd")
	backend := newStandbyDirectBackend(t, DirectBackendConfig{
		WorkspaceRoot: filepath.Join(root, "workspaces"),
		OzPath:        writeStandbyOzScript(t, `printf '%s' "$PWD" > "$CAPTURE"`),
	})

	var names []string
	for i := 0; i < 2; i++ {
		result := backend.ExecuteTask(context.Background(), &TaskParams{
			StandbyID: "sb-1",
			EnvVars:   []string{"CAPTURE=" + capture},
		})
		if result.Error != nil {
			t.Fatalf("ExecuteTask() error = %v", result.Error)
		}
		workspace, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(string(workspace))
		if !strings.HasPrefix(name, "prewarm-sb-1-") {
			t.Errorf("workspace name = %q, want prefix prewarm-sb-1-", name)
		}
		names = append(names, name)
	}
	if names[0] == names[1] {
		t.Errorf("two runs of one standby shared workspace name %q", names[0])
	}
}

func TestDirectHookEnvNamesStandbyNotRun(t *testing.T) {
	assertStandbyHookEnv := func(t *testing.T, path string) {
		t.Helper()
		env, err := parseEnvFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if env["WARP_PREWARMED_SANDBOX_ID"] != "sb-1" {
			t.Errorf("WARP_PREWARMED_SANDBOX_ID = %q, want sb-1", env["WARP_PREWARMED_SANDBOX_ID"])
		}
		for _, name := range []string{"OZ_PREWARMED_SANDBOX_ID", "OZ_RUN_ID", "WARP_RUN_ID", "OZ_EXECUTION_ID", "WARP_EXECUTION_ID"} {
			if _, ok := env[name]; ok {
				t.Errorf("%s is set in the standby hook environment", name)
			}
		}
	}

	t.Run("standby", func(t *testing.T) {
		clearRunIdentityEnv(t)
		root := t.TempDir()
		setupFile := filepath.Join(root, "setup.env")
		teardownFile := filepath.Join(root, "teardown.env")
		backend := newStandbyDirectBackend(t, DirectBackendConfig{
			WorkspaceRoot:   filepath.Join(root, "workspaces"),
			OzPath:          writeStandbyOzScript(t, "exit 0"),
			SetupCommand:    "env > '" + setupFile + "'",
			TeardownCommand: "env > '" + teardownFile + "'",
		})

		result := backend.ExecuteTask(context.Background(), &TaskParams{StandbyID: "sb-1"})
		if result.Error != nil {
			t.Fatalf("ExecuteTask() error = %v", result.Error)
		}
		assertStandbyHookEnv(t, setupFile)
		assertStandbyHookEnv(t, teardownFile)
	})

	t.Run("task keeps run ID and omits sandbox ID", func(t *testing.T) {
		clearRunIdentityEnv(t)
		root := t.TempDir()
		setupFile := filepath.Join(root, "setup.env")
		backend := newStandbyDirectBackend(t, DirectBackendConfig{
			WorkspaceRoot: filepath.Join(root, "workspaces"),
			OzPath:        writeStandbyOzScript(t, "exit 0"),
			SetupCommand:  "env > '" + setupFile + "'",
		})

		result := backend.ExecuteTask(context.Background(), &TaskParams{TaskID: "run-1", ExecutionID: "exec-1"})
		if result.Error != nil {
			t.Fatalf("ExecuteTask() error = %v", result.Error)
		}
		env, err := parseEnvFile(setupFile)
		if err != nil {
			t.Fatal(err)
		}
		if env["OZ_RUN_ID"] != "run-1" || env["WARP_RUN_ID"] != "run-1" {
			t.Errorf("run ID = (%q, %q), want run-1 under both names", env["OZ_RUN_ID"], env["WARP_RUN_ID"])
		}
		if _, ok := env["WARP_PREWARMED_SANDBOX_ID"]; ok {
			t.Error("WARP_PREWARMED_SANDBOX_ID is set in the task hook environment")
		}
	})
}

func TestDirectStandbyRejectsTargetDir(t *testing.T) {
	root := t.TempDir()
	ranMarker := filepath.Join(root, "ran")
	backend := newStandbyDirectBackend(t, DirectBackendConfig{
		TargetDir: t.TempDir(),
		OzPath:    writeStandbyOzScript(t, `touch "$CAPTURE"`),
	})

	result := backend.ExecuteTask(context.Background(), &TaskParams{
		StandbyID: "sb-1",
		EnvVars:   []string{"CAPTURE=" + ranMarker},
	})
	if result.Outcome != ExecuteOutcomeError {
		t.Fatalf("ExecuteTask() outcome = %v, want error", result.Outcome)
	}
	if reason := standbyTaskFailureReason(t, result.Error); reason != metrics.TaskFailureReasonWorkspaceSetup {
		t.Errorf("failure reason = %v, want %v", reason, metrics.TaskFailureReasonWorkspaceSetup)
	}
	if _, err := os.Stat(ranMarker); !os.IsNotExist(err) {
		t.Error("oz ran for a standby in target_dir mode")
	}
}

func TestDirectStandbyRejectsUnsafeID(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "workspaces")
	backend := newStandbyDirectBackend(t, DirectBackendConfig{
		WorkspaceRoot: workspaceRoot,
		OzPath:        writeStandbyOzScript(t, "exit 0"),
	})

	result := backend.ExecuteTask(context.Background(), &TaskParams{StandbyID: "../escape"})
	if result.Outcome != ExecuteOutcomeError {
		t.Fatalf("ExecuteTask() outcome = %v, want error", result.Outcome)
	}
	if reason := standbyTaskFailureReason(t, result.Error); reason != metrics.TaskFailureReasonWorkspaceSetup {
		t.Errorf("failure reason = %v, want %v", reason, metrics.TaskFailureReasonWorkspaceSetup)
	}
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("workspace root has %d entries after a rejected ID, want 0", len(entries))
	}
}

func TestDirectStandbyCancellationKillsProcess(t *testing.T) {
	clearRunIdentityEnv(t)
	root := t.TempDir()
	started := filepath.Join(root, "started")
	workspaceRoot := filepath.Join(root, "workspaces")
	backend := newStandbyDirectBackend(t, DirectBackendConfig{
		WorkspaceRoot: workspaceRoot,
		OzPath:        writeStandbyOzScript(t, "touch \"$STARTED\"\nexec sleep 60"),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan ExecuteResult, 1)
	go func() {
		results <- backend.ExecuteTask(ctx, &TaskParams{
			StandbyID: "sb-1",
			EnvVars:   []string{"STARTED=" + started},
		})
	}()
	waitForStandbyFile(t, started)

	cancel()
	var result ExecuteResult
	select {
	case result = <-results:
	case <-time.After(15 * time.Second):
		t.Fatal("ExecuteTask did not return after cancellation")
	}
	if reason := standbyTaskFailureReason(t, result.Error); reason != metrics.TaskFailureReasonTaskCancelled {
		t.Errorf("failure reason = %v, want %v", reason, metrics.TaskFailureReasonTaskCancelled)
	}
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("workspace root has %d entries after cancellation, want 0", len(entries))
	}
}
