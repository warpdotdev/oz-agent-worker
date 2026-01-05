package types

import (
	"encoding/json"
	"time"

	"github.com/warpdotdev/warp-agent-worker/internal/sources"
)

// TaskState represents the current state of a task
type TaskState string

const (
	TaskStateQueued     TaskState = "QUEUED"
	TaskStatePending    TaskState = "PENDING"
	TaskStateClaimed    TaskState = "CLAIMED"
	TaskStateInProgress TaskState = "INPROGRESS"
	TaskStateSucceeded  TaskState = "SUCCEEDED"
	TaskStateFailed     TaskState = "FAILED"
)

// TaskDefinition represents the definition of a task (prompt and potentially other data)
type TaskDefinition struct {
	Prompt string `json:"prompt"`
}

// Task represents a work item in the queue
type Task struct {
	ID                  string                             `json:"id"`
	Title               string                             `json:"title"`
	Definition          TaskDefinition                     `json:"task_definition"`
	State               TaskState                          `json:"state"`
	TaskSource          sources.TaskSource                 `json:"-"`
	CreatedAt           time.Time                          `json:"created_at"`
	UpdatedAt           time.Time                          `json:"updated_at"`
	WorkerID            string                             `json:"worker_id,omitempty"`
	WorkerData          json.RawMessage                    `json:"worker_data,omitempty"`
	AgentConfigSnapshot *sources.AmbientAgentConfig        `json:"agent_config_snapshot,omitempty"`
}
