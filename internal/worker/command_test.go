package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func testTaskParams() *TaskParams {
	return &TaskParams{
		TaskID:      "task-1",
		ExecutionID: "exec-1",
		Task:        &types.Task{ID: "task-1", Title: "test"},
		DockerImage: "ubuntu:22.04",
		BaseArgs:    []string{"agent", "run", "--task-id", "task-1"},
		EnvVars:     []string{"SUPER_SECRET_TOKEN=hunter2"},
		Sidecars:    []types.SidecarMount{{Image: "warpdotdev/warp-agent:latest", MountPath: "/agent"}},
	}
}

func newTestCommandBackend(t *testing.T, cfg CommandBackendConfig) *CommandBackend {
	t.Helper()
	if cfg.DispatchCommand == "" {
		cfg.DispatchCommand = "true"
	}
	b, err := NewCommandBackend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewCommandBackend() error = %v", err)
	}
	return b
}

func assertBackendFailureReason(t *testing.T, err error, wantReason string) {
	t.Helper()
	var failure *backendFailureError
	if !errors.As(err, &failure) {
		t.Fatalf("error %v is not a *backendFailureError", err)
	}
	if failure.reason != wantReason {
		t.Fatalf("failure reason = %q, want %q", failure.reason, wantReason)
	}
}

func TestCommandBackendDispatchesPayloadOnStdin(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "payload.json")
	b := newTestCommandBackend(t, CommandBackendConfig{
		DispatchCommand: "cat > " + outFile,
		ServerRootURL:   "https://app.warp.dev",
		WorkerID:        "my-worker",
	})

	result := b.ExecuteTask(context.Background(), testTaskParams())
	if result.Error != nil || result.Outcome != ExecuteOutcomeSpawned {
		t.Fatalf("ExecuteTask() = %+v, want ExecuteOutcomeSpawned with no error", result)
	}

	data, readErr := os.ReadFile(outFile) // #nosec G304 -- test-controlled temp path.
	if readErr != nil {
		t.Fatalf("failed to read captured payload: %v", readErr)
	}
	var got DispatchPayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("captured stdin is not valid DispatchPayload JSON: %v", err)
	}
	if got.Version != DispatchPayloadVersion {
		t.Errorf("Version = %d, want %d", got.Version, DispatchPayloadVersion)
	}
	if got.RunID != "task-1" {
		t.Errorf("RunID = %q, want task-1", got.RunID)
	}
	if got.DockerImage != "ubuntu:22.04" {
		t.Errorf("DockerImage = %q", got.DockerImage)
	}
	if len(got.BaseArgs) < 2 || got.BaseArgs[0] != "agent" || got.BaseArgs[1] != "run" {
		t.Errorf("BaseArgs = %v, want to start with [agent run]", got.BaseArgs)
	}
	if got.Env["SUPER_SECRET_TOKEN"] != "hunter2" {
		t.Errorf("Env[SUPER_SECRET_TOKEN] = %q, want hunter2", got.Env["SUPER_SECRET_TOKEN"])
	}
	if len(got.Sidecars) != 1 || got.Sidecars[0].MountPath != "/agent" {
		t.Errorf("Sidecars = %v", got.Sidecars)
	}
}

func TestCommandBackendSuccessReturnsSpawnedOutcome(t *testing.T) {
	b := newTestCommandBackend(t, CommandBackendConfig{DispatchCommand: "exit 0"})
	result := b.ExecuteTask(context.Background(), testTaskParams())
	if result.Error != nil || result.Outcome != ExecuteOutcomeSpawned {
		t.Fatalf("ExecuteTask() = %+v, want ExecuteOutcomeSpawned with no error", result)
	}
}

func TestCommandBackendNonZeroExitIsClassifiedFailure(t *testing.T) {
	b := newTestCommandBackend(t, CommandBackendConfig{DispatchCommand: "exit 7"})
	result := b.ExecuteTask(context.Background(), testTaskParams())
	if result.Error == nil || result.Outcome != ExecuteOutcomeError {
		t.Fatalf("non-zero exit must not be treated as a successful dispatch, got %+v", result)
	}
	assertBackendFailureReason(t, result.Error, metrics.TaskFailureReasonDispatchCommand)
}

func TestCommandBackendTimeoutIsClassifiedFailure(t *testing.T) {
	b := newTestCommandBackend(t, CommandBackendConfig{
		DispatchCommand: "sleep 5",
		DispatchTimeout: 50 * time.Millisecond,
	})
	result := b.ExecuteTask(context.Background(), testTaskParams())
	if result.Error == nil || result.Outcome != ExecuteOutcomeError {
		t.Fatalf("timed-out dispatch must not be treated as success, got %+v", result)
	}
	assertBackendFailureReason(t, result.Error, metrics.TaskFailureReasonDispatchTimeout)
}

