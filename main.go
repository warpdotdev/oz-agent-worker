package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
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
	TaskID       string      `json:"task_id"`
	Task         *types.Task `json:"task"`
	DockerImage  string      `json:"docker_image,omitempty"`
	SidecarImage string      `json:"sidecar_image,omitempty"`
	GitHubToken  string      `json:"github_token,omitempty"`
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

type WorkerConfig struct {
	APIKey        string
	WorkerID      string
	WebSocketURL  string
	ServerRootURL string
	LogLevel      string
}

type Worker struct {
	config         WorkerConfig
	conn           *websocket.Conn
	connMutex      sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	reconnectDelay time.Duration
	lastHeartbeat  time.Time
	connected      bool
	sendChan       chan []byte
	activeTasks    map[string]context.CancelFunc // taskID -> cancel function
	tasksMutex     sync.Mutex
	dockerClient   *client.Client
}

var CLI struct {
	APIKey        string `help:"API key for authentication" env:"WARP_API_KEY" required:""`
	WorkerID      string `help:"Worker host identifier" required:""`
	WebSocketURL  string `default:"wss://app.warp.dev/api/v1/selfhosted/worker/ws" hidden:""`
	ServerRootURL string `default:"https://app.warp.dev" hidden:""`
	LogLevel      string `help:"Log level (debug, info, warn, error)" default:"info" enum:"debug,info,warn,error"`
}

func main() {
	ctx := context.Background()

	kong.Parse(&CLI,
		kong.Name("warp-agent-worker"),
		kong.Description("Self-hosted worker for Warp ambient agents."),
		kong.UsageOnError(),
		kong.Vars{},
	)

	configureLogging(CLI.LogLevel)

	config := WorkerConfig{
		APIKey:        CLI.APIKey,
		WorkerID:      CLI.WorkerID,
		WebSocketURL:  CLI.WebSocketURL,
		ServerRootURL: CLI.ServerRootURL,
		LogLevel:      CLI.LogLevel,
	}

	worker, err := NewWorker(ctx, config)
	if err != nil {
		log.Fatalf(ctx, "Failed to create worker: %v", err)
	}

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start worker in background
	go func() {
		if err := worker.Start(); err != nil {
			log.Errorf(ctx, "Worker stopped with error: %v", err)
		}
	}()

	// Wait for signal
	sig := <-sigChan
	log.Infof(ctx, "Received signal %v, shutting down gracefully...", sig)

	worker.Shutdown()

	log.Infof(ctx, "Worker shutdown complete")
}

func configureLogging(level string) {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	var logLevel zerolog.Level
	switch level {
	case "debug":
		logLevel = zerolog.DebugLevel
	case "info":
		logLevel = zerolog.InfoLevel
	case "warn":
		logLevel = zerolog.WarnLevel
	case "error":
		logLevel = zerolog.ErrorLevel
	default:
		logLevel = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(logLevel)
}

func NewWorker(ctx context.Context, config WorkerConfig) (*Worker, error) {
	workerCtx, cancel := context.WithCancel(ctx)

	// Initialize Docker client and verify daemon is reachable
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	// Ping the Docker daemon with a short timeout to verify connectivity
	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pingCancel()

	if _, err := dockerClient.Ping(pingCtx); err != nil {
		dockerClient.Close()
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

			// Exponential backoff
			w.reconnectDelay = time.Duration(float64(w.reconnectDelay) * ReconnectBackoffRate)
			if w.reconnectDelay > MaxReconnectDelay {
				w.reconnectDelay = MaxReconnectDelay
			}
			continue
		}

		// Reset reconnect delay on successful connection
		w.reconnectDelay = InitialReconnectDelay

		w.run()
	}
}

