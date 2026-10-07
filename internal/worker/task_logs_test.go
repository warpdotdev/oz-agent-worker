package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/tasklogs"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/trace/noop"
	collector "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func taskLogServer(t *testing.T) (*httptest.Server, <-chan string, *atomic.Int32) {
	t.Helper()
	payloads := make(chan string, 32)
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/graphql/v2" {
			calls.Add(1)
			if req.Header.Get("Authorization") == "Bearer rejected-api-key" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issueTaskIdentityToken": map[string]any{
				"__typename": "IssueTaskIdentityTokenOutput", "token": "task-log-token", "expiresAt": time.Now().Add(time.Hour),
			}}})
			return
		}
		if req.URL.Path == "/otlp/v1/logs" {
			data, _ := io.ReadAll(req.Body)
			payload := &collector.ExportLogsServiceRequest{}
			if err := proto.Unmarshal(data, payload); err != nil {
				t.Error(err)
			}
			text, _ := protojson.Marshal(payload)
			payloads <- string(text)
			return
		}
		if !strings.HasSuffix(req.URL.Path, "/client-events") {
			t.Errorf("unexpected path %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, payloads, calls
}

func TestSetupEnvironmentCredentialsRedactedInAgentOutput(t *testing.T) {
	server, payloads, _ := taskLogServer(t)
	dir := t.TempDir()
	ozPath := filepath.Join(dir, "oz")
	script := "#!/bin/sh\ntest \"$CUSTOM_CREDENTIAL\" = generated-credential || exit 2\nprintf 'agent %s\\n' \"$CUSTOM_CREDENTIAL\"\n"
	if err := os.WriteFile(ozPath, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	backend := &DirectBackend{ozPath: ozPath, config: DirectBackendConfig{
		WorkspaceRoot: filepath.Join(dir, "workspaces"),
		SetupCommand:  `printf 'CUSTOM_CREDENTIAL=generated-credential\n' >"$OZ_ENVIRONMENT_FILE"`,
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := &Worker{config: Config{BackendType: "direct", ServerRootURL: server.URL, CollectTaskLogs: true},
		ctx: ctx, backend: backend, activeTasks: make(map[string]activeTask), outbound: newOutboundQueue(8)}
	_, span := noop.NewTracerProvider().Tracer("test").Start(ctx, "task")
	w.executeTask(ctx, cancel, span, logAssignment(server.URL), time.Now())
	text := logPayloads(payloads)
	if strings.Contains(text, "generated-credential") || !strings.Contains(text, "[REDACTED]") || !strings.Contains(text, "agent.stdout") {
		t.Fatalf("setup environment credential was not redacted in agent output: %s", text)
	}
}

func TestCollectedSubprocessOutputDoesNotWaitForBackgroundChildren(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, *tasklogs.Reporter, string) ExecuteResult
	}{
		{"hook", func(ctx context.Context, reporter *tasklogs.Reporter, command string) ExecuteResult {
			b := &DirectBackend{}
			if err := b.runCommand(ctx, command, t.TempDir(), nil, reporter); err != nil {
				return executeError(err)
			}
			return executeCompleted()
		}},
		{"agent", func(ctx context.Context, reporter *tasklogs.Reporter, command string) ExecuteResult {
			dir := t.TempDir()
			path := filepath.Join(dir, "oz")
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"+command+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			b := &DirectBackend{ozPath: path, config: DirectBackendConfig{WorkspaceRoot: dir}}
			return b.ExecuteTask(ctx, &TaskParams{TaskID: "run", Logs: reporter})
		}},
		{"dispatch", func(ctx context.Context, reporter *tasklogs.Reporter, command string) ExecuteResult {
			b := &CommandBackend{config: CommandBackendConfig{DispatchCommand: command, DispatchTimeout: time.Second}}
			return b.ExecuteTask(ctx, &TaskParams{TaskID: "run", Logs: reporter})
		}},
	} {
		for _, cancelParent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%t", tc.name, cancelParent), func(t *testing.T) {
				server, _, _ := taskLogServer(t)
				reporter, err := tasklogs.New(server.URL, "worker", tc.name, logAssignment(server.URL))
				if err != nil {
					t.Fatal(err)
				}
				defer reporter.Shutdown(t.Context())
				start := time.Now()
				ctx := t.Context()
				command := "sleep 2 &"
				if cancelParent {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
					defer cancel()
					command += " wait"
				}
				result := tc.run(ctx, reporter, command)
				if (result.Error != nil) != cancelParent || time.Since(start) > 750*time.Millisecond {
					t.Fatalf("descendant output changed subprocess completion: elapsed=%s error=%v", time.Since(start), result.Error)
				}
			})
		}
	}
}

