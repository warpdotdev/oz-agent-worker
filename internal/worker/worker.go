package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/warpdotdev/oz-agent-worker/internal/common"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
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
	// MaxConcurrentTasks caps how many tasks may execute locally at once
	// (0 means unlimited). A task's slot is released when the backend's
	// ExecuteTask returns, so for backends that spawn tasks fire-and-forget
	// (e.g. command), a slot is held only for the brief dispatch and the limit
	// effectively does not bound the number of remote tasks running at once.
	MaxConcurrentTasks int
	// OneShot makes the worker accept one task and exit after it reaches a
	// terminal outcome. Backend support is validated before initialization.
	OneShot bool
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
	config        Config
	ctx           context.Context
	outbound      *outboundQueue
	activeTasks   map[string]activeTask
	tasksMutex    sync.Mutex
	taskWG        sync.WaitGroup
	shuttingDown  bool
	oneShot       oneShotState
	backend       Backend
	taskSemaphore *semaphore.Weighted // nil when unlimited
	// heartbeatInterval is how often the worker pings the server. It defaults
	// to HeartbeatInterval and is overridable in tests.
	heartbeatInterval time.Duration
}

type runOutcome int

const (
	runOutcomeConnectionClosed runOutcome = iota
	runOutcomeOneShotComplete
	runOutcomeServerShutdown
)

type oneShotState struct {
	mutex        sync.Mutex
	accepted     bool
	done         chan struct{}
	completeOnce sync.Once
}

func newOneShotState() oneShotState {
	return oneShotState{done: make(chan struct{})}
}

func (s *oneShotState) tryAccept() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.accepted {
		return false
	}
	s.accepted = true
	return true
}

func (s *oneShotState) resetAcceptance() {
	s.mutex.Lock()
	s.accepted = false
	s.mutex.Unlock()
}

func (s *oneShotState) complete() {
	if s.done == nil {
		return
	}
	s.completeOnce.Do(func() {
		close(s.done)
	})
}

func (s *oneShotState) doneChannel() <-chan struct{} {
	return s.done
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
	backendType := config.BackendType
	capabilities, err := capabilitiesForBackend(backendType)
	if err != nil {
		return nil, err
	}
	if config.OneShot && !capabilities.supportsOneShot {
		return nil, fmt.Errorf("backend %q does not support one-shot mode", backendType)
	}
	if config.OneShot {
		config.MaxConcurrentTasks = 1
	}

	var backend Backend
	switch config.BackendType {
	case "kubernetes":
		if config.Kubernetes == nil {
			config.Kubernetes = &KubernetesBackendConfig{}
		}
		backend, err = NewKubernetesBackend(ctx, *config.Kubernetes)
	case "direct":
		if config.Direct == nil {
			return nil, fmt.Errorf("direct backend selected but no direct config provided")
		}
		backend, err = NewDirectBackend(ctx, *config.Direct)
	case "command":
		if config.Command == nil {
			return nil, fmt.Errorf("command backend selected but no command config provided")
		}
		backend, err = NewCommandBackend(ctx, *config.Command)
	case "docker", "":
		if config.Docker == nil {
			config.Docker = &DockerBackendConfig{}
		}
		backend, err = NewDockerBackend(ctx, *config.Docker)
	default:
		return nil, fmt.Errorf("unknown backend type: %q", backendType)
	}

	if err != nil {
		return nil, err
	}

	var taskSemaphore *semaphore.Weighted
	if config.MaxConcurrentTasks > 0 {
		taskSemaphore = semaphore.NewWeighted(int64(config.MaxConcurrentTasks))
	}

	return &Worker{
		config:            config,
		ctx:               ctx,
		outbound:          newOutboundQueue(256),
		activeTasks:       make(map[string]activeTask),
		oneShot:           newOneShotState(),
		backend:           backend,
		taskSemaphore:     taskSemaphore,
		heartbeatInterval: HeartbeatInterval,
	}, nil
}

func capabilitiesForBackend(backendType string) (backendCapabilities, error) {
	switch backendType {
	case "direct":
		return (&DirectBackend{}).Capabilities(), nil
	case "docker", "":
		return (&DockerBackend{}).Capabilities(), nil
	case "kubernetes":
		return (&KubernetesBackend{}).Capabilities(), nil
	case "command":
		return (&CommandBackend{}).Capabilities(), nil
	default:
		return backendCapabilities{}, fmt.Errorf("unknown backend type: %q", backendType)
	}
}

