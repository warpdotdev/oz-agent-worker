package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/warpdotdev/oz-agent-worker/internal/common"
	"github.com/warpdotdev/oz-agent-worker/internal/debuglog"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/semaphore"
)

const (
	InitialReconnectDelay = 1 * time.Second
	MaxReconnectDelay     = 60 * time.Second
	ReconnectBackoffRate  = 2.0

	HeartbeatInterval      = 30 * time.Second
	PongWait               = 60 * time.Second
	WriteWait              = 10 * time.Second
	BackendShutdownTimeout = 10 * time.Second

	warpServerRootURLEnv = "WARP_SERVER_ROOT_URL"
)

type Config struct {
	APIKey        string
	WorkerID      string
	WebSocketURL  string
	ServerRootURL string
	LogLevel      string
	BackendType   string // "docker", "direct", or "kubernetes"
	// Version is the worker's build identifier, reported to warp-server on
	// every authenticated WebSocket dial so the server can snapshot the exact
	// build that claims an execution.
	Version string
	// DebugLogCapture bounds the disk and concurrency debug-archive log
	// collection may use. Retention comes from the execution's existing
	// idle-on-complete cleanup grace, not from this configuration.
	DebugLogCapture debuglog.Config
	// MaxConcurrentTasks caps how many tasks may execute locally at once
	// (0 means unlimited). A task's slot is released when the backend's
	// ExecuteTask returns, so for backends that spawn tasks fire-and-forget
	// (e.g. command), a slot is held only for the brief dispatch and the limit
	// effectively does not bound the number of remote tasks running at once.
	MaxConcurrentTasks int
	// IdleOnComplete is passed to the oz CLI's --idle-on-complete flag for every task.
	// Empty string means use the oz CLI default (45m). Use "0s" to disable idle.
	IdleOnComplete string
	// SessionSharingServerURL, when non-empty, is forwarded to the oz CLI via --session-sharing-server-url.
	SessionSharingServerURL string

	// Backend-specific configs. Only the one matching BackendType should be set.
	Docker     *DockerBackendConfig
	Direct     *DirectBackendConfig
	Kubernetes *KubernetesBackendConfig
	Command    *CommandBackendConfig
}

type Worker struct {
	config         Config
	conn           *websocket.Conn
	connMutex      sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	reconnectDelay time.Duration
	lastHeartbeat  time.Time
	sendChan       chan []byte
	tasks          *TaskRegistry
	backend        Backend
	taskSemaphore  *semaphore.Weighted // nil when unlimited
	// heartbeatInterval is how often the worker pings the server. It defaults
	// to HeartbeatInterval and is overridable in tests.
	heartbeatInterval time.Duration
	// debugLogStore and debugLogs are nil when debug-log capture failed to
	// initialize. That is non-fatal: ordinary task execution never depends on
	// archive availability.
	debugLogStore *debuglog.Store
	debugLogs     *debuglog.Coordinator
}
type taskCancellationSource string

const (
	taskCancellationSourceUser     taskCancellationSource = "user"
	taskCancellationSourceShutdown taskCancellationSource = "shutdown"
)

type activeTask struct {
	ctx                context.Context
	cancel             context.CancelFunc
	cancellationSource taskCancellationSource
	// executionID is retained so a cancellation can hand the backend full
	// CancelParams without needing the original assignment.
	executionID string
	// spawned marks a task whose backend returned ExecuteOutcomeSpawned: it no
	// longer executes locally, but the entry is kept so a later cancellation
	// can be routed to the backend's CancelTask.
	spawned bool
}

func New(ctx context.Context, config Config) (*Worker, error) {
	workerCtx, cancel := context.WithCancel(ctx)

	var backend Backend
	var err error

	switch config.BackendType {
	case "kubernetes":
		if config.Kubernetes == nil {
			config.Kubernetes = &KubernetesBackendConfig{}
		}
		backend, err = NewKubernetesBackend(ctx, *config.Kubernetes)
	case "direct":
		if config.Direct == nil {
			cancel()
			return nil, fmt.Errorf("direct backend selected but no direct config provided")
		}
		backend, err = NewDirectBackend(ctx, *config.Direct)
	case "command":
		if config.Command == nil {
			cancel()
			return nil, fmt.Errorf("command backend selected but no command config provided")
		}
		backend, err = NewCommandBackend(ctx, *config.Command)
	case "docker", "":
		if config.Docker == nil {
			config.Docker = &DockerBackendConfig{}
		}
		backend, err = NewDockerBackend(ctx, *config.Docker)
	default:
		cancel()
		return nil, fmt.Errorf("unknown backend type: %q", config.BackendType)
	}

	if err != nil {
		cancel()
		return nil, err
	}

	var taskSemaphore *semaphore.Weighted
	if config.MaxConcurrentTasks > 0 {
		taskSemaphore = semaphore.NewWeighted(int64(config.MaxConcurrentTasks))
	}

	w := &Worker{
		config:            config,
		ctx:               workerCtx,
		cancel:            cancel,
		reconnectDelay:    InitialReconnectDelay,
		sendChan:          make(chan []byte, 256),
		tasks:             newTaskRegistry(),
		backend:           backend,
		taskSemaphore:     taskSemaphore,
		heartbeatInterval: HeartbeatInterval,
	}

	// Losing debug-log capture must never cost the operator task execution, so
	// an invalid bound or an unwritable capture root degrades to "no archive
	// capture" and the worker carries on.
	store, err := debuglog.NewStore(config.DebugLogCapture)
	if err != nil {
		log.Errorf(ctx, "Debug archive log capture is disabled: %v", err)
		return w, nil
	}
	w.debugLogStore = store
	w.debugLogs = debuglog.NewCoordinator(debuglog.CoordinatorOptions{
		Ownership: w.tasks,
		Source:    backendSnapshotSource{backend: backend},
		Sender:    w,
		Store:     store,
	})
	return w, nil
}

