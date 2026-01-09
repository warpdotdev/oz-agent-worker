package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	cliconfig "github.com/docker/cli/cli/config"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/warpdotdev/warp-agent-worker/internal/common"
	"github.com/warpdotdev/warp-agent-worker/internal/log"
	"github.com/warpdotdev/warp-agent-worker/internal/types"
)

const (
	InitialReconnectDelay = 1 * time.Second
	MaxReconnectDelay     = 60 * time.Second
	ReconnectBackoffRate  = 2.0

	HeartbeatInterval = 30 * time.Second
	PongWait          = 60 * time.Second
	WriteWait         = 10 * time.Second
)

type Config struct {
	APIKey        string
	WorkerID      string
	WebSocketURL  string
	ServerRootURL string
	LogLevel      string
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
	dockerClient   *client.Client
}

func New(ctx context.Context, config Config) (*Worker, error) {
	workerCtx, cancel := context.WithCancel(ctx)

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pingCancel()

	// Ping the Docker daemon to ensure it's reachable, as we depend on this.
	if _, err := dockerClient.Ping(pingCtx); err != nil {
		if closeErr := dockerClient.Close(); closeErr != nil {
			log.Warnf(ctx, "Failed to close Docker client: %v", closeErr)
		}
		cancel()
		return nil, fmt.Errorf("failed to reach Docker daemon: %w", err)
	}

	log.Debugf(ctx, "Docker daemon is reachable")

	return &Worker{
		config:         config,
		ctx:            workerCtx,
		cancel:         cancel,
		reconnectDelay: InitialReconnectDelay,
		sendChan:       make(chan []byte, 256),
		activeTasks:    make(map[string]context.CancelFunc),
		dockerClient:   dockerClient,
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
			time.Sleep(w.reconnectDelay)

			// Compute exponential back-off.
			w.reconnectDelay = min(time.Duration(float64(w.reconnectDelay)*ReconnectBackoffRate), MaxReconnectDelay)
			continue
		}

		w.reconnectDelay = InitialReconnectDelay

		w.run()
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

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), headers)
	if err != nil {
		return fmt.Errorf("failed to dial WebSocket: %w", err)
	}

	w.connMutex.Lock()
	w.conn = conn
	w.connMutex.Unlock()

	log.Infof(w.ctx, "Successfully connected to server")

	conn.SetPongHandler(func(string) error {
		w.lastHeartbeat = time.Now()
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

		log.Infof(w.ctx, "WebSocket received: %s", string(message))

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

			log.Infof(w.ctx, "WebSocket sending: %s", string(message))

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
		w.handleTaskCancellation(cancellation.TaskID)

	default:
		log.Warnf(w.ctx, "Unknown message type: %s", msg.Type)
	}
}

func (w *Worker) handleTaskAssignment(assignment *types.TaskAssignmentMessage) {
	log.Infof(w.ctx, "Received task assignment: taskID=%s, title=%s", assignment.TaskID, assignment.Task.Title)

	// It's important to update the task state to claimed as the task lifecycle treats this as a dependency to advance to further states.
	if err := w.sendTaskClaimed(assignment.TaskID); err != nil {
		log.Errorf(w.ctx, "Failed to send task claimed message: %v", err)
	}

	taskCtx, taskCancel := context.WithCancel(w.ctx)

	w.tasksMutex.Lock()
	w.activeTasks[assignment.TaskID] = taskCancel
	w.tasksMutex.Unlock()

	go w.executeTask(taskCtx, assignment)
}

func (w *Worker) handleTaskCancellation(taskID string) {
	w.tasksMutex.Lock()
	cancelFunc, exists := w.activeTasks[taskID]
	w.tasksMutex.Unlock()

	if !exists {
		log.Warnf(w.ctx, "Cannot cancel task %s: task not found or already completed", taskID)
		return
	}

	log.Infof(w.ctx, "Cancelling task: %s", taskID)
	cancelFunc()
}

func (w *Worker) executeTask(ctx context.Context, assignment *types.TaskAssignmentMessage) {
	defer func() {
		w.tasksMutex.Lock()
		delete(w.activeTasks, assignment.TaskID)
		w.tasksMutex.Unlock()
	}()

	taskID := assignment.TaskID
	log.Infof(ctx, "Starting task execution: taskID=%s, title=%s", taskID, assignment.Task.Title)

	if err := w.executeTaskInDocker(ctx, assignment); err != nil {
		log.Errorf(ctx, "Task launch failed: taskID=%s, error=%v", taskID, err)
		if statusErr := w.sendTaskFailed(taskID, fmt.Sprintf("Failed to launch task: %v", err)); statusErr != nil {
			log.Errorf(ctx, "Failed to send task failed message: %v", statusErr)
		}
		return
	}

	log.Infof(ctx, "Task container started successfully: taskID=%s", taskID)
}