// Run drives the worker's processing loops until the server context is cancelled
// for graceful shutdown or a one-shot task finishes:
// - The WebSocket connection loop
// - Task acceptance and processing
// - Client state maintained by backends
func (w *Worker) Run() error {
	protocolCtx, protocolCancel := context.WithCancel(context.Background())
	defer protocolCancel()
	defer w.shutdownBackend()
	reconnectDelay := InitialReconnectDelay

	for {
		if w.ctx.Err() != nil {
			w.shutdownTasks()
			w.outbound.Close()
			return nil
		}

		conn, err := w.connect()
		if err != nil {
			if w.ctx.Err() != nil {
				w.shutdownTasks()
				w.outbound.Close()
				return nil
			}
			log.Errorf(w.ctx, "Failed to connect: %v, retrying in %v", err, reconnectDelay)
			metrics.RecordWebsocketReconnect(metrics.WSReconnectReasonDialFailed)
			timer := time.NewTimer(reconnectDelay)
			select {
			case <-timer.C:
			case <-w.ctx.Done():
				// If the timer fired concurrently with cancellation, consume
				// its tick when available before returning.
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				w.shutdownTasks()
				w.outbound.Close()
				return nil
			}
			reconnectDelay = min(time.Duration(float64(reconnectDelay)*ReconnectBackoffRate), MaxReconnectDelay)
			continue
		}

		reconnectDelay = InitialReconnectDelay
		metrics.SetConnected(true)
		outcome := w.serveConnection(protocolCtx, conn)
		metrics.SetConnected(false)
		if outcome == runOutcomeOneShotComplete || outcome == runOutcomeServerShutdown {
			return nil
		}
		metrics.RecordWebsocketReconnect(metrics.WSReconnectReasonRemoteClose)
	}
}

func (w *Worker) connect() (*websocket.Conn, error) {
	u, err := url.Parse(w.config.WebSocketURL)
	if err != nil {
		return nil, fmt.Errorf("invalid WebSocket URL: %w", err)
	}

	query := u.Query()
	query.Set("worker_id", w.config.WorkerID)
	u.RawQuery = query.Encode()

	headers := make(map[string][]string)
	headers["Authorization"] = []string{fmt.Sprintf("Bearer %s", w.config.APIKey)}

	log.Infof(w.ctx, "Connecting to %s", u.String())
	conn, resp, err := websocket.DefaultDialer.Dial(u.String(), headers)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("failed to dial WebSocket: %w\n%s", err, resp.Status)
		}
		return nil, fmt.Errorf("failed to dial WebSocket: %w", err)
	}

	log.Infof(w.ctx, "Successfully connected to server")
	conn.SetPongHandler(func(string) error {
		if err := conn.SetReadDeadline(time.Now().Add(PongWait)); err != nil {
			log.Warnf(w.ctx, "Failed to set read deadline in pong handler: %v", err)
		}
		return nil
	})
	return conn, nil
}

func (w *Worker) serveConnection(protocolCtx context.Context, conn *websocket.Conn) runOutcome {
	connectionCtx, connectionCancel := context.WithCancel(protocolCtx)
	defer connectionCancel()
	group, groupCtx := errgroup.WithContext(connectionCtx)
	closeConnectionOnCancel := context.AfterFunc(groupCtx, func() {
		_ = conn.Close()
	})
	defer closeConnectionOnCancel()

	// group.Wait also waits for the reader and heartbeat, which only stop after
	// connection cancellation. Track the writer separately so graceful
	// shutdown can drain it before cancelling the connection.
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	group.Go(func() error {
		return w.readLoop(groupCtx, conn)
	})
	group.Go(func() error {
		defer writerWG.Done()
		return w.writeLoop(groupCtx, conn)
	})
	group.Go(func() error {
		return w.heartbeatLoop(groupCtx, conn)
	})

	// errgroup.Wait blocks rather than returning a channel, so adapt it to a
	// channel that can be selected alongside server cancellation.
	groupDone := make(chan error, 1)
	go func() {
		groupDone <- group.Wait()
	}()

	select {
	case err := <-groupDone:
		if err != nil && protocolCtx.Err() == nil {
			log.Warnf(w.ctx, "Connection closed: %v", err)
		}
		return runOutcomeConnectionClosed
	case <-w.ctx.Done():
		w.shutdownTasks()
		w.gracefullyCloseConnection(conn, connectionCancel, &writerWG, groupDone)
		return runOutcomeServerShutdown
	case <-w.oneShot.doneChannel():
		select {
		case err := <-groupDone:
			if err != nil && protocolCtx.Err() == nil {
				log.Warnf(w.ctx, "Connection closed: %v", err)
			}
			return runOutcomeConnectionClosed
		default:
		}
		w.gracefullyCloseConnection(conn, connectionCancel, &writerWG, groupDone)
		return runOutcomeOneShotComplete
	}
}

