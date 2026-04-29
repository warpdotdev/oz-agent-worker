package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/warpdotdev/oz-agent-worker/internal/common"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
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
)

type Config struct {
	APIKey             string
	WorkerID           string
	WebSocketURL       string
	ServerRootURL      string
	LogLevel           string
	BackendType        string // "docker", "direct", or "kubernetes"
	MaxConcurrentTasks int    // 0 means unlimited
	// IdleOnComplete is passed to the oz CLI's --idle-on-complete flag for every task.
	// Empty string means use the oz CLI default (45m). Use "0s" to disable idle.
	IdleOnComplete string
	// SessionSharingServerURL, when non-empty, is forwarded to the oz CLI via --session-sharing-server-url.
	SessionSharingServerURL string

	// Backend-specific configs. Only the one matching BackendType should be set.
	Docker     *DockerBackendConfig
	Direct     *DirectBackendConfig
	Kubernetes *KubernetesBackendConfig
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
	activeTasks    map[string]context.CancelFunc
	tasksMutex     sync.Mutex
	backend        Backend
	taskSemaphore  *semaphore.Weighted // nil when unlimited
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

	return &Worker{
		config:         config,
		ctx:            workerCtx,
		cancel:         cancel,
		reconnectDelay: InitialReconnectDelay,
		sendChan:       make(chan []byte, 256),
		activeTasks:    make(map[string]context.CancelFunc),
		backend:        backend,
		taskSemaphore:  taskSemaphore,
	}, nil
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
	done := make(chan struct{})

	go w.readLoop(done)
	go w.writeLoop(done)
	go w.heartbeatLoop(done)

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

func (w *Worker) readLoop(done chan struct{}) {
	defer close(done)

	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		w.connMutex.Lock()
		conn := w.conn
		w.connMutex.Unlock()

		if conn == nil {
			return
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

func (w *Worker) writeLoop(done chan struct{}) {
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-done:
			return
		case message := <-w.sendChan:
			w.connMutex.Lock()
			conn := w.conn
			w.connMutex.Unlock()

			if conn == nil {
				return
			}

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

func (w *Worker) heartbeatLoop(done chan struct{}) {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			w.connMutex.Lock()
			conn := w.conn
			w.connMutex.Unlock()

			if conn == nil {
				return
			}

			if err := conn.SetWriteDeadline(time.Now().Add(WriteWait)); err != nil {
				log.Errorf(w.ctx, "Failed to set write deadline: %v", err)
				return
			}
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
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

	// Currently there is only one message type, but we anticipate needing more in the future.
	switch msg.Type {
	case types.MessageTypeTaskAssignment:
		var assignment types.TaskAssignmentMessage
		if err := json.Unmarshal(msg.Data, &assignment); err != nil {
			log.Errorf(w.ctx, "Failed to unmarshal task assignment: %v", err)
			return
		}
		w.handleTaskAssignment(&assignment)

	default:
		log.Warnf(w.ctx, "Unknown message type: %s", msg.Type)
	}
}

func (w *Worker) handleTaskAssignment(assignment *types.TaskAssignmentMessage) {
	log.Infof(w.ctx, "Received task assignment: taskID=%s, title=%s", assignment.TaskID, assignment.Task.Title)

	// Check concurrency limit before claiming the task.
	if w.taskSemaphore != nil {
		if !w.taskSemaphore.TryAcquire(1) {
			log.Warnf(w.ctx, "Rejecting task %s: worker at maximum concurrency (%d)", assignment.TaskID, w.config.MaxConcurrentTasks)
			metrics.RecordTaskRejected(metrics.RejectReasonAtCapacity)
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
	metrics.IncTasksActive()

	taskCtx, taskCancel := context.WithCancel(w.ctx)

	w.tasksMutex.Lock()
	w.activeTasks[assignment.TaskID] = taskCancel
	w.tasksMutex.Unlock()

	go w.executeTask(taskCtx, assignment)
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
	for key, value := range assignment.EnvVars {
		envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
	}

	// Build base CLI arguments shared across all backends.
	baseArgs := []string{
		"agent",
		"run",
		"--share",
		"team:edit",
		"--task-id",
		task.ID,
		"--sandboxed",
		"--server-root-url",
		w.config.ServerRootURL,
	}
	baseArgs = common.AugmentArgsForTask(task, baseArgs, common.TaskAugmentOptions{
		IdleOnComplete: w.config.IdleOnComplete,
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

	return &TaskParams{
		TaskID:      assignment.TaskID,
		Task:        task,
		EnvVars:     envVars,
		BaseArgs:    baseArgs,
		DockerImage: dockerImage,
		Sidecars:    sidecars,
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

func (w *Worker) executeTask(ctx context.Context, assignment *types.TaskAssignmentMessage) {
	start := time.Now()
	result := metrics.TaskResultSucceeded

	defer func() {
		w.tasksMutex.Lock()
		delete(w.activeTasks, assignment.TaskID)
		w.tasksMutex.Unlock()

		if w.taskSemaphore != nil {
			w.taskSemaphore.Release(1)
		}

		metrics.DecTasksActive()
		metrics.RecordTaskCompleted(result, time.Since(start))
	}()

	taskID := assignment.TaskID
	log.Infof(ctx, "Starting task execution: taskID=%s, title=%s", taskID, assignment.Task.Title)

	params := w.prepareTaskParams(assignment)
	if err := w.backend.ExecuteTask(ctx, params); err != nil {
		result = metrics.TaskResultFailed
		log.Errorf(ctx, "Task execution failed: taskID=%s, error=%v", taskID, err)
		if statusErr := w.sendTaskFailed(taskID, fmt.Sprintf("Failed to execute task: %v", err)); statusErr != nil {
			log.Errorf(ctx, "Failed to send task failed message: %v", statusErr)
		}
		return
	}

	log.Infof(ctx, "Task execution completed successfully: taskID=%s", taskID)
	if err := w.sendTaskCompleted(taskID, "Task completed successfully"); err != nil {
		log.Errorf(ctx, "Failed to send task completed message: %v", err)
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

func (w *Worker) sendTaskFailed(taskID, message string) error {
	failedMsg := types.TaskFailedMessage{
		TaskID:  taskID,
		Message: message,
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

func (w *Worker) Shutdown() {
	log.Infof(w.ctx, "Shutting down worker...")

	w.tasksMutex.Lock()
	activeTaskCount := len(w.activeTasks)
	if activeTaskCount > 0 {
		log.Infof(w.ctx, "Cancelling %d active tasks", activeTaskCount)
		for taskID, cancel := range w.activeTasks {
			log.Debugf(w.ctx, "Cancelling task: %s", taskID)
			cancel()
		}
	}
	w.tasksMutex.Unlock()

	if activeTaskCount > 0 {
		time.Sleep(500 * time.Millisecond)
	}

	w.cancel()
	backendShutdownCtx, backendShutdownCancel := context.WithTimeout(context.Background(), BackendShutdownTimeout)
	defer backendShutdownCancel()
	w.backend.Shutdown(backendShutdownCtx)

	w.connMutex.Lock()
	if w.conn != nil {
		if err := w.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")); err != nil {
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
