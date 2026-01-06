package types

import "encoding/json"

// MessageType represents the type of WebSocket message
type MessageType string

const (
	MessageTypeTaskAssignment MessageType = "task_assignment"
	MessageTypeTaskClaimed    MessageType = "task_claimed"
	MessageTypeTaskFailed     MessageType = "task_failed"
	MessageTypeHeartbeat      MessageType = "heartbeat"
)

// WebSocketMessage is the base structure for all WebSocket messages
type WebSocketMessage struct {
	Type MessageType     `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// TaskAssignmentMessage is sent from server to worker when a task is available
type TaskAssignmentMessage struct {
	TaskID       string `json:"task_id"`
	Task         *Task  `json:"task"`
	DockerImage  string `json:"docker_image,omitempty"`
	SidecarImage string `json:"sidecar_image,omitempty"`
	GitHubToken  string `json:"github_token,omitempty"`
}

// TaskClaimedMessage is sent from worker to server after successfully claiming a task
type TaskClaimedMessage struct {
	TaskID   string `json:"task_id"`
	WorkerID string `json:"worker_id"`
}

// TaskFailedMessage is sent from worker to server if task launch fails
type TaskFailedMessage struct {
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
}
