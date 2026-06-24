package commandbackendexample

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// samplePayload mirrors the worker's DispatchPayload JSON for a single task.
const samplePayload = `{
  "version": 1,
  "task_id": "task-1",
  "execution_id": "exec-1",
  "server_root_url": "https://app.warp.dev",
  "worker_id": "my-worker",
  "docker_image": "ubuntu:22.04",
  "base_args": ["agent", "run", "--task-id", "task-1"],
  "env": {"GITHUB_ACCESS_TOKEN": "secret-token"},
  "sidecars": [{"image": "warpdotdev/warp-agent:latest", "mount_path": "/agent", "read_write": false}],
  "task": {"id": "task-1", "title": "do the thing", "task_definition": {"prompt": "go"}}
}`

// transformedBody is the shape dispatch.py's transform() produces.
type transformedBody struct {
	Run struct {
		TaskID      string            `json:"task_id"`
		ExecutionID string            `json:"execution_id"`
		Image       string            `json:"image"`
		Command     []string          `json:"command"`
		Env         map[string]string `json:"env"`
		Mounts      []struct {
			Image     string `json:"image"`
			Path      string `json:"path"`
			ReadWrite bool   `json:"read_write"`
		} `json:"mounts"`
		CallbackURL string `json:"callback_url"`
		Metadata    struct {
			WorkerID       string `json:"worker_id"`
			PayloadVersion int    `json:"payload_version"`
			Title          string `json:"title"`
			Prompt         string `json:"prompt"`
		} `json:"metadata"`
	} `json:"run"`
}

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; skipping reference dispatch script test")
	}
}

func runDispatch(t *testing.T, env []string, payload string) error {
	t.Helper()
	requirePython(t)
	cmd := exec.Command("python3", "dispatch.py")
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(payload)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("dispatch.py output:\n%s", out)
	}
	return err
}

func TestDispatchScriptTransformsAndPostsPayload(t *testing.T) {
	var rawBody []byte
	var gotTaskID, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		gotTaskID = r.Header.Get("X-Oz-Task-Id")
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	if err := runDispatch(t, []string{
		"OZ_DISPATCH_URL=" + srv.URL,
		"OZ_TASK_ID=task-1",
	}, samplePayload); err != nil {
		t.Fatalf("dispatch script returned error on 2xx: %v", err)
	}

	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotTaskID != "task-1" {
		t.Errorf("X-Oz-Task-Id = %q, want task-1", gotTaskID)
	}

	var got transformedBody
	if err := json.Unmarshal(rawBody, &got); err != nil {
		t.Fatalf("posted body is not the expected transformed JSON: %v\nbody: %s", err, rawBody)
	}
	if got.Run.TaskID != "task-1" {
		t.Errorf("run.task_id = %q, want task-1", got.Run.TaskID)
	}
	if got.Run.Image != "ubuntu:22.04" {
		t.Errorf("run.image = %q, want ubuntu:22.04", got.Run.Image)
	}
	if len(got.Run.Command) != 4 || got.Run.Command[0] != "agent" || got.Run.Command[1] != "run" {
		t.Errorf("run.command = %v, want the agent run argv", got.Run.Command)
	}
	if got.Run.Env["GITHUB_ACCESS_TOKEN"] != "secret-token" {
		t.Errorf("run.env[GITHUB_ACCESS_TOKEN] = %q, want secret-token", got.Run.Env["GITHUB_ACCESS_TOKEN"])
	}
	if len(got.Run.Mounts) != 1 || got.Run.Mounts[0].Path != "/agent" {
		t.Errorf("run.mounts = %+v, want a single mount at /agent (mount_path -> path)", got.Run.Mounts)
	}
	if got.Run.Metadata.PayloadVersion != 1 {
		t.Errorf("run.metadata.payload_version = %d, want 1", got.Run.Metadata.PayloadVersion)
	}
	if got.Run.Metadata.Title != "do the thing" {
		t.Errorf("run.metadata.title = %q, want 'do the thing'", got.Run.Metadata.Title)
	}
}

func TestDispatchScriptFailsOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := runDispatch(t, []string{
		"OZ_DISPATCH_URL=" + srv.URL,
		"OZ_TASK_ID=task-1",
	}, samplePayload); err == nil {
		t.Fatal("expected non-zero exit when the endpoint returns 500")
	}
}

func TestDispatchScriptRequiresURL(t *testing.T) {
	requirePython(t)
	cmd := exec.Command("python3", "dispatch.py")
	// Start from a clean env so OZ_DISPATCH_URL is definitely unset.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.Stdin = strings.NewReader(samplePayload)
	if err := cmd.Run(); err == nil {
		t.Fatal("expected non-zero exit when OZ_DISPATCH_URL is unset")
	}
}