// backendSnapshotSource adapts the worker's backend to the coordinator's
// snapshot contract.
type backendSnapshotSource struct {
	backend Backend
}

func (s backendSnapshotSource) SnapshotLogs(ctx context.Context, runID, executionID string, sink debuglog.Sink) error {
	return s.backend.SnapshotTaskLogs(ctx, &SnapshotParams{
		TaskID:      runID,
		ExecutionID: executionID,
		Sink:        sink,
	})
}

func (w *Worker) Start() error {
	for {
		select {
		case <-w.ctx.Done():
			return w.ctx.Err()
		default:
		}

		if err := w.connect(); err != nil {
			log.Errorf(w.ctx, "Failed to connect: %v, retrying in %v", err, w.reconnectDelay)
			metrics.RecordWebsocketReconnect(metrics.WSReconnectReasonDialFailed)
			time.Sleep(w.reconnectDelay)

			// Compute exponential back-off.
			w.reconnectDelay = min(time.Duration(float64(w.reconnectDelay)*ReconnectBackoffRate), MaxReconnectDelay)
			continue
		}

		w.reconnectDelay = InitialReconnectDelay
		metrics.SetConnected(true)

		w.run()

		// run() returns when the connection is torn down. The Start loop will
		// either exit via w.ctx.Done() above or reconnect on the next iteration.
		metrics.SetConnected(false)
		metrics.RecordWebsocketReconnect(metrics.WSReconnectReasonRemoteClose)
	}
}

func (w *Worker) connect() error {
	u, err := url.Parse(w.config.WebSocketURL)
	if err != nil {
		return fmt.Errorf("invalid WebSocket URL: %w", err)
	}

	query := u.Query()
	query.Set("worker_id", w.config.WorkerID)
	u.RawQuery = query.Encode()

	headers := make(map[string][]string)
	headers["Authorization"] = []string{fmt.Sprintf("Bearer %s", w.config.APIKey)}
	// The version travels as connection metadata rather than a later message so
	// it reaches the server before this connection is eligible for a task, and
	// a reconnect re-reports it for future assignments.
	if version, ok := workerVersionHeaderValue(w.config.Version); ok {
		headers[types.WorkerVersionHeader] = []string{version}
	} else if w.config.Version != "" {
		// The value itself is never logged: an invalid build identifier is
		// still attacker-influenced input.
		log.Warnf(w.ctx, "Omitting invalid worker version metadata from the connection")
	}

	log.Infof(w.ctx, "Connecting to %s", u.String())

	conn, resp, err := websocket.DefaultDialer.Dial(u.String(), headers)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("failed to dial WebSocket: %w\n%s", err, resp.Status)
		}
		return fmt.Errorf("failed to dial WebSocket: %w", err)
	}

	w.connMutex.Lock()
	w.conn = conn
	w.connMutex.Unlock()

	log.Infof(w.ctx, "Successfully connected to server")

	conn.SetPongHandler(func(string) error {
		w.lastHeartbeat = time.Now()
		if err := conn.SetReadDeadline(time.Now().Add(PongWait)); err != nil {
			log.Warnf(w.ctx, "Failed to set read deadline in pong handler: %v", err)
		}
		return nil
	})

	return nil
}

func (w *Worker) run() {
	w.connMutex.Lock()
	conn := w.conn
	w.connMutex.Unlock()
	if conn == nil {
		return
	}

	done := make(chan struct{})

	// Each loop is bound to this connection. A loop from a previous
	// connection must never write to a newer connection: gorilla/websocket
	// supports at most one concurrent writer per connection, and a stale
	// writer racing the current one panics the whole process with
	// "concurrent write to websocket connection".
	go w.readLoop(conn, done)
	go w.writeLoop(conn, done)
	go w.heartbeatLoop(conn, done)

	<-done

	w.connMutex.Lock()
	if w.conn != nil {
		if err := w.conn.Close(); err != nil {
			log.Warnf(w.ctx, "Error closing connection: %v", err)
		}
		w.conn = nil
	}
	w.connMutex.Unlock()

	log.Warnf(w.ctx, "Connection closed, will attempt to reconnect")
}