func TestDispatchLogCollectionRetainsTimeout(t *testing.T) {
	server, _, _ := taskLogServer(t)
	reporter, err := tasklogs.New(server.URL, "worker", "command", logAssignment(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Shutdown(t.Context())
	backend := &CommandBackend{config: CommandBackendConfig{
		DispatchCommand: "sleep 2 & wait", DispatchTimeout: 30 * time.Millisecond,
	}}
	start := time.Now()
	result := backend.ExecuteTask(t.Context(), &TaskParams{TaskID: "run", Logs: reporter})
	_, reason := taskFailureLabels(result.Error)
	if reason != metrics.TaskFailureReasonDispatchTimeout || time.Since(start) > 750*time.Millisecond {
		t.Fatalf("dispatch deadline not preserved: elapsed=%s error=%v", time.Since(start), result.Error)
	}
}

func TestSubprocessDrainRetainsExitFailure(t *testing.T) {
	server, _, _ := taskLogServer(t)
	reporter, err := tasklogs.New(server.URL, "worker", "direct", logAssignment(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Shutdown(t.Context())
	backend := &DirectBackend{}
	err = backend.runCommand(t.Context(), "sleep 2 & exit 7", t.TempDir(), nil, reporter)
	if code, ok := agentExitCode(err); !ok || code != 7 {
		t.Fatalf("output drain discarded subprocess exit error: %v", err)
	}
}

func logAssignment(endpoint string) *types.TaskAssignmentMessage {
	return &types.TaskAssignmentMessage{
		TaskID: "run", ExecutionID: "execution", Task: &types.Task{ID: "run", Title: "task"},
		EnvVars:             map[string]string{"WARP_API_KEY": "task-api-key", "WARP_WORKLOAD_TOKEN": "task-workload-token"},
		TelemetryCollection: &types.TelemetryCollectionConfig{Endpoint: endpoint + "/otlp", LoggingEnabled: true},
	}
}

func logPayloads(ch <-chan string) string {
	var result []string
	for {
		select {
		case text := <-ch:
			result = append(result, text)
		default:
			return strings.Join(result, "\n")
		}
	}
}

func TestDirectTaskLogLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, setup        string
		disabled, rejected bool
		want               string
	}{
		{name: "agent output", want: "agent.stdout"},
		{name: "setup failure", setup: "printf 'setup failed\\n'; exit 2", want: "setup_worker_setup_command"},
		{name: "operator disabled", disabled: true},
		{name: "identity rejected", rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, payloads, calls := taskLogServer(t)
			dir := t.TempDir()
			ozPath := filepath.Join(dir, "oz")
			if err := os.WriteFile(ozPath, []byte("#!/bin/sh\nprintf 'agent credential:%s\\n' \"$CUSTOM_CREDENTIAL\"\nprintf 'error output\\n' >&2\n"), 0700); err != nil {
				t.Fatal(err)
			}
			backend := &DirectBackend{
				ozPath: ozPath,
				config: DirectBackendConfig{WorkspaceRoot: filepath.Join(dir, "workspaces"), SetupCommand: tc.setup, Env: map[string]string{"CUSTOM_CREDENTIAL": "private-backend-value"}},
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			w := &Worker{
				config: Config{BackendType: "direct", ServerRootURL: server.URL, CollectTaskLogs: !tc.disabled},
				ctx:    ctx, backend: backend, activeTasks: make(map[string]activeTask), outbound: newOutboundQueue(8),
			}
			_, span := noop.NewTracerProvider().Tracer("test").Start(ctx, "task")
			assignment := logAssignment(server.URL)
			if tc.rejected {
				assignment.EnvVars[warpAPIKeyEnv] = "rejected-api-key"
			}
			w.executeTask(ctx, cancel, span, assignment, time.Now())
			text := logPayloads(payloads)
			if tc.disabled {
				if text != "" || calls.Load() != 0 {
					t.Fatal("operator kill switch still exported logs or requested credentials")
				}
				return
			}
			if tc.rejected {
				messages := drainMessages(t, w.outbound.messages)
				if len(messages) != 1 || messages[0].Type != types.MessageTypeTaskCompleted || text != "" {
					t.Fatal("identity failure changed the task outcome")
				}
				return
			}
			if !strings.Contains(text, tc.want) || strings.Contains(text, "private-backend-value") {
				t.Fatalf("task output or privacy mismatch: %s", text)
			}
			if tc.setup == "" && !strings.Contains(text, "agent.stderr") {
				t.Fatal("stderr was not collected")
			}
			if tc.setup != "" && !strings.Contains(text, "hook.stdout") {
				t.Fatal("setup failure output was not collected")
			}
			if len(drainMessages(t, w.outbound.messages)) != 1 {
				t.Fatal("observability changed terminal task reporting")
			}
		})
	}
}

type cancellableLoggingBackend struct {
	started  chan struct{}
	reporter *tasklogs.Reporter
	preserve bool
}

func (b *cancellableLoggingBackend) ExecuteTask(ctx context.Context, params *TaskParams) ExecuteResult {
	b.reporter = params.Logs
	log.Infof(ctx, "backend pending")
	close(b.started)
	<-ctx.Done()
	return executeError(ctx.Err())
}
func (b *cancellableLoggingBackend) CancelTask(context.Context, *CancelParams) error { return nil }
func (b *cancellableLoggingBackend) Shutdown(context.Context)                        {}
func (b *cancellableLoggingBackend) PreservesTasksOnShutdown() bool                  { return b.preserve }

func TestTaskLogsFlushOnCancellationAndPreservingShutdown(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancellation", true: "preserving shutdown"}[preserve], func(t *testing.T) {
			server, payloads, _ := taskLogServer(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			backend := &cancellableLoggingBackend{started: make(chan struct{}), preserve: preserve}
			w := &Worker{
				config: Config{ServerRootURL: server.URL, BackendType: "direct", CollectTaskLogs: true},
				ctx:    ctx, backend: backend, activeTasks: make(map[string]activeTask), outbound: newOutboundQueue(8), oneShot: newOneShotState(),
			}
			w.handleTaskAssignment(logAssignment(server.URL))
			<-backend.started
			if preserve {
				w.shutdownTasks()
				if w.activeTasks["run"].ctx.Err() != nil {
					t.Error("reporter shutdown cancelled a preserved task")
				}
				w.activeTasks["run"].cancel()
				w.taskWG.Wait()
			} else {
				w.handleTaskCancellation(&types.TaskCancellationMessage{TaskID: "run"})
				w.taskWG.Wait()
			}
			if backend.reporter.Context().Err() == nil || !strings.Contains(logPayloads(payloads), "backend pending") {
				t.Error("reporter was not stopped and flushed")
			}
		})
	}
}

