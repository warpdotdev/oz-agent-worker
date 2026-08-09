package common

import (
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func taskWithIdleTimeout(minutes int) *types.Task {
	return &types.Task{
		ID:                  "task-1",
		AgentConfigSnapshot: &types.AmbientAgentConfig{IdleTimeoutMinutes: &minutes},
	}
}

func TestResolveCleanupGracePrecedence(t *testing.T) {
	tests := []struct {
		name           string
		task           *types.Task
		idleOnComplete string
		want           time.Duration
	}{
		{
			name:           "task idle_timeout_minutes wins",
			task:           taskWithIdleTimeout(90),
			idleOnComplete: "10m",
			want:           90 * time.Minute,
		},
		{
			name:           "worker idle_on_complete is next",
			task:           &types.Task{ID: "task-1"},
			idleOnComplete: "10m",
			want:           10 * time.Minute,
		},
		{
			name:           "oz default is the fallback",
			task:           &types.Task{ID: "task-1"},
			idleOnComplete: "",
			want:           DefaultIdleOnComplete,
		},
		{
			name:           "a zero worker override disables retention",
			task:           &types.Task{ID: "task-1"},
			idleOnComplete: "0s",
			want:           0,
		},
		{
			name:           "a non-positive task timeout falls through to the worker override",
			task:           taskWithIdleTimeout(0),
			idleOnComplete: "5m",
			want:           5 * time.Minute,
		},
		{
			name:           "an unparseable worker override falls back to the default",
			task:           &types.Task{ID: "task-1"},
			idleOnComplete: "not-a-duration",
			want:           DefaultIdleOnComplete,
		},
		{
			name:           "a nil task uses the worker override",
			task:           nil,
			idleOnComplete: "15m",
			want:           15 * time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveCleanupGrace(tc.task, tc.idleOnComplete); got != tc.want {
				t.Fatalf("grace = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveCleanupGraceMatchesTheEmittedIdleOnCompleteFlag(t *testing.T) {
	// The retention window and the flag the agent receives must stay in step:
	// an operator tuning one is tuning both.
	task := taskWithIdleTimeout(30)
	args := AugmentArgsForTask(task, nil, TaskAugmentOptions{IdleOnComplete: "10m"})

	var emitted string
	for i, arg := range args {
		if arg == "--idle-on-complete" && i+1 < len(args) {
			emitted = args[i+1]
		}
	}
	if emitted != "30m" {
		t.Fatalf("emitted --idle-on-complete %q, want 30m", emitted)
	}

	emittedDuration, err := time.ParseDuration(emitted)
	if err != nil {
		t.Fatalf("the emitted flag %q is not a duration: %v", emitted, err)
	}
	if grace := ResolveCleanupGrace(task, "10m"); grace != emittedDuration {
		t.Fatalf("cleanup grace = %s, want it to match the emitted flag %s", grace, emitted)
	}
}