func (w *Worker) readLoop(conn *websocket.Conn, done chan struct{}) {
	defer close(done)

	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		if err := conn.SetReadDeadline(time.Now().Add(PongWait)); err != nil {
			log.Errorf(w.ctx, "Failed to set read deadline: %v", err)
			return
		}
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Errorf(w.ctx, "WebSocket read error: %v", err)
			}
			return
		}

		log.Debugf(w.ctx, "WebSocket received: %s", string(message))

		w.handleMessage(message)
	}
}

// writeLoop is the single writer of data frames on conn. All data messages
// must go through sendChan; nothing else may call WriteMessage on conn while
// this loop is running.
func (w *Worker) writeLoop(conn *websocket.Conn, done chan struct{}) {
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-done:
			return
		case message := <-w.sendChan:
			log.Debugf(w.ctx, "WebSocket sending: %s", string(message))

			if err := conn.SetWriteDeadline(time.Now().Add(WriteWait)); err != nil {
				log.Errorf(w.ctx, "Failed to set write deadline: %v", err)
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
				log.Errorf(w.ctx, "WebSocket write error: %v", err)
				return
			}
		}
	}
}

func (w *Worker) heartbeatLoop(conn *websocket.Conn, done chan struct{}) {
	interval := w.heartbeatInterval
	if interval <= 0 {
		interval = HeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			// Pings must use WriteControl: it is the only write method that
			// gorilla/websocket documents as safe to call concurrently with
			// the data writes performed by writeLoop. Using WriteMessage here
			// races writeLoop and panics the process with "concurrent write
			// to websocket connection".
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(WriteWait)); err != nil {
				log.Errorf(w.ctx, "Failed to send ping: %v", err)
				return
			}
		}
	}
}

func (w *Worker) handleMessage(message []byte) {
	log.Debugf(w.ctx, "Received message: %s", string(message))

	var msg types.WebSocketMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		log.Errorf(w.ctx, "Failed to unmarshal message: %v", err)
		return
	}

	switch msg.Type {
	case types.MessageTypeTaskAssignment:
		var assignment types.TaskAssignmentMessage
		if err := json.Unmarshal(msg.Data, &assignment); err != nil {
			log.Errorf(w.ctx, "Failed to unmarshal task assignment: %v", err)
			return
		}
		w.handleTaskAssignment(&assignment)

	case types.MessageTypeTaskCancellation:
		var cancellation types.TaskCancellationMessage
		if err := json.Unmarshal(msg.Data, &cancellation); err != nil {
			log.Errorf(w.ctx, "Failed to unmarshal task cancellation: %v", err)
			return
		}
		w.handleTaskCancellation(&cancellation)

	case types.MessageTypeDebugArchiveLogsRequested:
		var request types.DebugArchiveLogsRequestedMessage
		if err := json.Unmarshal(msg.Data, &request); err != nil {
			// A request whose envelope will not parse cannot be attributed to
			// an execution, so this process cannot know whether it owns it and
			// must not answer.
			log.Errorf(w.ctx, "Failed to unmarshal debug archive log request: %v", err)
			return
		}
		if w.debugLogs == nil {
			return
		}
		// Handing off to the coordinator keeps provider reads and uploads off
		// this loop, so heartbeats, cancellations, and later assignments are
		// still processed while a snapshot is in flight.
		w.debugLogs.Handle(w.ctx, &request)

	default:
		log.Warnf(w.ctx, "Unknown message type: %s", msg.Type)
	}
}

// maxWorkerVersionBytes bounds the build identifier the worker reports.
const maxWorkerVersionBytes = 128