func TestDockerLogFrameDecoding(t *testing.T) {
	var framed bytes.Buffer
	for _, frame := range []struct {
		stream byte
		text   string
	}{{1, "first"}, {2, "error\n"}, {1, "-second\n"}} {
		header := [8]byte{frame.stream}
		binary.BigEndian.PutUint32(header[4:], uint32(len(frame.text)))
		framed.Write(header[:])
		framed.WriteString(frame.text)
	}
	var stdout, stderr bytes.Buffer
	if err := copyDockerLogFrames(&framed, &stdout, &stderr); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if stdout.String() != "first-second\n" || stderr.String() != "error\n" {
		t.Fatal("Docker multiplexed streams were mixed or included frame headers")
	}
}

type labelledContainerEngine struct {
	t       *testing.T
	configs []container.Config
}

func (e *labelledContainerEngine) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case req.URL.Path == "/_ping":
		return mockEngineResponse(req, http.StatusOK, "text/plain", "OK"), nil
	case strings.Contains(req.URL.Path, "/images/") && strings.HasSuffix(req.URL.Path, "/json"):
		return mockEngineResponse(req, http.StatusOK, "application/json", `{"Os":"linux","Architecture":"amd64","Id":"sha256:abc123"}`), nil
	case strings.Contains(req.URL.Path, "/volumes/") && req.Method == http.MethodGet:
		return mockEngineErrorResponse(e.t, req, http.StatusNotFound, "no such volume"), nil
	case strings.HasSuffix(req.URL.Path, "/volumes/create"):
		return mockEngineResponse(req, http.StatusCreated, "application/json", `{}`), nil
	case strings.HasSuffix(req.URL.Path, "/containers/create"):
		var config container.Config
		if err := json.NewDecoder(req.Body).Decode(&config); err != nil {
			return nil, err
		}
		e.configs = append(e.configs, config)
		return mockEngineResponse(req, http.StatusCreated, "application/json", `{"Id":"created"}`), nil
	case strings.HasSuffix(req.URL.Path, "/export"):
		return mockEngineResponse(req, http.StatusOK, "application/x-tar", ""), nil
	default:
		return mockEngineErrorResponse(e.t, req, http.StatusInternalServerError, "stop after creation"), nil
	}
}