func (w *Worker) executeTaskInDocker(ctx context.Context, assignment *types.TaskAssignmentMessage) error {
	task := assignment.Task
	dockerClient := w.dockerClient

	var imageName string
	if assignment.DockerImage != "" {
		imageName = assignment.DockerImage
		log.Debugf(ctx, "Using Docker image from assignment: %s", imageName)
	} else {
		imageName = "ubuntu:22.04"
		if task.AgentConfigSnapshot.EnvironmentID != nil {
			log.Warnf(ctx, "Environment %s specified but no Docker image resolved. Using default: %s",
				*task.AgentConfigSnapshot.EnvironmentID, imageName)
		} else {
			log.Infof(ctx, "No environment specified, using default image: %s", imageName)
		}
	}

	log.Debugf(ctx, "Pulling Docker image: %s", imageName)

	cfg, err := cliconfig.Load("")
	if err != nil {
		log.Warnf(ctx, "Failed to load Docker config: %v. Attempting pull without auth.", err)
	}

	var authStr string
	if cfg != nil {
		// This is the default registry if not specified in the `imageName`.
		registryURL := "https://index.docker.io/v1/"

		if strings.Contains(imageName, "/") {
			parts := strings.SplitN(imageName, "/", 2)
			if strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") {
				registryURL = parts[0]
			}
		}

		authConfig, err := cfg.GetAuthConfig(registryURL)
		if err != nil {
			log.Warnf(ctx, "Failed to get auth config for registry %s: %v", registryURL, err)
		} else if authConfig.Username != "" {
			authJSON, _ := json.Marshal(authConfig)
			authStr = base64.URLEncoding.EncodeToString(authJSON)
			log.Infof(ctx, "Using Docker credentials for registry %s (username: %s)", registryURL, authConfig.Username)
		} else {
			log.Warnf(ctx, "No username found in auth config for registry %s", registryURL)
		}
	}

	pullOptions := image.PullOptions{
		Platform:     "linux/amd64",
		RegistryAuth: authStr,
	}
	reader, err := dockerClient.ImagePull(ctx, imageName, pullOptions)
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %w", imageName, err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			log.Warnf(ctx, "Failed to close image pull reader: %v", err)
		}
	}()

	// The image pull doesn't actually happen until you read from this stream, but we don't need the output.
	_, err = io.Copy(io.Discard, reader)
	if err != nil {
		return fmt.Errorf("failed to read image pull output: %w", err)
	}
	log.Infof(ctx, "Successfully pulled Docker image: %s", imageName)

	if assignment.SidecarImage == "" {
		return fmt.Errorf("no sidecar image specified in assignment")
	}

	log.Debugf(ctx, "Checking if sidecar image %s exists locally", assignment.SidecarImage)
	_, err = dockerClient.ImageInspect(ctx, assignment.SidecarImage)
	if err == nil {
		log.Debugf(ctx, "Sidecar image %s already exists locally, skipping pull", assignment.SidecarImage)
	} else {
		log.Infof(ctx, "Pulling sidecar image: %s", assignment.SidecarImage)

		sidecarReader, err := dockerClient.ImagePull(ctx, assignment.SidecarImage, pullOptions)
		if err != nil {
			return fmt.Errorf("failed to pull sidecar image %s: %w", assignment.SidecarImage, err)
		}
		_, err = io.Copy(io.Discard, sidecarReader)
		if closeErr := sidecarReader.Close(); closeErr != nil {
			log.Warnf(ctx, "Failed to close sidecar reader: %v", closeErr)
		}
		if err != nil {
			return fmt.Errorf("failed to read sidecar image pull output: %w", err)
		}
		log.Infof(ctx, "Successfully pulled sidecar image: %s", assignment.SidecarImage)
	}

	volumeName := sanitizeImageNameForVolume(assignment.SidecarImage)
	log.Debugf(ctx, "Using shared volume: %s for sidecar image: %s", volumeName, assignment.SidecarImage)

	_, err = dockerClient.VolumeInspect(ctx, volumeName)
	if err == nil {
		log.Debugf(ctx, "Reusing existing volume %s (already populated from sidecar)", volumeName)
	} else {
		log.Infof(ctx, "Creating new Docker volume: %s", volumeName)
		volumeResp, err := dockerClient.VolumeCreate(ctx, volume.CreateOptions{
			Name: volumeName,
		})
		if err != nil {
			return fmt.Errorf("failed to create volume: %w", err)
		}
		log.Debugf(ctx, "Created volume: %s at %s", volumeName, volumeResp.Mountpoint)

		log.Debugf(ctx, "Copying warp agent from sidecar to volume (first time)")

		if err := w.copySidecarFilesystemToVolume(ctx, dockerClient, assignment.SidecarImage, volumeName); err != nil {
			return fmt.Errorf("failed to copy sidecar to volume: %w", err)
		}
	}

	envVars := []string{
		fmt.Sprintf("WARP_API_KEY=%s", w.config.APIKey),
		fmt.Sprintf("TASK_ID=%s", task.ID),
		"GIT_TERMINAL_PROMPT=0",
		"GH_PROMPT_DISABLED=1",
	}

	if assignment.GitHubToken != "" {
		log.Infof(ctx, "Setting GitHub token from assignment for task %s", task.ID)
		envVars = append(envVars, fmt.Sprintf("GITHUB_ACCESS_TOKEN=%s", assignment.GitHubToken))
	} else {
		log.Warnf(ctx, "No GitHub token provided in assignment for task %s", task.ID)
	}

	cmd := []string{
		"/bin/sh",
		"/agent/entrypoint.sh",
		"agent",
		"run",
		"--share",
		"team:edit",
		"--task-id",
		task.ID,
		"--prompt",
		common.EffectivePromptForTask(task),
		"--sandboxed",
		"--server-root-url",
		w.config.ServerRootURL,
	}

	cmd = common.AugmentArgsForTask(task, cmd)

	log.Debugf(ctx, "Creating Docker container with image=%s", imageName)

	containerConfig := &container.Config{
		Image:      imageName,
		Cmd:        cmd,
		Env:        envVars,
		WorkingDir: "/workspace",
	}

	hostConfig := &container.HostConfig{
		AutoRemove: true,
		Binds: []string{
			fmt.Sprintf("%s:/agent:ro", volumeName),
		},
	}

	resp, err := dockerClient.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	containerID := resp.ID
	log.Debugf(ctx, "Created Docker container: %s", containerID)

	defer func() {
		if containerID != "" {
			if removeErr := dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); removeErr != nil {
				log.Debugf(ctx, "Container %s already removed or removal failed: %v", containerID, removeErr)
			}
		}
	}()

	if err := dockerClient.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	log.Debugf(ctx, "Started Docker container: %s", containerID)

	statusCh, errCh := dockerClient.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("error waiting for container: %w", err)
		}
	case status := <-statusCh:
		log.Debugf(ctx, "Container exited with status code: %d", status.StatusCode)

		logOutput, logErr := w.getContainerLogs(ctx, dockerClient, containerID)
		if zerolog.GlobalLevel() <= zerolog.DebugLevel {
			if logErr != nil {
				log.Warnf(ctx, "Failed to get container logs: %v", logErr)
			} else if logOutput != "" {
				log.Debugf(ctx, "Container output:\n%s", logOutput)
			}
		}

		if status.StatusCode != 0 {
			errorMsg := fmt.Sprintf("container exited with non-zero status: %d", status.StatusCode)
			if logOutput != "" {
				lines := strings.Split(logOutput, "\n")
				if len(lines) > 10 {
					lines = lines[len(lines)-10:]
				}
				errorMsg = fmt.Sprintf("%s. Last output:\n%s", errorMsg, strings.Join(lines, "\n"))
			}
			return fmt.Errorf("%s", errorMsg)
		}
	}

	log.Infof(ctx, "Task %s execution completed successfully", task.ID)
	return nil
}