func (w *Worker) connect() error {
	// Build WebSocket URL with query parameters
	u, err := url.Parse(w.config.WebSocketURL)
	if err != nil {
		return fmt.Errorf("invalid WebSocket URL: %w", err)
	}

	query := u.Query()
	query.Set("worker_id", w.config.WorkerID)
	u.RawQuery = query.Encode()

	// Set up WebSocket headers with API key in Authorization header
	headers := make(map[string][]string)
	headers["Authorization"] = []string{fmt.Sprintf("Bearer %s", w.config.APIKey)}

	log.Infof(w.ctx, "Connecting to %s", u.String())

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), headers)
	if err != nil {
		return fmt.Errorf("failed to dial WebSocket: %w", err)
	}

	w.connMutex.Lock()
	w.conn = conn
	w.connected = true
	w.connMutex.Unlock()

	log.Infof(w.ctx, "Successfully connected to server")

	// Set up pong handler
	conn.SetPongHandler(func(string) error {
		w.lastHeartbeat = time.Now()
		return nil
	})

	return nil
}

func (w *Worker) run() {
	// Start read and write loops
	done := make(chan struct{})

	go w.readLoop(done)
	go w.writeLoop(done)
	go w.heartbeatLoop(done)

	// Wait for any loop to finish
	<-done

	// Close connection
	w.connMutex.Lock()
	if w.conn != nil {
		w.conn.Close()
		w.conn = nil
	}
	w.connected = false
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

		conn.SetReadDeadline(time.Now().Add(PongWait))
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

			conn.SetWriteDeadline(time.Now().Add(WriteWait))
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

			// Send ping
			conn.SetWriteDeadline(time.Now().Add(WriteWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Errorf(w.ctx, "Failed to send ping: %v", err)
				return
			}
		}
	}
}

func (w *Worker) handleMessage(message []byte) {
	log.Debugf(w.ctx, "Received message: %s", string(message))

	var msg WebSocketMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		log.Errorf(w.ctx, "Failed to unmarshal message: %v", err)
		return
	}

	switch msg.Type {
	case MessageTypeTaskAssignment:
		var assignment TaskAssignmentMessage
		if err := json.Unmarshal(msg.Data, &assignment); err != nil {
			log.Errorf(w.ctx, "Failed to unmarshal task assignment: %v", err)
			return
		}
		w.handleTaskAssignment(&assignment)

	default:
		log.Warnf(w.ctx, "Unknown message type: %s", msg.Type)
	}
}

func (w *Worker) handleTaskAssignment(assignment *TaskAssignmentMessage) {
	log.Infof(w.ctx, "Received task assignment: taskID=%s, title=%s", assignment.TaskID, assignment.Task.Title)

	// Send task claimed message immediately
	if err := w.sendTaskClaimed(assignment.TaskID); err != nil {
		log.Errorf(w.ctx, "Failed to send task claimed message: %v", err)
		// Continue anyway - we'll still try to execute the task
	}

	taskCtx, taskCancel := context.WithCancel(w.ctx)

	// Track active task
	w.tasksMutex.Lock()
	w.activeTasks[assignment.TaskID] = taskCancel
	w.tasksMutex.Unlock()

	// Execute task in background
	go w.executeTask(taskCtx, assignment)
}

// executeTask executes a task assignment
func (w *Worker) executeTask(ctx context.Context, assignment *TaskAssignmentMessage) {
	defer func() {
		// Clean up active task tracking
		w.tasksMutex.Lock()
		delete(w.activeTasks, assignment.TaskID)
		w.tasksMutex.Unlock()
	}()

	taskID := assignment.TaskID
	log.Infof(ctx, "Starting task execution: taskID=%s, title=%s", taskID, assignment.Task.Title)

	// Execute the task using Docker
	// If this fails, the worker notifies the server. Otherwise, the agent process
	// running inside Docker will handle all status updates.
	if err := w.executeTaskInDocker(ctx, assignment); err != nil {
		log.Errorf(ctx, "Task launch failed: taskID=%s, error=%v", taskID, err)
		if statusErr := w.sendTaskFailed(taskID, fmt.Sprintf("Failed to launch task: %v", err)); statusErr != nil {
			log.Errorf(ctx, "Failed to send task failed message: %v", statusErr)
		}
		return
	}

	log.Infof(ctx, "Task container started successfully: taskID=%s", taskID)
}