func (w *Worker) gracefullyCloseConnection(
	conn *websocket.Conn,
	connectionCancel context.CancelFunc,
	writerWG *sync.WaitGroup,
	groupDone <-chan error,
) {
	w.outbound.Close()
	writerWG.Wait()

	closeMessage := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	if err := conn.WriteControl(websocket.CloseMessage, closeMessage, time.Now().Add(WriteWait)); err != nil {
		log.Warnf(w.ctx, "Failed to send close message: %v", err)
	}
	connectionCancel()
	<-groupDone
}

func (w *Worker) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if err := conn.SetReadDeadline(time.Now().Add(PongWait)); err != nil {
			return fmt.Errorf("failed to set read deadline: %w", err)
		}
		_, message, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("WebSocket read failed: %w", err)
		}
		log.Debugf(w.ctx, "WebSocket received: %s", string(message))
		w.handleMessage(message)
	}
}

// writeLoop is the single writer of data frames on conn. All data messages
// must go through the outbound queue; nothing else may call WriteMessage on
// conn while this loop is running.
func (w *Worker) writeLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case message, ok := <-w.outbound.Messages():
			if !ok {
				return nil
			}
			log.Debugf(w.ctx, "WebSocket sending: %s", string(message))
			if err := conn.SetWriteDeadline(time.Now().Add(WriteWait)); err != nil {
				return fmt.Errorf("failed to set write deadline: %w", err)
			}
			if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return fmt.Errorf("WebSocket write failed: %w", err)
			}
		}
	}
}

func (w *Worker) heartbeatLoop(ctx context.Context, conn *websocket.Conn) error {
	interval := w.heartbeatInterval
	if interval <= 0 {
		interval = HeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Pings must use WriteControl: it is the only write method that
			// gorilla/websocket documents as safe to call concurrently with
			// the data writes performed by writeLoop. Using WriteMessage here
			// races writeLoop and can panic the process.
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(WriteWait)); err != nil {
				return fmt.Errorf("failed to send ping: %w", err)
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

	// Currently there is only one message type, but we anticipate needing more in the future.
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

	default:
		log.Warnf(w.ctx, "Unknown message type: %s", msg.Type)
	}
}

func (w *Worker) handleTaskCancellation(cancellation *types.TaskCancellationMessage) {
	w.tasksMutex.Lock()
	task, ok := w.activeTasks[cancellation.TaskID]
	if ok {
		if task.cancellationSource == "" {
			task.cancellationSource = taskCancellationSourceUser
			w.activeTasks[cancellation.TaskID] = task
		}
		if task.spawned {
			// executeTask has already returned for a spawned task, so no
			// deferred cleanup will remove the entry; drop it now that the
			// cancellation is being routed to the backend.
			delete(w.activeTasks, cancellation.TaskID)
		}
	}
	w.tasksMutex.Unlock()

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

	w.tasksMutex.Lock()
	rejectionReason := ""
	rejectionMetric := ""
	switch {
	case w.shuttingDown || w.ctx.Err() != nil:
		rejectionReason = "worker is shutting down"
		rejectionMetric = metrics.RejectReasonShuttingDown
	case w.config.OneShot && !w.oneShot.tryAccept():
		rejectionReason = "one-shot worker has already accepted a task"
		rejectionMetric = metrics.RejectReasonOneShotComplete
	default:
		w.taskWG.Add(1)
	}
	w.tasksMutex.Unlock()
	if rejectionReason != "" {
		w.rejectTaskAssignment(taskCtx, span, assignment.TaskID, rejectionReason, rejectionMetric)
		return
	}

	// Check concurrency limit before claiming the task.
	if w.taskSemaphore != nil {
		if !w.taskSemaphore.TryAcquire(1) {
			w.tasksMutex.Lock()
			if w.config.OneShot {
				w.oneShot.resetAcceptance()
			}
			w.tasksMutex.Unlock()
			w.taskWG.Done()
			w.rejectTaskAssignment(taskCtx, span, assignment.TaskID, "worker at maximum concurrency", metrics.RejectReasonAtCapacity)
			return
		}
	}

	executionCtx := taskCtx
	if w.backend.PreservesTasksOnShutdown() {
		executionCtx = context.WithoutCancel(taskCtx)
	}
	taskCtx, taskCancel := context.WithCancel(executionCtx)

	w.tasksMutex.Lock()
	if w.shuttingDown || w.ctx.Err() != nil {
		if w.config.OneShot {
			w.oneShot.resetAcceptance()
		}
		w.tasksMutex.Unlock()
		taskCancel()
		if w.taskSemaphore != nil {
			w.taskSemaphore.Release(1)
		}
		w.taskWG.Done()
		w.rejectTaskAssignment(taskCtx, span, assignment.TaskID, "worker is shutting down", metrics.RejectReasonShuttingDown)
		return
	}
	w.activeTasks[assignment.TaskID] = activeTask{
		ctx:         taskCtx,
		cancel:      taskCancel,
		executionID: assignment.ExecutionID,
	}
	w.tasksMutex.Unlock()

	// It's important to update the task state to claimed as the task lifecycle
	// treats this as a dependency to advance to further states.
	if err := w.sendTaskClaimed(assignment.TaskID); err != nil {
		log.Errorf(w.ctx, "Failed to send task claimed message: %v", err)
	}
	metrics.RecordTaskClaim()
	metrics.AddTaskEvent(taskCtx, "task.claimed")
	metrics.IncTasksActive()
	go func() {
		defer w.taskWG.Done()
		w.executeTask(taskCtx, taskCancel, span, assignment, receivedAt)
	}()
}

