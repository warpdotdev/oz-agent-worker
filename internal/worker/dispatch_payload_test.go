package worker

import (
	"encoding/json"
	"testing"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func TestNewDispatchPayload(t *testing.T) {
	task := &types.Task{ID: "task-1", Title: "do the thing"}
	sidecars := []types.SidecarMount{{Image: "warpdotdev/warp-agent:latest", MountPath: "/agent"}}
	params := &TaskParams{
		TaskID:      "task-1",
		ExecutionID: "exec-1",
		Task:        task,
		DockerImage: "ubuntu:22.04",
		BaseArgs:    []string{"agent", "run", "--task-id", "task-1"},
		EnvVars:     []string{"A=1", "B=2", "A=3", "URL=a=b"},
		Sidecars:    sidecars,
	}

	got := NewDispatchPayload(params, "https://app.warp.dev", "my-worker")

	if got.Version != DispatchPayloadVersion {
		t.Errorf("Version = %d, want %d", got.Version, DispatchPayloadVersion)
	}
	if got.TaskID != "task-1" || got.ExecutionID != "exec-1" {
		t.Errorf("identifiers = (%q, %q), want (task-1, exec-1)", got.TaskID, got.ExecutionID)
	}
	if got.ServerRootURL != "https://app.warp.dev" || got.WorkerID != "my-worker" {
		t.Errorf("server/worker = (%q, %q)", got.ServerRootURL, got.WorkerID)
	}
	if got.DockerImage != "ubuntu:22.04" {
		t.Errorf("DockerImage = %q", got.DockerImage)
	}
	if got.Task != task {
		t.Errorf("Task pointer not preserved")
	}
	if len(got.BaseArgs) != 4 || got.BaseArgs[0] != "agent" || got.BaseArgs[1] != "run" {
		t.Errorf("BaseArgs = %v", got.BaseArgs)
	}
	if len(got.Sidecars) != 1 || got.Sidecars[0].MountPath != "/agent" {
		t.Errorf("Sidecars = %v", got.Sidecars)
	}

	// Env: last-wins on duplicate keys; values may contain '='.
	wantEnv := map[string]string{"A": "3", "B": "2", "URL": "a=b"}
	if len(got.Env) != len(wantEnv) {
		t.Fatalf("Env = %v, want %v", got.Env, wantEnv)
	}
	for k, v := range wantEnv {
		if got.Env[k] != v {
			t.Errorf("Env[%q] = %q, want %q", k, got.Env[k], v)
		}
	}
}

func TestNewDispatchPayloadMarshalsSnakeCaseKeys(t *testing.T) {
	params := &TaskParams{
		TaskID:      "task-1",
		ExecutionID: "exec-1",
		Task:        &types.Task{ID: "task-1"},
		EnvVars:     []string{"K=v"},
	}
	data, err := json.Marshal(NewDispatchPayload(params, "https://app.warp.dev", "w"))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var generic map[string]json.RawMessage
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for _, key := range []string{
		"version", "task_id", "execution_id", "server_root_url", "worker_id",
		"docker_image", "base_args", "env", "sidecars", "task",
	} {
		if _, ok := generic[key]; !ok {
			t.Errorf("payload JSON missing key %q", key)
		}
	}
}
