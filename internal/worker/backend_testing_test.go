package worker

import (
	"context"

	"github.com/warpdotdev/oz-agent-worker/internal/debuglog"
)

// noLogSnapshotBackend satisfies the debug-archive half of the Backend
// contract for fakes whose test does not exercise log collection.
type noLogSnapshotBackend struct{}

func (noLogSnapshotBackend) SnapshotTaskLogs(context.Context, *SnapshotParams) error {
	return debuglog.ErrBackendNotSupported
}

func (noLogSnapshotBackend) CleanupTaskResources(context.Context, *CancelParams) error { return nil }

// registryWith builds a task registry pre-populated with active tasks, so a
// test can start a worker mid-execution without going through assignment.
func registryWith(tasks map[string]activeTask) *TaskRegistry {
	registry := newTaskRegistry()
	for taskID, task := range tasks {
		registry.StartTask(taskID, task, debuglog.BackendDocker, nil)
	}
	return registry
}
