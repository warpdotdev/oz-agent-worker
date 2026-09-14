package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func (w *Worker) activeTaskCount() int {
	w.tasksMutex.Lock()
	defer w.tasksMutex.Unlock()
	return len(w.activeTasks)
}

func drainMessages(t *testing.T, ch <-chan []byte) []types.WebSocketMessage {
	t.Helper()
	var msgs []types.WebSocketMessage
	for {
		select {
		case b := <-ch:
			var m types.WebSocketMessage
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("failed to unmarshal websocket message: %v", err)
			}
			msgs = append(msgs, m)
		default:
			return msgs
		}
	}
}

func newCommandWorker(t *testing.T, dispatchCommand string) *Worker {
	t.Helper()
	w, err := New(context.Background(), Config{
		WorkerID:      "it-worker",
		ServerRootURL: "https://app.warp.dev",
		BackendType:   "command",
		Command: &CommandBackendConfig{
			DispatchCommand: dispatchCommand,
			ServerRootURL:   "https://app.warp.dev",
			WorkerID:        "it-worker",
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return w
}

func commandTaskAssignment() *types.TaskAssignmentMessage {
	return &types.TaskAssignmentMessage{
		TaskID:      "task-1",
		ExecutionID: "exec-1",
		Task: &types.Task{
			ID:    "task-1",
			Title: "integration task",
			Owner: &types.TaskOwner{Type: "TEAM", Id: 1},
		},
		DockerImage: "ubuntu:22.04",
		EnvVars:     map[string]string{"GITHUB_ACCESS_TOKEN": "secret-token"},
	}
}

func TestIntegrationCommandBackendDispatchSuppressesTerminalMessage(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "payload.json")
	w := newCommandWorker(t, "cat > "+outFile)

	w.handleTaskAssignment(commandTaskAssignment())

	// The dispatch script writes the captured payload, then executeTask returns
	// and the deferred cleanup keeps the entry in activeTasks marked spawned.
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(outFile)
		return err == nil
	})
	waitFor(t, 5*time.Second, func() bool {
		w.tasksMutex.Lock()
		defer w.tasksMutex.Unlock()
		task, ok := w.activeTasks["task-1"]
		return ok && task.spawned
	})

	// Verify the captured dispatch payload.
	data, err := os.ReadFile(outFile) // #nosec G304 -- test-controlled temp path.
	if err != nil {
		t.Fatalf("failed to read captured payload: %v", err)
	}
	var payload DispatchPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("captured stdin is not valid DispatchPayload JSON: %v", err)
	}
	if payload.Version != DispatchPayloadVersion {
		t.Errorf("Version = %d, want %d", payload.Version, DispatchPayloadVersion)
	}
	if payload.RunID != "task-1" {
		t.Errorf("RunID = %q, want task-1", payload.RunID)
	}
	if len(payload.BaseArgs) < 2 || payload.BaseArgs[0] != "agent" || payload.BaseArgs[1] != "run" {
		t.Errorf("BaseArgs = %v, want to start with [agent run]", payload.BaseArgs)
	}
	if payload.Env["GITHUB_ACCESS_TOKEN"] != "secret-token" {
		t.Errorf("Env[GITHUB_ACCESS_TOKEN] = %q, want secret-token", payload.Env["GITHUB_ACCESS_TOKEN"])
	}

	// The worker must have sent task_claimed but no terminal message.
	msgs := drainMessages(t, w.sendChan)
	var sawClaimed bool
	for _, m := range msgs {
		switch m.Type {
		case types.MessageTypeTaskClaimed:
			sawClaimed = true
		case types.MessageTypeTaskCompleted, types.MessageTypeTaskFailed:
			t.Fatalf("worker must not send a terminal message for a dispatched task, got %q", m.Type)
		}
	}
	if !sawClaimed {
		t.Error("expected the worker to send task_claimed")
	}
}

func TestIntegrationCommandBackendDispatchFailureReportsTaskFailed(t *testing.T) {
	w := newCommandWorker(t, "exit 1")

	w.handleTaskAssignment(commandTaskAssignment())

	waitFor(t, 5*time.Second, func() bool { return w.activeTaskCount() == 0 })

	var failedCount int
	waitFor(t, 5*time.Second, func() bool {
		for _, m := range drainMessages(t, w.sendChan) {
			if m.Type == types.MessageTypeTaskFailed {
				failedCount++
			}
		}
		return failedCount > 0
	})
	if failedCount != 1 {
		t.Fatalf("task_failed message count = %d, want 1", failedCount)
	}
}