// executeTaskInDocker executes the task using Docker container runtime
func (w *Worker) executeTaskInDocker(ctx context.Context, assignment *TaskAssignmentMessage) error {
	task := assignment.Task
	dockerClient := w.dockerClient

	// Get the OCI image from the assignment (server has already resolved it)
	var imageName string
	if assignment.DockerImage != "" {
		imageName = assignment.DockerImage
		log.Infof(ctx, "Using Docker image from assignment: %s", imageName)
	} else if task.AgentConfigSnapshot != nil && task.AgentConfigSnapshot.EnvironmentID != nil {
		// Fallback: environment specified but no image resolved by server
		imageName = "ubuntu:22.04"
		log.Warnf(ctx, "Environment %s specified but no Docker image resolved. Using default: %s",
			*task.AgentConfigSnapshot.EnvironmentID, imageName)
	} else {
		// No environment specified at all
		imageName = "ubuntu:22.04"
		log.Infof(ctx, "No environment specified, using default image: %s", imageName)
	}

	// Pull the Docker image with platform specification and authentication
	// Warp agent images are typically built for linux/amd64
	log.Infof(ctx, "Pulling Docker image: %s", imageName)

	// Load Docker config to get registry authentication
	cfg, err := cliconfig.Load("")
	if err != nil {
		log.Warnf(ctx, "Failed to load Docker config: %v. Attempting pull without auth.", err)
	}

	// Get auth for the image's registry
	var authStr string
	if cfg != nil {
		// For Docker Hub images (no explicit registry), use the default Docker Hub registry
		registryURL := "https://index.docker.io/v1/"

		// Check if image has an explicit registry (contains domain with dot or colon)
		if strings.Contains(imageName, "/") {
			parts := strings.SplitN(imageName, "/", 2)
			if strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") {
				registryURL = parts[0]
			}
		}

		// Get credentials for the registry
		authConfig, err := cfg.GetAuthConfig(registryURL)
		if err != nil {
			log.Warnf(ctx, "Failed to get auth config for registry %s: %v", registryURL, err)
		} else if authConfig.Username != "" {
			// Encode auth as base64 JSON
			authJSON, _ := json.Marshal(authConfig)
			authStr = base64.URLEncoding.EncodeToString(authJSON)
			log.Infof(ctx, "Using Docker credentials for registry %s (username: %s)", registryURL, authConfig.Username)
		} else {
			log.Warnf(ctx, "No username found in auth config for registry %s", registryURL)
		}
	}

	pullOptions := image.PullOptions{
		Platform:     "linux/amd64", // Specify platform for compatibility
		RegistryAuth: authStr,       // Pass authentication
	}
	reader, err := dockerClient.ImagePull(ctx, imageName, pullOptions)
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %w", imageName, err)
	}
	defer reader.Close()

	// Read the pull output to completion (required for pull to actually happen)
	_, err = io.Copy(io.Discard, reader)
	if err != nil {
		return fmt.Errorf("failed to read image pull output: %w", err)
	}
	log.Infof(ctx, "Successfully pulled Docker image: %s", imageName)

	// Pull the sidecar image containing warp agent
	if assignment.SidecarImage == "" {
		return fmt.Errorf("no sidecar image specified in assignment")
	}

	// Check if sidecar image already exists locally
	log.Infof(ctx, "Checking if sidecar image %s exists locally", assignment.SidecarImage)
	_, err = dockerClient.ImageInspect(ctx, assignment.SidecarImage)
	if err == nil {
		log.Infof(ctx, "Sidecar image %s already exists locally, skipping pull", assignment.SidecarImage)
	} else {
		log.Infof(ctx, "Sidecar image not found locally (error: %v), will pull", err)
		// Image doesn't exist locally, pull it
		log.Infof(ctx, "Pulling sidecar image: %s", assignment.SidecarImage)

		sidecarReader, err := dockerClient.ImagePull(ctx, assignment.SidecarImage, pullOptions)
		if err != nil {
			return fmt.Errorf("failed to pull sidecar image %s: %w", assignment.SidecarImage, err)
		}
		_, err = io.Copy(io.Discard, sidecarReader)
		sidecarReader.Close()
		if err != nil {
			return fmt.Errorf("failed to read sidecar image pull output: %w", err)
		}
		log.Infof(ctx, "Successfully pulled sidecar image: %s", assignment.SidecarImage)
	}

	// Create a shared Docker volume to hold the warp agent files from the sidecar
	// Since the volume is mounted read-only, we can reuse it across tasks for the same sidecar image
	// Use a sanitized version of the sidecar image name as the volume name
	volumeName := sanitizeImageNameForVolume(assignment.SidecarImage)
	log.Infof(ctx, "Using shared volume: %s for sidecar image: %s", volumeName, assignment.SidecarImage)

	// Check if the volume already exists
	_, err = dockerClient.VolumeInspect(ctx, volumeName)
	if err == nil {
		log.Infof(ctx, "Reusing existing volume %s (already populated from sidecar)", volumeName)
	} else {
		// Volume doesn't exist, create and populate it
		log.Infof(ctx, "Creating new Docker volume: %s", volumeName)
		volumeResp, err := dockerClient.VolumeCreate(ctx, volume.CreateOptions{
			Name: volumeName,
		})
		if err != nil {
			return fmt.Errorf("failed to create volume: %w", err)
		}
		log.Infof(ctx, "Created volume: %s at %s", volumeName, volumeResp.Mountpoint)

		// Copy warp agent files from sidecar image to volume using docker export
		// This emulates Namespace's sidecar volume feature:
		// 1. Create a container from sidecar image
		// 2. Export the container filesystem as a tar archive
		// 3. Extract the tar into the volume using an alpine container
		log.Infof(ctx, "Copying warp agent from sidecar to volume (first time)")

		if err := w.copySidecarToVolume(ctx, dockerClient, assignment.SidecarImage, volumeName); err != nil {
			return fmt.Errorf("failed to copy sidecar to volume: %w", err)
		}
	}

	// Build environment variables using worker's own API key
	envVars := []string{
		fmt.Sprintf("WARP_API_KEY=%s", w.config.APIKey),
		fmt.Sprintf("TASK_ID=%s", task.ID),
		"GIT_TERMINAL_PROMPT=0",
		"GH_PROMPT_DISABLED=1",
	}

	// Add GitHub credentials if available from assignment
	if assignment.GitHubToken != "" {
		log.Infof(ctx, "Setting GitHub token from assignment for task %s", task.ID)
		envVars = append(envVars, fmt.Sprintf("GITHUB_ACCESS_TOKEN=%s", assignment.GitHubToken))
	} else {
		log.Warnf(ctx, "No GitHub token provided in assignment for task %s", task.ID)
	}

	// Build command to run Warp agent from the mounted volume
	// This matches the Namespace pattern: /bin/sh /agent/entrypoint.sh agent run ...
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

	// Allow task source types to augment args
	cmd = common.AugmentArgsForTask(task, cmd)

	log.Infof(ctx, "Creating Docker container with image=%s", imageName)

	// Create and start Docker container
	containerConfig := &container.Config{
		Image:      imageName,
		Cmd:        cmd,
		Env:        envVars,
		WorkingDir: "/workspace",
	}

	hostConfig := &container.HostConfig{
		AutoRemove: true,
		Binds: []string{
			fmt.Sprintf("%s:/agent:ro", volumeName), // Mount agent volume read-only at /agent
		},
	}

	resp, err := dockerClient.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	containerID := resp.ID
	log.Infof(ctx, "Created Docker container: %s", containerID)

	// Ensure cleanup on errors
	defer func() {
		if containerID != "" {
			// Try to remove container if it still exists (in case AutoRemove didn't work)
			if removeErr := dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); removeErr != nil {
				log.Debugf(ctx, "Container %s already removed or removal failed: %v", containerID, removeErr)
			}
		}
	}()

	// Start the container
	if err := dockerClient.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	log.Infof(ctx, "Started Docker container: %s", containerID)

	// Wait for container to complete
	statusCh, errCh := dockerClient.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("error waiting for container: %w", err)
		}
	case status := <-statusCh:
		log.Infof(ctx, "Container exited with status code: %d", status.StatusCode)

		// Collect and log container output
		logOutput, logErr := w.getContainerLogs(ctx, dockerClient, containerID)
		if logErr != nil {
			log.Warnf(ctx, "Failed to get container logs: %v", logErr)
		} else if logOutput != "" {
			log.Infof(ctx, "Container output:\n%s", logOutput)
		}

		if status.StatusCode != 0 {
			errorMsg := fmt.Sprintf("container exited with non-zero status: %d", status.StatusCode)
			if logOutput != "" {
				// Include last part of logs in error message
				lines := strings.Split(logOutput, "\n")
				if len(lines) > 10 {
					lines = lines[len(lines)-10:]
				}
				errorMsg = fmt.Sprintf("%s. Last output:\n%s", errorMsg, strings.Join(lines, "\n"))
			}
			return fmt.Errorf("%s", errorMsg)
		}
	}

	log.Infof(ctx, "Task execution completed successfully")
	return nil
}