// workerVersionHeaderValue reports whether a build identifier is safe to send
// as connection metadata. It is an opaque, non-secret display value, so the
// only requirements are that it fits the bound, is valid UTF-8, and carries no
// control characters that could break header framing. An empty or invalid
// value is simply omitted: the server records provenance as not reported and
// the connection still executes tasks.
func workerVersionHeaderValue(version string) (string, bool) {
	if version == "" || len(version) > maxWorkerVersionBytes || !utf8.ValidString(version) {
		return "", false
	}
	for _, r := range version {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return version, true
}

// SendDebugArchiveAck enqueues a debug-archive acknowledgement through the
// worker's single WebSocket writer.
func (w *Worker) SendDebugArchiveAck(ack *types.DebugArchiveLogsUploadedMessage) error {
	data, err := json.Marshal(ack)
	if err != nil {
		return fmt.Errorf("failed to marshal debug archive acknowledgement: %w", err)
	}

	msgBytes, err := json.Marshal(types.WebSocketMessage{
		Type: types.MessageTypeDebugArchiveLogsUploaded,
		Data: data,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal websocket message: %w", err)
	}

	return w.sendMessage(msgBytes)
}

func (w *Worker) handleTaskCancellation(cancellation *types.TaskCancellationMessage) {
	task, ok := w.tasks.Update(cancellation.TaskID, func(task *activeTask) {
		if task.cancellationSource == "" {
			task.cancellationSource = taskCancellationSourceUser
		}
	})
	if ok && task.spawned {
		// executeTask has already returned for a spawned task, so no deferred
		// cleanup will remove the entry; drop it now that the cancellation is
		// being routed to the backend.
		w.tasks.Delete(cancellation.TaskID)
	}

	if !ok {
		log.Warnf(w.ctx, "Received cancellation for inactive task: taskID=%s", cancellation.TaskID)
		return
	}

	log.Infof(w.ctx, "Cancelling task from server request: taskID=%s", cancellation.TaskID)
	metrics.AddTaskEvent(task.ctx, "task.cancellation_requested",
		attribute.String("source", "server"),
		attribute.String("task.id", cancellation.TaskID),
	)
	// Every backend gets the same cancellation contract: its cancelation hook
	// is invoked explicitly, and then its execution context is canceled..
	w.cancelTaskOnBackend(&CancelParams{TaskID: cancellation.TaskID, ExecutionID: task.executionID})
	task.cancel()
}

// cancelTaskOnBackend makes a best-effort attempt to cancel a task via the
// backend's CancelTask.
func (w *Worker) cancelTaskOnBackend(params *CancelParams) {
	log.Infof(w.ctx, "Requesting backend cancellation for task %s", params.TaskID)
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), BackendShutdownTimeout)
		defer cancel()
		if err := w.backend.CancelTask(ctx, params); err != nil {
			log.Warnf(w.ctx, "Backend cancellation failed for task %s: %v", params.TaskID, err)
			metrics.AddTaskEvent(ctx, "cancel.failed",
				attribute.String("reason", string(metrics.TaskFailureReasonCancelCommand)),
				attribute.String("task.id", params.TaskID),
			)
		}
	}()
}

func (w *Worker) handleTaskAssignment(assignment *types.TaskAssignmentMessage) {
	receivedAt := time.Now()
	log.Infof(w.ctx, "Received task assignment: taskID=%s, title=%s", assignment.TaskID, assignment.Task.Title)
	taskCtx, span := metrics.StartTaskSpan(w.ctx, assignment.TaskID, assignment.Task.Title)
	metrics.AddTaskEvent(taskCtx, "task.assigned",
		attribute.String("worker.id", w.config.WorkerID),
		attribute.String("worker.backend", w.config.BackendType),
		attribute.String("task.id", assignment.TaskID),
	)

	// Check concurrency limit before claiming the task.
	if w.taskSemaphore != nil {
		if !w.taskSemaphore.TryAcquire(1) {
			log.Warnf(w.ctx, "Rejecting task %s: worker at maximum concurrency (%d)", assignment.TaskID, w.config.MaxConcurrentTasks)
			metrics.RecordTaskRejected(metrics.RejectReasonAtCapacity)
			metrics.AddTaskEvent(taskCtx, "task.rejected",
				attribute.String("reason", metrics.RejectReasonAtCapacity),
			)
			span.End()
			if err := w.sendTaskRejected(assignment.TaskID, "worker at maximum concurrency"); err != nil {
				log.Errorf(w.ctx, "Failed to send task rejected message: %v", err)
			}
			return
		}
	}

	// It's important to update the task state to claimed as the task lifecycle treats this as a dependency to advance to further states.
	if err := w.sendTaskClaimed(assignment.TaskID); err != nil {
		log.Errorf(w.ctx, "Failed to send task claimed message: %v", err)
	}
	metrics.RecordTaskClaim()
	metrics.AddTaskEvent(taskCtx, "task.claimed")
	metrics.IncTasksActive()
	select {
	case <-w.ctx.Done():
		log.Infof(w.ctx, "Skipping task execution after worker shutdown during claim: taskID=%s", assignment.TaskID)
		if w.taskSemaphore != nil {
			w.taskSemaphore.Release(1)
		}
		metrics.DecTasksActive()
		span.End()
		return
	default:
	}

	executionCtx := taskCtx
	if w.backend.PreservesTasksOnShutdown() {
		executionCtx = context.WithoutCancel(taskCtx)
	}
	taskCtx, taskCancel := context.WithCancel(executionCtx)

	w.tasks.StartTask(assignment.TaskID, activeTask{
		ctx:         taskCtx,
		cancel:      taskCancel,
		executionID: assignment.ExecutionID,
	}, w.backendKind(), w.newTaskLogCapture())
	go w.executeTask(taskCtx, taskCancel, span, assignment, receivedAt)
}

// backendKind is the resolved backend name reported on debug-archive records
// and acknowledgements. It normalizes the empty default to docker so ownership
// lookups and NDJSON records always name a real backend.
func (w *Worker) backendKind() string {
	if w.config.BackendType == "" {
		return debuglog.BackendDocker
	}
	return w.config.BackendType
}