func TestCommandBackendDoesNotLeakSecretsIntoSubprocessEnv(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "env.txt")
	b := newTestCommandBackend(t, CommandBackendConfig{
		DispatchCommand: "env > " + outFile,
		Env:             map[string]string{"OPERATOR_VAR": "ok"},
	})

	if result := b.ExecuteTask(context.Background(), testTaskParams()); result.Error != nil || result.Outcome != ExecuteOutcomeSpawned {
		t.Fatalf("ExecuteTask() = %+v, want ExecuteOutcomeSpawned with no error", result)
	}

	data, err := os.ReadFile(outFile) // #nosec G304 -- test-controlled temp path.
	if err != nil {
		t.Fatalf("failed to read captured env: %v", err)
	}
	env := string(data)
	if !strings.Contains(env, "OZ_RUN_ID=task-1") {
		t.Errorf("expected OZ_RUN_ID in subprocess env, got:\n%s", env)
	}
	if !strings.Contains(env, "OPERATOR_VAR=ok") {
		t.Errorf("expected operator env var in subprocess env")
	}
	if strings.Contains(env, "SUPER_SECRET_TOKEN") {
		t.Errorf("task secret must NOT appear in subprocess env; got:\n%s", env)
	}
}

func TestCommandBackendWellKnownVarsCannotBeClobberedByOperatorEnv(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "env.txt")
	b := newTestCommandBackend(t, CommandBackendConfig{
		DispatchCommand: "env > " + outFile,
		// Operator attempts to override well-known vars — they must not take effect.
		Env: map[string]string{
			"OZ_RUN_ID":          "operator-injected",
			"OZ_EXECUTION_ID":    "operator-injected",
			"OZ_WORKER_BACKEND": "operator-injected",
		},
	})

	if result := b.ExecuteTask(context.Background(), testTaskParams()); result.Error != nil || result.Outcome != ExecuteOutcomeSpawned {
		t.Fatalf("ExecuteTask() = %+v, want ExecuteOutcomeSpawned with no error", result)
	}

	data, err := os.ReadFile(outFile) // #nosec G304 -- test-controlled temp path.
	if err != nil {
		t.Fatalf("failed to read captured env: %v", err)
	}
	env := string(data)
	if !strings.Contains(env, "OZ_RUN_ID=task-1") {
		t.Errorf("OZ_RUN_ID must be the actual task ID, not overridable by operator Env; got:\n%s", env)
	}
	if !strings.Contains(env, "OZ_EXECUTION_ID=exec-1") {
		t.Errorf("OZ_EXECUTION_ID must be the actual execution ID, not overridable by operator Env; got:\n%s", env)
	}
	if !strings.Contains(env, "OZ_WORKER_BACKEND=command") {
		t.Errorf("OZ_WORKER_BACKEND must be 'command', not overridable by operator Env; got:\n%s", env)
	}
}

func TestNewCommandBackendRequiresDispatchCommand(t *testing.T) {
	if _, err := NewCommandBackend(context.Background(), CommandBackendConfig{}); err == nil {
		t.Fatal("expected error for empty dispatch command")
	}
}

func TestCommandBackendCancelTask(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "cancelled")
	b := newTestCommandBackend(t, CommandBackendConfig{
		DispatchCommand: "true",
		CancelCommand:   "touch " + sentinel,
	})

	if err := b.CancelTask(context.Background(), &CancelParams{TaskID: "task-1", ExecutionID: "exec-1"}); err != nil {
		t.Fatalf("CancelTask() error = %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("cancel command did not run (sentinel missing): %v", err)
	}
}

func TestCommandBackendCancelTaskNoCommandIsNoop(t *testing.T) {
	b := newTestCommandBackend(t, CommandBackendConfig{DispatchCommand: "true"})
	if err := b.CancelTask(context.Background(), &CancelParams{TaskID: "task-1"}); err != nil {
		t.Fatalf("CancelTask() with no cancel command should be a no-op, got %v", err)
	}
}

func TestNewWorkerSelectsCommandBackend(t *testing.T) {
	w, err := New(context.Background(), Config{
		BackendType: "command",
		Command:     &CommandBackendConfig{DispatchCommand: "true"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, ok := w.backend.(*CommandBackend); !ok {
		t.Fatalf("backend type = %T, want *CommandBackend", w.backend)
	}

	if _, err := New(context.Background(), Config{BackendType: "command"}); err == nil {
		t.Fatal("expected error when command backend selected but no config provided")
	}
}