// getContainerLogs retrieves and returns the container logs as a string
func (w *Worker) getContainerLogs(ctx context.Context, dockerClient *client.Client, containerID string) (string, error) {
	out, err := dockerClient.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: false,
	})
	if err != nil {
		return "", err
	}
	defer out.Close()

	// Read all logs
	logBytes, err := io.ReadAll(out)
	if err != nil {
		return "", err
	}

	return string(logBytes), nil
}

// copySidecarToVolume copies the sidecar image filesystem to a volume
// This emulates Namespace's sidecar volume feature using docker export and tar
func (w *Worker) copySidecarToVolume(ctx context.Context, dockerClient *client.Client, sidecarImage, volumeName string) error {
	// Step 1: Create a container from the sidecar image (don't start it)
	log.Infof(ctx, "Creating temporary container from sidecar image")
	sidecarConfig := &container.Config{
		Image: sidecarImage,
		Cmd:   []string{"true"}, // No-op command
	}

	sidecarResp, err := dockerClient.ContainerCreate(ctx, sidecarConfig, nil, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create sidecar container: %w", err)
	}

	sidecarContainerID := sidecarResp.ID
	defer func() {
		// Clean up sidecar container
		if removeErr := dockerClient.ContainerRemove(ctx, sidecarContainerID, container.RemoveOptions{Force: true}); removeErr != nil {
			log.Warnf(ctx, "Failed to remove sidecar container %s: %v", sidecarContainerID, removeErr)
		}
	}()

	log.Infof(ctx, "Created sidecar container: %s", sidecarContainerID)

	// Step 2: Export the container filesystem as a tar archive
	log.Infof(ctx, "Exporting sidecar filesystem")
	tarReader, err := dockerClient.ContainerExport(ctx, sidecarContainerID)
	if err != nil {
		return fmt.Errorf("failed to export sidecar container: %w", err)
	}
	defer tarReader.Close()

	// Step 3: Extract the tar into the volume using an alpine container
	// This is equivalent to: docker run --rm -v volumeName:/target alpine tar -x -C /target
	log.Infof(ctx, "Extracting sidecar filesystem to volume")

	// Pull alpine if not present (for tar extraction)
	alpineImage := "alpine:latest"
	alpineReader, err := dockerClient.ImagePull(ctx, alpineImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull alpine image: %w", err)
	}
	io.Copy(io.Discard, alpineReader)
	alpineReader.Close()

	// Create extraction container with volume mounted
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
		// Clean up extraction container
		if removeErr := dockerClient.ContainerRemove(ctx, extractContainerID, container.RemoveOptions{Force: true}); removeErr != nil {
			log.Warnf(ctx, "Failed to remove extraction container %s: %v", extractContainerID, removeErr)
		}
	}()

	log.Infof(ctx, "Created extraction container: %s", extractContainerID)

	// Attach to the container to pipe the tar data
	attachResp, err := dockerClient.ContainerAttach(ctx, extractContainerID, container.AttachOptions{
		Stdin:  true,
		Stream: true,
	})
	if err != nil {
		return fmt.Errorf("failed to attach to extraction container: %w", err)
	}
	defer attachResp.Close()

	// Start the container
	if err := dockerClient.ContainerStart(ctx, extractContainerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start extraction container: %w", err)
	}

	// Pipe the tar data to the extraction container's stdin
	go func() {
		defer attachResp.CloseWrite()
		io.Copy(attachResp.Conn, tarReader)
	}()

	// Wait for extraction to complete
	statusCh, errCh := dockerClient.ContainerWait(ctx, extractContainerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("error waiting for extraction container: %w", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			// Get logs to see what went wrong
			logOutput, _ := w.getContainerLogs(ctx, dockerClient, extractContainerID)
			return fmt.Errorf("extraction container exited with status %d. Logs: %s", status.StatusCode, logOutput)
		}
		log.Infof(ctx, "Successfully extracted sidecar filesystem to volume %s", volumeName)
	}

	return nil
}