// newTaskLogCapture allocates a bounded output capture for a direct execution.
// Every other backend reads provider-native logs on demand instead of keeping
// a second copy, and a capture that cannot be allocated is recorded and
// skipped rather than failing the assignment.
func (w *Worker) newTaskLogCapture() *debuglog.TaskLogCapture {
	if w.debugLogStore == nil || w.backendKind() != debuglog.BackendDirect {
		return nil
	}
	capture, err := w.debugLogStore.NewTaskLogCapture(nil)
	if err != nil {
		log.Warnf(w.ctx, "Debug archive output capture is unavailable for this task: %v", err)
		return nil
	}
	metrics.SetDebugArchiveCaptureBytes(w.debugLogStore.ReservedBytes())
	return capture
}

// prepareTaskParams converts a TaskAssignmentMessage into backend-agnostic TaskParams,
// resolving common environment variables, default images, and base CLI arguments.
func (w *Worker) prepareTaskParams(assignment *types.TaskAssignmentMessage) *TaskParams {
	task := assignment.Task

	// Resolve Docker image.
	// Precedence: server-provided image (from environment) > worker config default_image > hardcoded ubuntu:22.04.
	dockerImage := w.defaultImageForTask(assignment.DockerImage, task)

	// Build common environment variables.
	envVars := []string{
		fmt.Sprintf("TASK_ID=%s", task.ID),
		"GIT_TERMINAL_PROMPT=0",
		"GH_PROMPT_DISABLED=1",
	}
	if w.config.ServerRootURL != "" {
		envVars = append(envVars, fmt.Sprintf("%s=%s", warpServerRootURLEnv, w.config.ServerRootURL))
	}
	for key, value := range assignment.EnvVars {
		envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
	}

	// Build base CLI arguments shared across all backends.
	baseArgs := []string{
		"agent",
		"run",
	}
	// Only share with the team when the task is team-owned. User-owned tasks
	// (created with "Team visible" unchecked) use user-scoped API keys that
	// cannot set up team-level session sharing.
	if task.Owner.IsTeamOwned() {
		baseArgs = append(baseArgs, "--share", "team:edit")
	}
	baseArgs = append(baseArgs,
		"--task-id",
		task.ID,
		"--sandboxed",
		"--server-root-url",
		w.config.ServerRootURL,
	)
	baseArgs = common.AugmentArgsForTask(task, baseArgs, common.TaskAugmentOptions{
		IdleOnComplete:   w.config.IdleOnComplete,
		AdditionalOzArgs: assignment.AdditionalOzArgs,
	})
	if w.config.SessionSharingServerURL != "" {
		baseArgs = append(baseArgs, "--session-sharing-server-url", w.config.SessionSharingServerURL)
	}

	// Build a unified sidecar list:
	// entrypoint.sh lives) comes first, followed by any additional sidecars.
	var sidecars []types.SidecarMount
	if assignment.SidecarImage != "" {
		sidecarImage := assignment.SidecarImage
		if w.config.Kubernetes != nil && w.config.Kubernetes.SidecarImage != "" {
			log.Infof(w.ctx, "Overriding server sidecar image %s with configured sidecar image %s", assignment.SidecarImage, w.config.Kubernetes.SidecarImage)
			sidecarImage = w.config.Kubernetes.SidecarImage
		}
		sidecars = append(sidecars, types.SidecarMount{
			Image:     sidecarImage,
			MountPath: "/agent",
		})
	}
	sidecars = append(sidecars, assignment.AdditionalSidecars...)

	// Apply worker-configured coding CLI sidecar overrides.
	// For each harness entry in the worker's coding_cli_sidecars config, replace the
	// server-provided sidecar image at /mnt/{harness}-cli-sidecar or inject a new entry
	// when the server did not send one (e.g. because no Warp-side image is configured).
	if w.config.Kubernetes != nil && len(w.config.Kubernetes.CodingCLISidecars) > 0 {
		if task != nil && task.AgentConfigSnapshot != nil &&
			task.AgentConfigSnapshot.Harness != nil &&
			task.AgentConfigSnapshot.Harness.Type != nil {
			harnessType := strings.TrimSpace(*task.AgentConfigSnapshot.Harness.Type)
			if customImage, ok := w.config.Kubernetes.CodingCLISidecars[harnessType]; ok && customImage != "" {
				mountPath := fmt.Sprintf("/mnt/%s-cli-sidecar", harnessType)
				overridden := false
				for i, s := range sidecars {
					if s.MountPath == mountPath {
						log.Infof(w.ctx, "Overriding server coding CLI sidecar %s with configured image %s for harness %s", s.Image, customImage, harnessType)
						sidecars[i].Image = customImage
						overridden = true
						break
					}
				}
				if !overridden {
					log.Infof(w.ctx, "Injecting configured coding CLI sidecar %s for harness %s at %s", customImage, harnessType, mountPath)
					sidecars = append(sidecars, types.SidecarMount{
						Image:     customImage,
						MountPath: mountPath,
					})
				}
			}
		}
	}

	return &TaskParams{
		TaskID:        assignment.TaskID,
		ExecutionID:   assignment.ExecutionID,
		Task:          task,
		EnvVars:       envVars,
		BaseArgs:      baseArgs,
		DockerImage:   dockerImage,
		Sidecars:      sidecars,
		InstanceShape: assignment.InstanceShape,
	}
}

