package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/debuglog"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// writeShellScript writes an executable script for the direct backend to run.
func writeShellScript(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { // #nosec G306 -- an executable test fixture.
		t.Fatalf("failed to write %s: %v", path, err)
	}
	return path
}

func newCaptureStore(t *testing.T) *debuglog.Store {
	t.Helper()
	config := debuglog.DefaultConfig()
	config.Directory = t.TempDir()
	store, err := debuglog.NewStore(config)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// TestDirectBackendCapturesEveryPhaseOnBothStreams runs a real direct execution
// whose setup, agent, and teardown each write a distinguishable sentinel to
// stdout and stderr, then proves every sentinel survives a snapshot taken after
// the per-task workspace has been removed.
func TestDirectBackendCapturesEveryPhaseOnBothStreams(t *testing.T) {
	testDir := t.TempDir()
	ozPath := writeShellScript(t, filepath.Join(testDir, "oz"), `#!/bin/sh
echo "AGENT-STDOUT-SENTINEL"
echo "AGENT-STDERR-SENTINEL" >&2
exit 0
`)

	workspaceRoot := filepath.Join(testDir, "workspaces")
	backend, err := NewDirectBackend(context.Background(), DirectBackendConfig{
		WorkspaceRoot: workspaceRoot,
		OzPath:        ozPath,
		SetupCommand: strings.Join([]string{
			`echo "SETUP-STDOUT-SENTINEL"`,
			`echo "SETUP-STDERR-SENTINEL" >&2`,
		}, "\n"),
		TeardownCommand: strings.Join([]string{
			`echo "TEARDOWN-STDOUT-SENTINEL"`,
			`echo "TEARDOWN-STDERR-SENTINEL" >&2`,
		}, "\n"),
	})
	if err != nil {
		t.Fatalf("NewDirectBackend: %v", err)
	}

	store := newCaptureStore(t)
	capture, err := store.NewTaskLogCapture(nil)
	if err != nil {
		t.Fatalf("NewTaskLogCapture: %v", err)
	}
	defer capture.Close()

	result := backend.ExecuteTask(context.Background(), &TaskParams{
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		LogCapture:  capture,
	})
	if result.Error != nil {
		t.Fatalf("ExecuteTask: %v", result.Error)
	}

	// The per-task workspace is gone by now; only the capture remains.
	if _, err := os.Stat(filepath.Join(workspaceRoot, "task-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected the workspace to be cleaned up, stat err = %v", err)
	}

	capture.Finalize(5 * time.Second)
	sink := &captureSink{}
	if err := backend.SnapshotTaskLogs(context.Background(), &SnapshotParams{
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		Sink:        sink,
	}); err != nil {
		t.Fatalf("SnapshotTaskLogs: %v", err)
	}

	// Each sentinel must appear exactly once, under the phase and stream the
	// backend actually owns the handle for.
	want := map[string]struct {
		phase  debuglog.Phase
		stream debuglog.Stream
	}{
		"SETUP-STDOUT-SENTINEL":    {debuglog.PhaseSetup, debuglog.StreamStdout},
		"SETUP-STDERR-SENTINEL":    {debuglog.PhaseSetup, debuglog.StreamStderr},
		"AGENT-STDOUT-SENTINEL":    {debuglog.PhaseAgent, debuglog.StreamStdout},
		"AGENT-STDERR-SENTINEL":    {debuglog.PhaseAgent, debuglog.StreamStderr},
		"TEARDOWN-STDOUT-SENTINEL": {debuglog.PhaseTeardown, debuglog.StreamStdout},
		"TEARDOWN-STDERR-SENTINEL": {debuglog.PhaseTeardown, debuglog.StreamStderr},
	}
	for sentinel, expected := range want {
		occurrences := 0
		for _, chunk := range sink.chunks {
			if !strings.Contains(string(chunk.Data), sentinel) {
				continue
			}
			occurrences++
			if chunk.Phase != expected.phase {
				t.Errorf("%s phase = %q, want %q", sentinel, chunk.Phase, expected.phase)
			}
			if chunk.Stream != expected.stream {
				t.Errorf("%s stream = %q, want %q", sentinel, chunk.Stream, expected.stream)
			}
		}
		if occurrences != 1 {
			t.Errorf("%s appeared %d times, want exactly once", sentinel, occurrences)
		}
	}
}

func TestDirectBackendStoresNoWorkspaceFileContent(t *testing.T) {
	testDir := t.TempDir()
	ozPath := writeShellScript(t, filepath.Join(testDir, "oz"), `#!/bin/sh
printf 'WORKSPACE-FILE-SECRET\n' > secret.txt
echo "AGENT-OUTPUT"
exit 0
`)

	backend, err := NewDirectBackend(context.Background(), DirectBackendConfig{
		WorkspaceRoot: filepath.Join(testDir, "workspaces"),
		OzPath:        ozPath,
	})
	if err != nil {
		t.Fatalf("NewDirectBackend: %v", err)
	}

	store := newCaptureStore(t)
	capture, err := store.NewTaskLogCapture(nil)
	if err != nil {
		t.Fatalf("NewTaskLogCapture: %v", err)
	}
	defer capture.Close()

	if result := backend.ExecuteTask(context.Background(), &TaskParams{
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		LogCapture:  capture,
	}); result.Error != nil {
		t.Fatalf("ExecuteTask: %v", result.Error)
	}
	capture.Finalize(5 * time.Second)

	sink := &captureSink{}
	if err := backend.SnapshotTaskLogs(context.Background(), &SnapshotParams{
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		Sink:        sink,
	}); err != nil {
		t.Fatalf("SnapshotTaskLogs: %v", err)
	}

	var captured strings.Builder
	for _, chunk := range sink.chunks {
		captured.Write(chunk.Data)
	}
	if !strings.Contains(captured.String(), "AGENT-OUTPUT") {
		t.Fatalf("captured = %q, want the agent's stdout", captured.String())
	}
	if strings.Contains(captured.String(), "WORKSPACE-FILE-SECRET") {
		t.Fatal("the capture stored workspace file content, not just stdout/stderr")
	}
}

func TestDirectBackendRunsWithoutACapture(t *testing.T) {
	// Archive capture is optional: an execution must run identically when the
	// store could not allocate one.
	testDir := t.TempDir()
	ozPath := writeShellScript(t, filepath.Join(testDir, "oz"), "#!/bin/sh\necho ok\nexit 0\n")

	backend, err := NewDirectBackend(context.Background(), DirectBackendConfig{
		WorkspaceRoot: filepath.Join(testDir, "workspaces"),
		OzPath:        ozPath,
	})
	if err != nil {
		t.Fatalf("NewDirectBackend: %v", err)
	}

	if result := backend.ExecuteTask(context.Background(), &TaskParams{
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		Task:        &types.Task{ID: "task-1"},
	}); result.Error != nil {
		t.Fatalf("ExecuteTask: %v", result.Error)
	}

	err = backend.SnapshotTaskLogs(context.Background(), &SnapshotParams{
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		Sink:        &captureSink{},
	})
	var snapshotErr *debuglog.SnapshotError
	if !errors.As(err, &snapshotErr) {
		t.Fatalf("error = %v, want a *debuglog.SnapshotError", err)
	}
	if snapshotErr.ReasonCode != types.DebugArchiveReasonCaptureUnavailable {
		t.Fatalf("reason = %q, want %q", snapshotErr.ReasonCode, types.DebugArchiveReasonCaptureUnavailable)
	}
}
