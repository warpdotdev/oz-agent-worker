package worker

import (
	"context"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// Backend defines the interface for task execution backends.
type Backend interface {
	// ExecuteTask runs the agent for the given task assignment.
	ExecuteTask(ctx context.Context, assignment *types.TaskAssignmentMessage) error
	// Shutdown cleans up backend resources.
	Shutdown(ctx context.Context)
}