// defaultImageForTask returns the Docker image to use for a task, applying the
// precedence: server-provided > worker config default_image > hardcoded fallback.
func (w *Worker) defaultImageForTask(assignmentImage string, task *types.Task) string {
	if assignmentImage != "" {
		return assignmentImage
	}
	if w.config.Kubernetes != nil && w.config.Kubernetes.DefaultImage != "" {
		log.Infof(w.ctx, "Using worker-configured default image: %s", w.config.Kubernetes.DefaultImage)
		return w.config.Kubernetes.DefaultImage
	}
	fallback := "ubuntu:22.04"
	if task.AgentConfigSnapshot != nil && task.AgentConfigSnapshot.EnvironmentID != nil {
		log.Warnf(w.ctx, "Environment %s specified but no Docker image resolved. Using default: %s",
			*task.AgentConfigSnapshot.EnvironmentID, fallback)
	} else {
		log.Infof(w.ctx, "No environment specified, using default image: %s", fallback)
	}
	return fallback
}

func (w *Worker) executeTask(ctx context.Context, taskCancel context.CancelFunc, span trace.Span, assignment *types.TaskAssignmentMessage, receivedAt time.Time) {
	start := time.Now()
	result := metrics.TaskResultSucceeded

	defer func() {
		taskCancel()
		span.End()
		// Spawned tasks stay tracked so a later cancellation can be routed to
		// the backend's CancelTask; everything else is done.
		if task, tracked := w.tasks.Get(assignment.TaskID); !tracked || !task.spawned {
			w.tasks.Delete(assignment.TaskID)
		}

		if w.taskSemaphore != nil {
			w.taskSemaphore.Release(1)
		}

		metrics.DecTasksActive()
		metrics.RecordTaskCompleted(result, time.Since(start))
	}()

	taskID := assignment.TaskID
	log.Infof(ctx, "Starting task execution: taskID=%s, title=%s", taskID, assignment.Task.Title)
	metrics.AddTaskEvent(ctx, "task.started")

	params := w.prepareTaskParams(assignment)
	if owner, owned := w.tasks.LookupExecution(assignment.TaskID, assignment.ExecutionID); owned {
		params.LogCapture = owner.Capture
	}
	metrics.AddTaskEvent(ctx, "backend.started",
		attribute.String("backend", w.config.BackendType),
		attribute.String("docker.image", params.DockerImage),
	)

	executeResult := w.backend.ExecuteTask(ctx, params)

	// Ownership moves to cleanup grace before any terminal lifecycle message is
	// enqueued. A server that reacts to task_failed by requesting logs then
	// always finds the grace entry instead of racing registry deletion and
	// backend cleanup, which is the whole point of the ANY_FAILURE path.
	if executeResult.Outcome != ExecuteOutcomeSpawned {
		w.beginCleanupGrace(assignment)
	}

	if executeResult.Error != nil {
		err := executeResult.Error
		if ctx.Err() == context.Canceled && w.cancellationSource(taskID) == taskCancellationSourceUser {
			result = metrics.TaskResultCancelled
			metrics.AddTaskEvent(ctx, "task.cancelled",
				attribute.String("source", string(taskCancellationSourceUser)),
			)
			span.SetStatus(codes.Ok, "task cancelled by user request")
			log.Infof(ctx, "Task execution cancelled by user request: taskID=%s", taskID)
			if statusErr := w.sendTaskCancelled(taskID, "Task cancelled by user request."); statusErr != nil {
				log.Errorf(ctx, "Failed to send task cancelled message: %v", statusErr)
			}
			return
		}

		result = metrics.TaskResultFailed
		metricsPhase, metricsReason := taskFailureLabels(err)
		exitCode := failureExitCode(err)
		// Reclassify failures caused by a graceful worker shutdown (task
		// cancelled, or agent killed by the shutdown's SIGTERM) as
		// graceful_shutdown.
		if w.cancellationSource(taskID) == taskCancellationSourceShutdown &&
			(metricsReason == metrics.TaskFailureReasonTaskCancelled || exitCode == sigtermExitCode) {
			metricsReason = metrics.TaskFailureReasonGracefulShutdown
		}
		metrics.RecordTaskFailure(metricsPhase, metricsReason)
		metrics.AddTaskEvent(ctx, "task.failed",
			attribute.String("failure.phase", string(metricsPhase)),
			attribute.String("failure.reason", string(metricsReason)),
			attribute.String("error.message", err.Error()),
		)
		span.RecordError(err)
		span.SetStatus(codes.Error, string(metricsReason))
		log.Errorf(ctx, "Task execution failed: taskID=%s, error=%v", taskID, err)
		if statusErr := w.sendTaskFailed(taskID, userFacingTaskError(err), metricsReason, exitCode); statusErr != nil {
			log.Errorf(ctx, "Failed to send task failed message: %v", statusErr)
		}
		return
	}

	if executeResult.Outcome == ExecuteOutcomeSpawned {
		// If the backend spawned the task asynchronously, then we must not
		// finalize the task now. Instead, we keep the active task record
		// so that cancellation can be routed to the backend's CancelTask
		// implementation later.
		result = metrics.TaskResultDispatched
		w.tasks.Update(taskID, func(task *activeTask) {
			if task.cancellationSource == "" {
				task.spawned = true
			}
		})
		metrics.AddTaskEvent(ctx, "task.dispatched")
		span.SetStatus(codes.Ok, "task dispatched to remote runtime")
		log.Infof(ctx, "Task %s dispatched", taskID)
		return
	}

	log.Infof(ctx, "Task execution completed successfully: taskID=%s", taskID)
	metrics.AddTaskEvent(ctx, "task.completed")
	span.SetStatus(codes.Ok, "task completed")
	if err := w.sendTaskCompleted(taskID, "Task completed successfully"); err != nil {
		log.Errorf(ctx, "Failed to send task completed message: %v", err)
	}
}

