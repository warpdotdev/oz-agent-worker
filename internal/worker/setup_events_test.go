package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func TestNewSetupEventReporter(t *testing.T) {
	assignment := &types.TaskAssignmentMessage{
		TaskID: "task-123",
		EnvVars: map[string]string{
			warpAPIKeyEnv:        "api-key",
			warpWorkloadTokenEnv: "workload-token",
		},
	}

	tests := []struct {
		name          string
		serverRootURL string
		assignment    *types.TaskAssignmentMessage
		wantNil       bool
	}{
		{name: "enabled", serverRootURL: "https://app.warp.dev", assignment: assignment},
		{name: "trailing slash trimmed", serverRootURL: "https://app.warp.dev/", assignment: assignment},
		{name: "no server root URL", serverRootURL: "", assignment: assignment, wantNil: true},
		{name: "nil assignment", serverRootURL: "https://app.warp.dev", assignment: nil, wantNil: true},
		{
			name:          "no API key",
			serverRootURL: "https://app.warp.dev",
			assignment:    &types.TaskAssignmentMessage{TaskID: "task-123"},
			wantNil:       true,
		},
		{
			name:          "workload token optional",
			serverRootURL: "https://app.warp.dev",
			assignment: &types.TaskAssignmentMessage{
				TaskID:  "task-123",
				EnvVars: map[string]string{warpAPIKeyEnv: "api-key"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reporter := newSetupEventReporter(tc.serverRootURL, tc.assignment)
			if (reporter == nil) != tc.wantNil {
				t.Fatalf("newSetupEventReporter() nil = %v, want %v", reporter == nil, tc.wantNil)
			}
			if reporter != nil && reporter.serverRootURL != "https://app.warp.dev" {
				t.Errorf("serverRootURL = %q, want %q", reporter.serverRootURL, "https://app.warp.dev")
			}
		})
	}
}

func TestSetupEventReporterSend(t *testing.T) {
	start := time.Date(2026, 8, 12, 15, 35, 0, 0, time.UTC)
	finish := start.Add(24598 * time.Millisecond)

	t.Run("posts a well-formed client event", func(t *testing.T) {
		var gotRequest *http.Request
		var gotBody clientEventRequest
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotRequest = r.Clone(context.Background())
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("failed to decode request body: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()

		reporter := newSetupEventReporter(server.URL, &types.TaskAssignmentMessage{
			TaskID: "task-123",
			EnvVars: map[string]string{
				warpAPIKeyEnv:        "api-key",
				warpWorkloadTokenEnv: "workload-token",
			},
		})

		if err := reporter.send(context.Background(), SetupEventImagePull, start, finish, false); err != nil {
			t.Fatalf("send() error = %v", err)
		}

		if wantPath := "/api/v1/agent/runs/task-123/client-events"; gotRequest.URL.Path != wantPath {
			t.Errorf("path = %q, want %q", gotRequest.URL.Path, wantPath)
		}
		if got := gotRequest.Header.Get("Authorization"); got != "Bearer api-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer api-key")
		}
		if got := gotRequest.Header.Get(cloudAgentIDHeader); got != "task-123" {
			t.Errorf("%s = %q, want %q", cloudAgentIDHeader, got, "task-123")
		}
		if got := gotRequest.Header.Get(workloadTokenHeader); got != "workload-token" {
			t.Errorf("%s = %q, want %q", workloadTokenHeader, got, "workload-token")
		}
		if gotBody.EventName != SetupEventImagePull {
			t.Errorf("event_name = %q, want %q", gotBody.EventName, SetupEventImagePull)
		}
		if _, err := uuid.Parse(gotBody.EventUUID); err != nil {
			t.Errorf("event_uuid %q is not a valid UUID: %v", gotBody.EventUUID, err)
		}
		if !gotBody.Timestamp.Equal(finish) {
			t.Errorf("timestamp = %v, want %v", gotBody.Timestamp, finish)
		}
		if !gotBody.Payload.StartTS.Equal(start) || !gotBody.Payload.FinishTS.Equal(finish) {
			t.Errorf("payload range = [%v, %v], want [%v, %v]", gotBody.Payload.StartTS, gotBody.Payload.FinishTS, start, finish)
		}
		if gotBody.Payload.LatencyMS != 24598 {
			t.Errorf("latency_ms = %v, want %v", gotBody.Payload.LatencyMS, 24598)
		}
		if gotBody.Payload.IsError {
			t.Errorf("is_error = true, want false")
		}
	})

	t.Run("omits workload token header when absent", func(t *testing.T) {
		var gotToken *string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			values := r.Header.Values(workloadTokenHeader)
			if len(values) > 0 {
				gotToken = &values[0]
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()

		reporter := newSetupEventReporter(server.URL, &types.TaskAssignmentMessage{
			TaskID:  "task-123",
			EnvVars: map[string]string{warpAPIKeyEnv: "api-key"},
		})

		if err := reporter.send(context.Background(), SetupEventContainerStart, start, finish, true); err != nil {
			t.Fatalf("send() error = %v", err)
		}
		if gotToken != nil {
			t.Errorf("workload token header = %q, want unset", *gotToken)
		}
	})

	t.Run("returns error on non-2xx response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "forbidden", http.StatusForbidden)
		}))
		defer server.Close()

		reporter := newSetupEventReporter(server.URL, &types.TaskAssignmentMessage{
			TaskID:  "task-123",
			EnvVars: map[string]string{warpAPIKeyEnv: "api-key"},
		})

		if err := reporter.send(context.Background(), SetupEventSidecarPrep, start, finish, false); err == nil {
			t.Fatal("send() error = nil, want non-nil")
		}
	})
}

func TestSetupEventReporterNilSafe(t *testing.T) {
	var reporter *setupEventReporter
	// Must not panic.
	reporter.reportPhase(context.Background(), SetupEventImagePull, time.Now(), time.Now(), false)
}