// sendTaskClaimed sends a task claimed message to the server
func (w *Worker) sendTaskClaimed(taskID string) error {
	claimed := TaskClaimedMessage{
		TaskID:   taskID,
		WorkerID: w.config.WorkerID,
	}

	data, err := json.Marshal(claimed)
	if err != nil {
		return fmt.Errorf("failed to marshal task claimed message: %w", err)
	}

	msg := WebSocketMessage{
		Type: MessageTypeTaskClaimed,
		Data: data,
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal websocket message: %w", err)
	}

	return w.sendMessage(msgBytes)
}

// sendTaskFailed sends a task failed message to the server
func (w *Worker) sendTaskFailed(taskID, message string) error {
	failedMsg := TaskFailedMessage{
		TaskID:  taskID,
		Message: message,
	}

	data, err := json.Marshal(failedMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal task failed message: %w", err)
	}

	msg := WebSocketMessage{
		Type: MessageTypeTaskFailed,
		Data: data,
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal websocket message: %w", err)
	}

	return w.sendMessage(msgBytes)
}

// sendMessage sends a message to the server with timeout
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

// sanitizeImageNameForVolume converts a Docker image name to a valid volume name
// Docker volume names can contain alphanumeric characters, periods, underscores, and hyphens
func sanitizeImageNameForVolume(imageName string) string {
	// Replace disallowed characters with hyphens
	// Common image patterns:
	//   - docker.io/library/alpine:3.18 -> docker-io-library-alpine-3-18
	//   - gcr.io/project/image:tag -> gcr-io-project-image-tag
	//   - myregistry:5000/image:latest -> myregistry-5000-image-latest
	sanitized := strings.ReplaceAll(imageName, "/", "-")
	sanitized = strings.ReplaceAll(sanitized, ":", "-")
	return sanitized
}

func (w *Worker) Shutdown() {
	log.Infof(w.ctx, "Shutting down worker...")

	// Cancel all active tasks
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

	// Give tasks a moment to clean up
	if activeTaskCount > 0 {
		time.Sleep(500 * time.Millisecond)
	}

	// Cancel context
	w.cancel()

	// Close Docker client
	if w.dockerClient != nil {
		w.dockerClient.Close()
	}

	// Close connection
	w.connMutex.Lock()
	if w.conn != nil {
		// Send close message
		w.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		w.conn.Close()
		w.conn = nil
	}
	w.connMutex.Unlock()

	log.Infof(w.ctx, "Worker shutdown complete")
}
