package sources

import "encoding/json"

// TaskSourceType represents different types of task sources
type TaskSourceType string

const (
	TaskSourceTypeWebhook        TaskSourceType = "webhook"
	TaskSourceTypeScheduledAgent TaskSourceType = "scheduled_agent"
)

// TaskSource is an interface for different task source types
type TaskSource interface {
	GetType() TaskSourceType
}

// AmbientAgentConfig represents the agent configuration
type AmbientAgentConfig struct {
	EnvironmentID *string               `json:"environment_id,omitempty"`
	BasePrompt    *string               `json:"base_prompt,omitempty"`
	ModelID       *string               `json:"model_id,omitempty"`
	MCPServers    []json.RawMessage     `json:"mcp_servers,omitempty"`
}

// ScheduledAgentTaskSource represents a scheduled agent task source
type ScheduledAgentTaskSource struct{}

func (s *ScheduledAgentTaskSource) GetType() TaskSourceType {
	return TaskSourceTypeScheduledAgent
}