func TestDockerTaskAndSidecarContainerLabels(t *testing.T) {
	engine := &labelledContainerEngine{t: t}
	c, err := client.New(client.WithHTTPClient(&http.Client{Transport: engine}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	backend := &DockerBackend{dockerClient: c, platform: "linux/amd64", config: DockerBackendConfig{ImagePullPolicy: PullPolicyNever, NoCleanup: true}}
	params := &TaskParams{TaskID: "run", ExecutionID: "execution", DockerImage: "ubuntu:22.04"}
	_ = backend.ExecuteTask(t.Context(), params)
	params.TaskID, params.ExecutionID = "second-run", "second-execution"
	params.Sidecars = []types.SidecarMount{{Image: "sidecar:latest", MountPath: "/mnt/sidecar"}}
	_ = backend.ExecuteTask(t.Context(), params)
	if len(engine.configs) != 3 {
		t.Fatalf("created %d configs, want task/export/extract", len(engine.configs))
	}
	for i, config := range engine.configs {
		wantTask, wantExecution := "run", "execution"
		if i > 0 {
			wantTask, wantExecution = "second-run", "second-execution"
		}
		if config.Labels["oz-task-id"] != wantTask || config.Labels["oz-execution-id"] != wantExecution {
			t.Errorf("missing task labels: %v", config.Labels)
		}
	}
}

func TestKubernetesLogStreamsDeduplicateObservedContainers(t *testing.T) {
	collectorServer, payloads, _ := taskLogServer(t)
	reporter, err := tasklogs.New(collectorServer.URL, "worker", "kubernetes", logAssignment(collectorServer.URL))
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if req.URL.Query().Get("follow") != "true" {
			t.Error("pod logs were not followed")
		}
		_, _ = io.WriteString(w, "pod task-api-key\n")
	}))
	defer api.Close()
	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	backend := &KubernetesBackend{clientset: clientset, config: KubernetesBackendConfig{Namespace: "agents"}}
	streams := newKubernetesLogStreams(t.Context(), backend, reporter)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", UID: "uid"}, Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{Name: "setup", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}},
		ContainerStatuses:     []corev1.ContainerStatus{{Name: "task", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
	}}
	streams.observePod(pod)
	streams.observePod(pod)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	streams.finish(ctx)
	reporter.Shutdown(ctx)
	text := logPayloads(payloads)
	if requests.Load() != 2 || !strings.Contains(text, "pod.setup") || !strings.Contains(text, "pod.task") || strings.Contains(text, "task-api-key") {
		t.Fatalf("pod stream duplication or redaction failed: requests=%d logs=%s", requests.Load(), text)
	}
}