func (w *Worker) getContainerLogs(ctx context.Context, dockerClient *client.Client, containerID string) (string, error) {
	out, err := dockerClient.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: false,
	})
	if err != nil {
		return "", err
	}
	defer func() {
		if err := out.Close(); err != nil {
			log.Warnf(ctx, "Failed to close container logs reader: %v", err)
		}
	}()

	logBytes, err := io.ReadAll(out)
	if err != nil {
		return "", err
	}

	return string(logBytes), nil
}

// copySidecarFilesystemToVolume takes an image and creates a volume from its filesystem.
// We mount this volume into the image for each task as a means of predictably injecting dependencies.
// This is basically the `sidecar_volume` concept in `namespace.so`:
// https://buf.build/namespace/cloud/docs/main:namespace.cloud.compute.v1beta#namespace.cloud.compute.v1beta.ContainerRequest
func (w *Worker) copySidecarFilesystemToVolume(ctx context.Context, dockerClient *client.Client, sidecarImage, volumeName string) error {
	log.Infof(ctx, "Creating temporary container from sidecar image")
	sidecarConfig := &container.Config{
		Image: sidecarImage,
		Cmd:   []string{"true"},
	}

	sidecarResp, err := dockerClient.ContainerCreate(ctx, sidecarConfig, nil, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create sidecar container: %w", err)
	}

	sidecarContainerID := sidecarResp.ID
	defer func() {
		if removeErr := dockerClient.ContainerRemove(ctx, sidecarContainerID, container.RemoveOptions{Force: true}); removeErr != nil {
			log.Warnf(ctx, "Failed to remove sidecar container %s: %v", sidecarContainerID, removeErr)
		}
	}()

	log.Infof(ctx, "Created sidecar container: %s", sidecarContainerID)

	// Export the full filesystem of the sidecar.
	tarReader, err := dockerClient.ContainerExport(ctx, sidecarContainerID)
	if err != nil {
		return fmt.Errorf("failed to export sidecar container: %w", err)
	}
	defer func() {
		if err := tarReader.Close(); err != nil {
			log.Warnf(ctx, "Failed to close tar reader: %v", err)
		}
	}()

	log.Infof(ctx, "Extracting sidecar filesystem to volume")

	// Use an arbitrary image to copy the exported filesystem onto a volume.
	alpineImage := "alpine:latest"
	alpineReader, err := dockerClient.ImagePull(ctx, alpineImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull alpine image: %w", err)
	}
	if _, err := io.Copy(io.Discard, alpineReader); err != nil {
		log.Warnf(ctx, "Error reading alpine image pull output: %v", err)
	}
	if err := alpineReader.Close(); err != nil {
		log.Warnf(ctx, "Failed to close alpine reader: %v", err)
	}

	extractConfig := &container.Config{
		Image:        alpineImage,
		Cmd:          []string{"tar", "-x", "-C", "/target"},
		StdinOnce:    true,
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}

	extractHostConfig := &container.HostConfig{
		Binds: []string{
			fmt.Sprintf("%s:/target", volumeName),
		},
	}

	extractResp, err := dockerClient.ContainerCreate(ctx, extractConfig, extractHostConfig, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create extraction container: %w", err)
	}

	extractContainerID := extractResp.ID
	defer func() {
		if removeErr := dockerClient.ContainerRemove(ctx, extractContainerID, container.RemoveOptions{Force: true}); removeErr != nil {
			log.Warnf(ctx, "Failed to remove extraction container %s: %v", extractContainerID, removeErr)
		}
	}()

	log.Infof(ctx, "Created extraction container: %s", extractContainerID)

	attachResp, err := dockerClient.ContainerAttach(ctx, extractContainerID, container.AttachOptions{
		Stdin:  true,
		Stream: true,
	})
	if err != nil {
		return fmt.Errorf("failed to attach to extraction container: %w", err)
	}
	defer attachResp.Close()

	if err := dockerClient.ContainerStart(ctx, extractContainerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start extraction container: %w", err)
	}

	go func() {
		defer func() {
			if err := attachResp.CloseWrite(); err != nil {
				log.Warnf(ctx, "Failed to close write side of attach: %v", err)
			}
		}()
		if _, err := io.Copy(attachResp.Conn, tarReader); err != nil {
			log.Warnf(ctx, "Error copying tar data: %v", err)
		}
	}()

	statusCh, errCh := dockerClient.ContainerWait(ctx, extractContainerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("error waiting for extraction container: %w", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			logOutput, _ := w.getContainerLogs(ctx, dockerClient, extractContainerID)
			return fmt.Errorf("extraction container exited with status %d. Logs: %s", status.StatusCode, logOutput)
		}
		log.Infof(ctx, "Successfully extracted sidecar filesystem to volume %s", volumeName)
	}

	return nil
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

func sanitizeImageNameForVolume(imageName string) string {
	sanitized := strings.ReplaceAll(imageName, "/", "-")
	sanitized = strings.ReplaceAll(sanitized, ":", "-")
	return sanitized
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

	if w.dockerClient != nil {
		if err := w.dockerClient.Close(); err != nil {
			log.Warnf(w.ctx, "Failed to close Docker client: %v", err)
		}
	}

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