func (w *Worker) rejectTaskAssignment(ctx context.Context, span trace.Span, taskID, reason, metricReason string) {
	log.Warnf(w.ctx, "Rejecting task %s: %s", taskID, reason)
	metrics.RecordTaskRejected(metricReason)
	metrics.AddTaskEvent(ctx, "task.rejected",
		attribute.String("reason", metricReason),
	)
	span.End()
	if err := w.sendTaskRejected(taskID, reason); err != nil {
		log.Errorf(w.ctx, "Failed to send task rejected message: %v", err)
	}
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
		if override := w.configuredWarpAgentSidecarImage(); override != "" {
			log.Infof(w.ctx, "Overriding server sidecar image %s with configured sidecar image %s", assignment.SidecarImage, override)
			sidecarImage = override
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

	setupEvents := newSetupEventReporter(w.config.ServerRootURL, assignment)
	if setupEvents == nil {
		// Warn once per task: a fleet-wide config or credential change that
		// disables setup event reporting must be visible in the worker logs,
		// not silently drop the setup metrics.
		reason := "the worker has no server root URL configured"
		if w.config.ServerRootURL != "" {
			reason = warpAPIKeyEnv + " is not present in the task assignment env vars"
		}
		log.Warnf(w.ctx, "Setup event reporting is disabled for task %s: %s", assignment.TaskID, reason)
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
		SetupEvents:   setupEvents,
	}
}

// configuredWarpAgentSidecarImage returns the operator-configured warp-agent sidecar
// image, or empty if neither backend set one. Only one backend config is populated
// at runtime.
func (w *Worker) configuredWarpAgentSidecarImage() string {
	if w.config.Kubernetes != nil && w.config.Kubernetes.SidecarImage != "" {
		return w.config.Kubernetes.SidecarImage
	}
	if w.config.Docker != nil && w.config.Docker.SidecarImage != "" {
		return w.config.Docker.SidecarImage
	}
	return ""
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
	// One-shot-capable backends always wait for a terminal outcome. Register
	// this first so the task bookkeeping defer below runs before completion is
	// signalled.
	defer w.signalOneShotComplete()

	defer func() {
		taskCancel()
		span.End()
		w.tasksMutex.Lock()
		// Spawned tasks stay in activeTasks so a later cancellation can be
		// routed to the backend's CancelTask; everything else is done.
		if task, tracked := w.activeTasks[assignment.TaskID]; !tracked || !task.spawned {
			delete(w.activeTasks, assignment.TaskID)
		}
		w.tasksMutex.Unlock()

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
	metrics.AddTaskEvent(ctx, "backend.started",
		attribute.String("backend", w.config.BackendType),
		attribute.String("docker.image", params.DockerImage),
	)

	executeResult := w.backend.ExecuteTask(ctx, params)
	if executeResult.Error != nil {
		err := executeResult.Error
		if ctx.Err() == context.Canceled && w.cancellationSource(taskID) == taskCancellationSourceUser {
			result = metrics.TaskResultCancelled
			metrics.AddTaskEvent(ctx, "task.cancelled",
				attribute.String("source", string(taskCancellationSourceUser)),
			)
			span.SetStatus(codes.Ok, "task cancelled by user request")
			log.Infof(ctx, "Task execution cancelled by user request: taskID=%s", taskID)
			if statusErr := w.sendTaskCancelled(taskID, assignment.ExecutionID, "Task cancelled by user request."); statusErr != nil {
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
		if statusErr := w.sendTaskFailed(taskID, assignment.ExecutionID, userFacingTaskError(err), metricsReason, exitCode, taskFailureDetails(err)); statusErr != nil {
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
		w.tasksMutex.Lock()
		if task, tracked := w.activeTasks[taskID]; tracked && task.cancellationSource == "" {
			task.spawned = true
			w.activeTasks[taskID] = task
		}
		w.tasksMutex.Unlock()
		metrics.AddTaskEvent(ctx, "task.dispatched")
		span.SetStatus(codes.Ok, "task dispatched to remote runtime")
		log.Infof(ctx, "Task %s dispatched", taskID)
		return
	}

	log.Infof(ctx, "Task execution completed successfully: taskID=%s", taskID)
	metrics.AddTaskEvent(ctx, "task.completed")
	span.SetStatus(codes.Ok, "task completed")
	if err := w.sendTaskCompleted(taskID, assignment.ExecutionID, "Task completed successfully"); err != nil {
		log.Errorf(ctx, "Failed to send task completed message: %v", err)
	}
}

func (w *Worker) signalOneShotComplete() {
	if !w.config.OneShot {
		return
	}
	w.oneShot.complete()
}

func (w *Worker) cancellationSource(taskID string) taskCancellationSource {
	w.tasksMutex.Lock()
	defer w.tasksMutex.Unlock()

	task, ok := w.activeTasks[taskID]
	if !ok {
		return ""
	}
	return task.cancellationSource
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

func (w *Worker) sendTaskCancelled(taskID, executionID, message string) error {
	taskState := types.TaskStateCancelled
	completedMsg := types.TaskCompletedMessage{
		TaskID:      taskID,
		ExecutionID: executionID,
		Message:     message,
		TaskState:   &taskState,
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

func (w *Worker) sendTaskCompleted(taskID, executionID, message string) error {
	completedMsg := types.TaskCompletedMessage{
		TaskID:      taskID,
		ExecutionID: executionID,
		Message:     message,
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

func (w *Worker) sendTaskFailed(taskID, executionID, message string, reason metrics.TaskFailureReason, exitCode int, failureDetails *types.FailureDetails) error {
	failedMsg := types.TaskFailedMessage{
		TaskID:         taskID,
		ExecutionID:    executionID,
		Message:        message,
		FailureReason:  string(reason),
		ExitCode:       exitCode,
		FailureDetails: failureDetails,
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
	if w.outbound == nil {
		return fmt.Errorf("worker send queue is not initialized")
	}
	return w.outbound.Send(message)
}

func (w *Worker) shutdownTasks() {
	preserveActiveTasks := w.backend.PreservesTasksOnShutdown()
	w.tasksMutex.Lock()
	if w.shuttingDown {
		w.tasksMutex.Unlock()
		return
	}
	w.shuttingDown = true
	activeTaskCount := len(w.activeTasks)
	if activeTaskCount > 0 && preserveActiveTasks {
		log.Infof(w.ctx, "Preserving %d active tasks during worker shutdown", activeTaskCount)
	} else if activeTaskCount > 0 {
		log.Infof(w.ctx, "Cancelling %d active tasks", activeTaskCount)
		for taskID, task := range w.activeTasks {
			if task.cancellationSource == "" {
				task.cancellationSource = taskCancellationSourceShutdown
				w.activeTasks[taskID] = task
			}
			log.Debugf(w.ctx, "Cancelling task: %s", taskID)
			metrics.AddTaskEvent(task.ctx, "task.cancellation_requested",
				attribute.String("source", "signal"),
				attribute.String("task.id", taskID),
			)
			task.cancel()
		}
	}
	w.tasksMutex.Unlock()

	if activeTaskCount > 0 && !preserveActiveTasks {
		tasksDone := make(chan struct{})
		go func() {
			w.taskWG.Wait()
			close(tasksDone)
		}()
		select {
		case <-tasksDone:
		case <-time.After(BackendShutdownTimeout):
			log.Warnf(w.ctx, "Timed out waiting for active tasks to finish during shutdown")
		}
	}
}

func (w *Worker) shutdownBackend() {

	backendShutdownCtx, backendShutdownCancel := context.WithTimeout(context.Background(), BackendShutdownTimeout)
	defer backendShutdownCancel()
	w.backend.Shutdown(backendShutdownCtx)
}