func (w *Worker) cancellationSource(taskID string) taskCancellationSource {
	task, ok := w.tasks.Get(taskID)
	if !ok {
		return ""
	}
	return task.cancellationSource
}

// beginCleanupGrace retains the execution's backend resources and output
// capture for the same window the agent itself stays idle, then releases them.
// Reusing the resolved idle-on-complete duration keeps one cleanup clock:
// operators size retention with the setting they already tune, and an archive
// request never extends it.
func (w *Worker) beginCleanupGrace(assignment *types.TaskAssignmentMessage) {
	grace := common.ResolveCleanupGrace(assignment.Task, w.config.IdleOnComplete)
	runID := assignment.TaskID
	executionID := assignment.ExecutionID

	w.tasks.MoveToCleanupGrace(runID, executionID, grace, func() {
		w.expireCleanupGrace(runID, executionID)
	})
}

// expireCleanupGrace performs the backend's normal resource cleanup and frees
// the execution's capture. It is idempotent: the registry hands the entry over
// exactly once, so a racing shutdown sweep finds nothing left to do.
func (w *Worker) expireCleanupGrace(runID, executionID string) {
	entry, ok := w.tasks.ReleaseCleanupGrace(runID, executionID)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), BackendShutdownTimeout)
	defer cancel()

	result := "succeeded"
	if err := w.backend.CleanupTaskResources(ctx, &CancelParams{TaskID: runID, ExecutionID: executionID}); err != nil {
		result = "failed"
		log.Warnf(w.ctx, "Backend cleanup failed after cleanup grace for task %s: %v", runID, err)
	}
	metrics.RecordCleanupGraceResult(entry.backendKind, result)

	if entry.capture != nil {
		entry.capture.Close()
	}
	if w.debugLogStore != nil {
		metrics.SetDebugArchiveCaptureBytes(w.debugLogStore.ReservedBytes())
	}
	if w.debugLogs != nil {
		w.debugLogs.ForgetExecution(runID, executionID)
	}
}
func (w *Worker) sendTaskClaimed(taskID string) error {
	claimed := types.TaskClaimedMessage{
		TaskID:   taskID,
		WorkerID: w.config.WorkerID,
	}

	data, err := json.Marshal(claimed)
	if err != nil {
		return fmt.Errorf("failed to marshal task claimed message: %w", err)
	}

	msg := types.WebSocketMessage{
		Type: types.MessageTypeTaskClaimed,
		Data: data,
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal websocket message: %w", err)
	}

	return w.sendMessage(msgBytes)
}

func (w *Worker) sendTaskCancelled(taskID, message string) error {
	taskState := types.TaskStateCancelled
	completedMsg := types.TaskCompletedMessage{
		TaskID:    taskID,
		Message:   message,
		TaskState: &taskState,
	}

	data, err := json.Marshal(completedMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal task cancelled message: %w", err)
	}

	msg := types.WebSocketMessage{
		Type: types.MessageTypeTaskCompleted,
		Data: data,
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal websocket message: %w", err)
	}

	return w.sendMessage(msgBytes)
}

func (w *Worker) sendTaskRejected(taskID, reason string) error {
	rejectedMsg := types.TaskRejectedMessage{
		TaskID: taskID,
		Reason: reason,
	}

	data, err := json.Marshal(rejectedMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal task rejected message: %w", err)
	}

	msg := types.WebSocketMessage{
		Type: types.MessageTypeTaskRejected,
		Data: data,
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal websocket message: %w", err)
	}

	return w.sendMessage(msgBytes)
}

func (w *Worker) sendTaskCompleted(taskID, message string) error {
	completedMsg := types.TaskCompletedMessage{
		TaskID:  taskID,
		Message: message,
	}

	data, err := json.Marshal(completedMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal task completed message: %w", err)
	}

	msg := types.WebSocketMessage{
		Type: types.MessageTypeTaskCompleted,
		Data: data,
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal websocket message: %w", err)
	}

	return w.sendMessage(msgBytes)
}

