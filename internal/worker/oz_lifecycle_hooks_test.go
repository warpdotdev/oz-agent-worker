package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func testOzLifecycleHooksContext() *types.OzLifecycleHooksContext {
	return &types.OzLifecycleHooksContext{
		Required:                       true,
		SupportedPayloadSchemaVersions: []string{types.OzHookPayloadSchemaV1},
		ProjectTrust: []types.OzLifecycleHookTrustRecord{
			{
				GitRoot:    "/workspace/repo",
				ConfigPath: "/workspace/repo/.warp/hooks.json",
				SHA256:     strings.Repeat("a", 64),
			},
		},
	}
}

func mustPrepareTaskParams(t *testing.T, worker *Worker, assignment *types.TaskAssignmentMessage) *TaskParams {
	t.Helper()
	params, err := worker.prepareTaskParams(assignment)
	if err != nil {
		t.Fatalf("prepareTaskParams() error = %v", err)
	}
	return params
}

func ozHookAssignment(harness *string) *types.TaskAssignmentMessage {
	return &types.TaskAssignmentMessage{
		TaskID: "task-1",
		Task: &types.Task{
			ID: "task-1",
			AgentConfigSnapshot: &types.AmbientAgentConfig{
				Harness: &types.Harness{Type: harness},
			},
		},
		OzLifecycleHooks: testOzLifecycleHooksContext(),
	}
}

func TestPrepareTaskParamsCarriesOzLifecycleHooksOnlyInArgv(t *testing.T) {
	worker := &Worker{
		ctx:     context.Background(),
		backend: &recordingBackend{},
		config:  Config{BackendType: "direct"},
	}
	assignment := ozHookAssignment(stringPtr("oz"))
	params := mustPrepareTaskParams(t, worker, assignment)

	if params.OzLifecycleHooks != assignment.OzLifecycleHooks {
		t.Fatal("TaskParams did not preserve lifecycle hook metadata")
	}
	argIndex := -1
	for i, arg := range params.BaseArgs {
		if arg == ozLifecycleHooksContextArg {
			argIndex = i
			break
		}
	}
	if argIndex < 0 || argIndex+1 >= len(params.BaseArgs) {
		t.Fatalf("hook context argument missing from %v", params.BaseArgs)
	}
	var decoded types.OzLifecycleHooksContext
	if err := json.Unmarshal([]byte(params.BaseArgs[argIndex+1]), &decoded); err != nil {
		t.Fatalf("hook context argument is invalid: %v", err)
	}
	for _, env := range params.EnvVars {
		if strings.Contains(env, "LIFECYCLE_HOOK") || strings.Contains(env, assignment.OzLifecycleHooks.ProjectTrust[0].SHA256) {
			t.Fatalf("hook metadata leaked into environment: %q", env)
		}
	}
}

func TestValidateTaskAssignmentRejectsIncompatibleHookTasks(t *testing.T) {
	unsupportedBackend := &recordingBackend{}
	worker := &Worker{
		ctx:     context.Background(),
		backend: unsupportedOzLifecycleHooksBackend{Backend: unsupportedBackend},
		config:  Config{BackendType: "unsupported"},
	}

	tests := []struct {
		name       string
		assignment *types.TaskAssignmentMessage
	}{
		{
			name:       "third-party harness",
			assignment: ozHookAssignment(stringPtr("codex")),
		},
		{
			name:       "unsupported backend",
			assignment: ozHookAssignment(stringPtr("oz")),
		},
		{
			name: "reserved argument collision",
			assignment: func() *types.TaskAssignmentMessage {
				assignment := ozHookAssignment(stringPtr("oz"))
				assignment.AdditionalOzArgs = []string{ozLifecycleHooksContextArg, `{}`}
				return assignment
			}(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testWorker := worker
			if test.name != "unsupported backend" {
				testWorker = &Worker{
					ctx:     context.Background(),
					backend: &recordingBackend{},
					config:  Config{BackendType: "direct"},
				}
			}
			if err := testWorker.validateTaskAssignment(test.assignment); err == nil {
				t.Fatal("expected assignment to be rejected")
			}
		})
	}
}

func TestValidateTaskAssignmentAcceptsSupportedAndUnhookedTasks(t *testing.T) {
	worker := &Worker{
		ctx:     context.Background(),
		backend: &recordingBackend{},
		config:  Config{BackendType: "direct"},
	}
	implicitOz := ozHookAssignment(nil)
	implicitOz.Task.AgentConfigSnapshot = nil
	if err := worker.validateTaskAssignment(implicitOz); err != nil {
		t.Fatalf("implicit Oz hook assignment rejected: %v", err)
	}

	unhookedThirdParty := ozHookAssignment(stringPtr("codex"))
	unhookedThirdParty.OzLifecycleHooks = nil
	if err := worker.validateTaskAssignment(unhookedThirdParty); err != nil {
		t.Fatalf("unhooked third-party assignment rejected: %v", err)
	}
}

func TestMalformedHookMetadataIsRejectedBeforeClaim(t *testing.T) {
	worker := &Worker{
		ctx:      context.Background(),
		backend:  &recordingBackend{},
		config:   Config{BackendType: "direct"},
		sendChan: make(chan []byte, 1),
	}
	message := []byte(`{
		"type":"task_assignment",
		"data":{
			"task_id":"task-1",
			"task":{"id":"task-1","title":"test"},
			"oz_lifecycle_hooks":{
				"required":true,
				"supported_payload_schema_versions":["warp.oz_hook.v1"],
				"project_trust":[],
				"unknown":true
			}
		}
	}`)

	worker.handleMessage(message)

	rejected := readWebSocketMessage(t, worker.sendChan)
	if rejected.Type != types.MessageTypeTaskRejected {
		t.Fatalf("message type = %q, want task_rejected", rejected.Type)
	}
	if len(worker.activeTasks) != 0 {
		t.Fatal("malformed hook task was added to active tasks")
	}
}

type unsupportedOzLifecycleHooksBackend struct {
	Backend
}

func (unsupportedOzLifecycleHooksBackend) SupportsOzLifecycleHooks() bool {
	return false
}