func (w *Worker) sendTaskFailed(taskID, message string, reason metrics.TaskFailureReason, exitCode int) error {
	failedMsg := types.TaskFailedMessage{
		TaskID:        taskID,
		Message:       message,
		FailureReason: string(reason),
		ExitCode:      exitCode,
	}

	data, err := json.Marshal(failedMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal task failed message: %w", err)
	}

	msg := types.WebSocketMessage{
		Type: types.MessageTypeTaskFailed,
		Data: data,
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal websocket message: %w", err)
	}

	return w.sendMessage(msgBytes)
}

func (w *Worker) sendMessage(message []byte) error {
	select {
	case w.sendChan <- message:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timeout sending message")
	case <-w.ctx.Done():
		return fmt.Errorf("worker context cancelled")
	}
}

// releaseCleanupGraceEntries performs the backend cleanup each cleanup-grace
// entry was waiting for and deletes the bytes held for log retrieval.
//
// These executions have already reported terminal state; only their log source
// is being retained. Ownership is process-local, so a replacement worker cannot
// inherit the pending timer — leaving the resources behind would strand them
// until an unrelated backstop (the Kubernetes Job TTL) eventually collected
// them, far past the operator's chosen cleanup grace. Running the cleanup early
// here is the same work the expiry timer would have done. Active executions are
// untouched: they are not in cleanup grace, so each backend's own shutdown
// contract still decides whether their task units may outlive this process.
func (w *Worker) releaseCleanupGraceEntries() {
	pending := w.tasks.PendingCleanups()
	if len(pending) == 0 {
		return
	}

	// One bounded budget covers the whole sweep so shutdown cannot stall on an
	// unresponsive backend.
	ctx, cancel := context.WithTimeout(context.Background(), BackendShutdownTimeout)
	defer cancel()

	log.Infof(w.ctx, "Releasing %d cleanup-grace executions during worker shutdown", len(pending))
	for key, entry := range pending {
		result := "succeeded"
		if err := w.backend.CleanupTaskResources(ctx, &CancelParams{
			TaskID:      key.runID,
			ExecutionID: key.executionID,
		}); err != nil {
			result = "failed"
			log.Warnf(w.ctx, "Backend cleanup failed during shutdown for task %s: %v", key.runID, err)
		}
		metrics.RecordCleanupGraceResult(entry.backendKind, result)

		if entry.capture != nil {
			entry.capture.Close()
		}
	}

	if w.debugLogStore != nil {
		metrics.SetDebugArchiveCaptureBytes(w.debugLogStore.ReservedBytes())
	}
}

func (w *Worker) Shutdown() {
	log.Infof(w.ctx, "Shutting down worker...")
	preserveActiveTasks := w.backend.PreservesTasksOnShutdown()

	active := w.tasks.Snapshot()
	activeTaskCount := len(active)
	if activeTaskCount > 0 && preserveActiveTasks {
		log.Infof(w.ctx, "Preserving %d active tasks during worker shutdown", activeTaskCount)
	} else if activeTaskCount > 0 {
		log.Infof(w.ctx, "Cancelling %d active tasks", activeTaskCount)
		for taskID, task := range active {
			w.tasks.Update(taskID, func(tracked *activeTask) {
				if tracked.cancellationSource == "" {
					tracked.cancellationSource = taskCancellationSourceShutdown
				}
			})
			log.Debugf(w.ctx, "Cancelling task: %s", taskID)
			metrics.AddTaskEvent(task.ctx, "task.cancellation_requested",
				attribute.String("source", "signal"),
				attribute.String("task.id", taskID),
			)
			task.cancel()
		}
	}

	if activeTaskCount > 0 && !preserveActiveTasks {
		time.Sleep(500 * time.Millisecond)
	}

	w.cancel()

	// Cancelling the worker context aborts in-flight archive requests, so
	// waiting for them adds no delay beyond the bounded backend shutdown below.
	if w.debugLogs != nil {
		w.debugLogs.Wait()
	}
	w.releaseCleanupGraceEntries()

	backendShutdownCtx, backendShutdownCancel := context.WithTimeout(context.Background(), BackendShutdownTimeout)
	defer backendShutdownCancel()
	w.backend.Shutdown(backendShutdownCtx)

	w.connMutex.Lock()
	if w.conn != nil {
		// WriteControl is safe to call concurrently with writeLoop's data
		// writes and enforces its own deadline, so shutdown can neither panic
		// the process nor block indefinitely on a wedged connection.
		closeMessage := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
		if err := w.conn.WriteControl(websocket.CloseMessage, closeMessage, time.Now().Add(WriteWait)); err != nil {
			log.Warnf(w.ctx, "Failed to send close message: %v", err)
		}
		if err := w.conn.Close(); err != nil {
			log.Warnf(w.ctx, "Failed to close connection: %v", err)
		}
		w.conn = nil
	}
	w.connMutex.Unlock()

	log.Infof(w.ctx, "Worker shutdown complete")
}
