package worker

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/distribution/reference"
	cliconfig "github.com/docker/cli/cli/config"
	"github.com/moby/moby/api/pkg/authconfig"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/rs/zerolog"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

const dockerHubAuthConfigKey = "https://index.docker.io/v1/"

// DockerBackendConfig holds configuration specific to the Docker backend.
type DockerBackendConfig struct {
	NoCleanup bool
	Volumes   []string
	Env       map[string]string
}

func (b *DockerBackend) containerWasOOMKilled(ctx context.Context, dockerClient *client.Client, containerID string) bool {
	inspect, err := dockerClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil || inspect.Container.State == nil {
		return false
	}
	return inspect.Container.State.OOMKilled
}

// DockerBackend executes tasks in Docker containers.
type DockerBackend struct {
	config       DockerBackendConfig
	dockerClient *client.Client
	platform     string // Docker daemon platform (e.g., "linux/amd64" or "linux/arm64")
	platformSpec ocispec.Platform
}

// NewDockerBackend creates a new Docker backend, connecting to the Docker daemon.
func NewDockerBackend(ctx context.Context, config DockerBackendConfig) (*DockerBackend, error) {
	dockerClient, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pingCancel()

	// Ping the Docker daemon to ensure it's reachable, as we depend on this.
	if _, err := dockerClient.Ping(pingCtx, client.PingOptions{}); err != nil {
		if closeErr := dockerClient.Close(); closeErr != nil {
			log.Warnf(ctx, "Failed to close Docker client: %v", closeErr)
		}
		return nil, fmt.Errorf("failed to reach Docker daemon: %w", err)
	}

	// Get the Docker daemon version to determine its platform.
	versionInfo, err := dockerClient.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		if closeErr := dockerClient.Close(); closeErr != nil {
			log.Warnf(ctx, "Failed to close Docker client: %v", closeErr)
		}
		return nil, fmt.Errorf("failed to get Docker version: %w", err)
	}

	// Determine the platform. The sidecar only supports linux/amd64 and linux/arm64,
	// so we enforce that all images are pulled for one of these platforms.
	platform := fmt.Sprintf("%s/%s", versionInfo.Os, versionInfo.Arch)
	if platform != "linux/amd64" && platform != "linux/arm64" {
		if closeErr := dockerClient.Close(); closeErr != nil {
			log.Warnf(ctx, "Failed to close Docker client: %v", closeErr)
		}
		return nil, fmt.Errorf("unsupported Docker platform %s (only linux/amd64 and linux/arm64 are supported)", platform)
	}

	log.Debugf(ctx, "Docker daemon is reachable, platform: %s", platform)

	return &DockerBackend{
		config:       config,
		dockerClient: dockerClient,
		platform:     platform,
		platformSpec: ocispec.Platform{
			OS:           versionInfo.Os,
			Architecture: versionInfo.Arch,
		},
	}, nil
}

// ExecuteTask runs the agent in a Docker container.
func (b *DockerBackend) ExecuteTask(ctx context.Context, params *TaskParams) error {
	dockerClient := b.dockerClient
	imageName := params.DockerImage

	log.Debugf(ctx, "Using Docker image: %s", imageName)

	authStr := b.getRegistryAuth(ctx, imageName)
	if err := b.pullImage(ctx, imageName, authStr); err != nil {
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonImagePull, err)
	}

	// Prepare all sidecar volumes (Warp agent sidecar + any additional sidecars).
	sidecarBinds, err := b.prepareSidecars(ctx, dockerClient, params.Sidecars)
	if err != nil {
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonSidecarPrep, err)
	}

	// Start with common env vars, then append backend-specific config env vars.
	envVars := make([]string, len(params.EnvVars))
	copy(envVars, params.EnvVars)
	for key, value := range b.config.Env {
		envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
	}

	// Build Docker-specific command: entrypoint prefix + base args.
	cmd := append([]string{"/agent/entrypoint.sh"}, params.BaseArgs...)

	log.Debugf(ctx, "Creating Docker container with image=%s", imageName)

	containerConfig := &container.Config{
		Image:      imageName,
		Cmd:        cmd,
		Env:        envVars,
		WorkingDir: "/workspace",
	}

	// Sidecar binds come first, then user-configured volumes.
	binds := sidecarBinds
	binds = append(binds, b.config.Volumes...)

	hostConfig := &container.HostConfig{
		Binds:     binds,
		Resources: dockerResourcesForShape(params.InstanceShape),
	}

	resp, err := dockerClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     containerConfig,
		HostConfig: hostConfig,
	})
	if err != nil {
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonContainerCreate, fmt.Errorf("failed to create container: %w", err))
	}

	containerID := resp.ID
	log.Debugf(ctx, "Created Docker container: %s", containerID)

	defer func() {
		if containerID != "" && !b.config.NoCleanup {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), BackendShutdownTimeout)
			defer cleanupCancel()
			if _, removeErr := dockerClient.ContainerRemove(cleanupCtx, containerID, client.ContainerRemoveOptions{Force: true}); removeErr != nil {
				log.Debugf(ctx, "Container %s already removed or removal failed: %v", containerID, removeErr)
			}
		}
	}()

	if _, err := dockerClient.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonContainerStart, fmt.Errorf("failed to start container: %w", err))
	}

	log.Debugf(ctx, "Started Docker container: %s", containerID)

	waitResult := dockerClient.ContainerWait(ctx, containerID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-waitResult.Error:
		if err != nil {
			return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonContainerWait, fmt.Errorf("error waiting for container: %w", err))
		}
	case status := <-waitResult.Result:
		log.Debugf(ctx, "Container exited with status code: %d", status.StatusCode)

		logOutput, logErr := b.getContainerLogs(ctx, dockerClient, containerID)
		if zerolog.GlobalLevel() <= zerolog.DebugLevel || status.StatusCode != 0 {
			if logErr != nil {
				log.Warnf(ctx, "Failed to get container logs: %v", logErr)
			} else if logOutput != "" {
				if status.StatusCode != 0 {
					log.Infof(ctx, "Container output:\n%s", logOutput)
				} else {
					log.Debugf(ctx, "Container output:\n%s", logOutput)
				}
			}
		}

		if status.StatusCode != 0 {
			metricsReason := metrics.TaskFailureReasonContainerExit
			oomKilled := b.containerWasOOMKilled(ctx, dockerClient, containerID)
			if oomKilled {
				metricsReason = metrics.TaskFailureReasonContainerOOM
			}
			exitCode := int(status.StatusCode)
			var wireCause types.TaskFailureCause
			if oomKilled {
				wireCause = types.TaskFailureCauseOOM
			} else if _, ok := signalFromExitCode(exitCode); ok {
				wireCause = types.TaskFailureCauseRuntimeCrash
			} else {
				wireCause = types.TaskFailureCauseBackendFailure
			}
			return newBackendFailureWithCause(metrics.TaskFailurePhaseBackend, metricsReason, fmt.Errorf("container exited with non-zero status: %d", status.StatusCode), wireCause)
		}
	}

	log.Infof(ctx, "Task %s execution completed successfully", params.TaskID)
	return nil
}

// dockerResourcesForShape maps an instance shape to Docker container resource limits.
// Each axis is applied only when positive; a nil shape (or non-positive axes) yields no
// limits, so the container runs unconstrained as it does without a runner shape. Memory is
// a hard cap: MemorySwap is pinned to Memory so the container cannot exceed memory_gb via
// swap, matching the Kubernetes backend's memory limit and the requested SKU size regardless
// of host swap configuration.
func dockerResourcesForShape(shape *types.InstanceShape) container.Resources {
	var res container.Resources
	if shape == nil {
		return res
	}
	if shape.Vcpus > 0 {
		res.NanoCPUs = int64(shape.Vcpus) * 1_000_000_000
	}
	if shape.MemoryGb > 0 {
		memoryBytes := int64(shape.MemoryGb) << 30
		res.Memory = memoryBytes
		res.MemorySwap = memoryBytes
	}
	return res
}

// Shutdown closes the Docker client.
func (b *DockerBackend) Shutdown(ctx context.Context) {
	if b.dockerClient != nil {
		if err := b.dockerClient.Close(); err != nil {
			log.Warnf(ctx, "Failed to close Docker client: %v", err)
		}
	}
}

func (b *DockerBackend) PreservesTasksOnShutdown() bool {
	return false
}

// pullImage pulls a Docker image. If authStr is non-empty, it will be used for registry authentication.
// Docker only downloads changed layers, so this is efficient even if the image exists locally.
func (b *DockerBackend) pullImage(ctx context.Context, imageName string, authStr string) error {
	log.Infof(ctx, "Pulling image: %s", imageName)
	pullOptions := client.ImagePullOptions{
		Platforms:    []ocispec.Platform{b.platformSpec},
		RegistryAuth: authStr,
	}
	reader, err := b.dockerClient.ImagePull(ctx, imageName, pullOptions)
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %w", imageName, err)
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			log.Warnf(ctx, "Failed to close image pull reader: %v", closeErr)
		}
	}()

	// The image pull doesn't actually happen until you read from this stream, but we don't need the output.
	if _, err = io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("failed to read image pull output: %w", err)
	}

	// Verify the pulled image matches the host platform. Docker may pull an image for a different
	// architecture than what is specified in client.ImagePullOptions.Platforms
	// See: https://github.com/moby/moby/pull/42325
	inspect, err := b.dockerClient.ImageInspect(ctx, imageName)
	if err != nil {
		return fmt.Errorf("failed to inspect pulled image %s: %w", imageName, err)
	}
	imagePlatform := fmt.Sprintf("%s/%s", inspect.Os, inspect.Architecture)
	if imagePlatform != b.platform {
		return fmt.Errorf(
			"image %s is for platform %s, but this worker requires %s",
			imageName, imagePlatform, b.platform,
		)
	}

	log.Infof(ctx, "Successfully pulled image: %s", imageName)
	return nil
}

// getRegistryAuth returns the auth string for the registry of the given image, or empty string if not found.
func (b *DockerBackend) getRegistryAuth(ctx context.Context, imageName string) string {
	cfg, err := cliconfig.Load("")
	if err != nil {
		log.Warnf(ctx, "Failed to load Docker config: %v. Attempting pull without auth.", err)
		return ""
	}
	if cfg == nil {
		return ""
	}

	ref, err := reference.ParseNormalizedNamed(imageName)
	if err != nil {
		log.Warnf(ctx, "Failed to parse image name %s: %v", imageName, err)
		return ""
	}

	authKey := getAuthConfigKey(reference.Domain(ref))

	authConfig, err := cfg.GetAuthConfig(authKey)
	if err != nil {
		log.Warnf(ctx, "Failed to get auth config for registry %s: %v", authKey, err)
		return ""
	}
	if authConfig.Username == "" {
		return ""
	}

	authStr, err := authconfig.Encode(registry.AuthConfig{
		Username:      authConfig.Username,
		Password:      authConfig.Password,
		ServerAddress: authConfig.ServerAddress,
		Auth:          authConfig.Auth,
		IdentityToken: authConfig.IdentityToken,
		RegistryToken: authConfig.RegistryToken,
	}) // #nosec G117 -- Docker RegistryAuth requires marshaling credentials before base64 encoding; the value is not logged.
	if err != nil {
		log.Warnf(ctx, "Failed to encode auth config for registry %s: %v", authKey, err)
		return ""
	}
	log.Debugf(ctx, "Using Docker credentials for registry %s (username: %s)", authKey, authConfig.Username)
	return authStr
}

func (b *DockerBackend) getContainerLogs(ctx context.Context, dockerClient *client.Client, containerID string) (string, error) {
	out, err := dockerClient.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
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
func (b *DockerBackend) copySidecarFilesystemToVolume(ctx context.Context, dockerClient *client.Client, sidecarImage, volumeName string) error {
	log.Infof(ctx, "Creating temporary container from sidecar image")
	sidecarConfig := &container.Config{
		Image: sidecarImage,
		Cmd:   []string{"true"},
	}

	sidecarHostConfig := &container.HostConfig{
		AutoRemove: true,
	}

	sidecarResp, err := dockerClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     sidecarConfig,
		HostConfig: sidecarHostConfig,
	})
	if err != nil {
		return fmt.Errorf("failed to create sidecar container: %w", err)
	}

	sidecarContainerID := sidecarResp.ID

	log.Infof(ctx, "Created sidecar container: %s", sidecarContainerID)

	// Export the full filesystem of the sidecar.
	tarReader, err := dockerClient.ContainerExport(ctx, sidecarContainerID, client.ContainerExportOptions{})
	if err != nil {
		return fmt.Errorf("failed to export sidecar container: %w", err)
	}
	defer func() {
		if err := tarReader.Close(); err != nil {
			log.Warnf(ctx, "Failed to close tar reader: %v", err)
		}
	}()

	log.Infof(ctx, "Extracting sidecar filesystem to volume")

	// Use the sidecar image itself to extract the exported filesystem onto the volume.
	// Override the entrypoint to ensure we only run tar, not the sidecar's default command.
	// Run as root to ensure we have permissions to write to the volume.
	extractConfig := &container.Config{
		Image:        sidecarImage,
		User:         "root",
		Entrypoint:   []string{"/bin/sh", "-c"},
		Cmd:          []string{"tar -x -C /target"},
		StdinOnce:    true,
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}

	extractHostConfig := &container.HostConfig{
		AutoRemove: true,
		Binds: []string{
			fmt.Sprintf("%s:/target", volumeName),
		},
	}

	extractResp, err := dockerClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     extractConfig,
		HostConfig: extractHostConfig,
	})
	if err != nil {
		return fmt.Errorf("failed to create extraction container: %w", err)
	}

	extractContainerID := extractResp.ID

	log.Infof(ctx, "Created extraction container: %s", extractContainerID)

	attachResp, err := dockerClient.ContainerAttach(ctx, extractContainerID, client.ContainerAttachOptions{
		Stdin:  true,
		Stream: true,
	})
	if err != nil {
		return fmt.Errorf("failed to attach to extraction container: %w", err)
	}
	defer attachResp.Close()

	if _, err := dockerClient.ContainerStart(ctx, extractContainerID, client.ContainerStartOptions{}); err != nil {
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

	waitResult := dockerClient.ContainerWait(ctx, extractContainerID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-waitResult.Error:
		if err != nil {
			return fmt.Errorf("error waiting for extraction container: %w", err)
		}
	case status := <-waitResult.Result:
		if status.StatusCode != 0 {
			logOutput, _ := b.getContainerLogs(ctx, dockerClient, extractContainerID)
			return fmt.Errorf("extraction container exited with status %d. Logs: %s", status.StatusCode, logOutput)
		}
		log.Infof(ctx, "Successfully extracted sidecar filesystem to volume %s", volumeName)
	}

	return nil
}

// prepareSidecars pulls each sidecar image, creates a Docker volume from its filesystem,
// and returns the list of bind mount strings to add to the container.
func (b *DockerBackend) prepareSidecars(ctx context.Context, dockerClient *client.Client, sidecars []types.SidecarMount) ([]string, error) {
	var binds []string
	seenMountPaths := make(map[string]bool)

	for _, sidecar := range sidecars {
		if sidecar.Image == "" {
			return nil, fmt.Errorf("additional sidecar has empty image")
		}
		if sidecar.MountPath == "" {
			return nil, fmt.Errorf("additional sidecar %s has empty mount path", sidecar.Image)
		}
		if seenMountPaths[sidecar.MountPath] {
			return nil, fmt.Errorf("duplicate mount path %s for additional sidecar %s", sidecar.MountPath, sidecar.Image)
		}
		seenMountPaths[sidecar.MountPath] = true

		log.Infof(ctx, "Preparing additional sidecar: image=%s, mount=%s", sidecar.Image, sidecar.MountPath)

		// Additional sidecar images are public, so no auth is needed.
		if err := b.pullImage(ctx, sidecar.Image, ""); err != nil {
			return nil, fmt.Errorf("failed to pull additional sidecar image %s: %w", sidecar.Image, err)
		}

		digest, err := b.getImageDigest(ctx, sidecar.Image)
		if err != nil {
			return nil, fmt.Errorf("failed to get digest for additional sidecar image %s: %w", sidecar.Image, err)
		}

		volumeName := sanitizeVolumeName(sidecar.Image, digest)
		log.Debugf(ctx, "Using volume %s for additional sidecar %s", volumeName, sidecar.Image)

		_, err = dockerClient.VolumeInspect(ctx, volumeName, client.VolumeInspectOptions{})
		if err == nil {
			log.Debugf(ctx, "Reusing existing volume %s for additional sidecar", volumeName)
		} else {
			log.Infof(ctx, "Creating new Docker volume: %s", volumeName)
			if _, err := dockerClient.VolumeCreate(ctx, client.VolumeCreateOptions{Name: volumeName}); err != nil {
				return nil, fmt.Errorf("failed to create volume for additional sidecar %s: %w", sidecar.Image, err)
			}

			if err := b.copySidecarFilesystemToVolume(ctx, dockerClient, sidecar.Image, volumeName); err != nil {
				// Clean up the empty volume so it isn't silently reused on retry.
				if _, removeErr := dockerClient.VolumeRemove(ctx, volumeName, client.VolumeRemoveOptions{}); removeErr != nil {
					log.Warnf(ctx, "Failed to clean up volume %s after copy failure: %v", volumeName, removeErr)
				}
				return nil, fmt.Errorf("failed to copy additional sidecar %s to volume: %w", sidecar.Image, err)
			}
		}

		mode := ":ro"
		if sidecar.ReadWrite {
			// Docker defaults to read-write when no mode suffix is provided.
			mode = ""
		}
		binds = append(binds, fmt.Sprintf("%s:%s%s", volumeName, sidecar.MountPath, mode))
	}
	return binds, nil
}

// sanitizeVolumeName creates a volume name from the image name and digest.
// The digest ensures uniqueness when the image tag points to different content.
func sanitizeVolumeName(imageName, digest string) string {
	var repoName string
	ref, err := reference.ParseNormalizedNamed(imageName)
	if err == nil {
		// Use FamiliarName with TrimNamed to get the repository without tag/digest
		// e.g., "namespace/warp-agent:latest" -> "namespace/warp-agent"
		repoName = reference.FamiliarName(reference.TrimNamed(ref))
	} else {
		// Fallback to original image name if parsing fails
		repoName = imageName
	}

	// Local-registry image refs can contain ':' and '/', but Docker volume names
	// only allow [a-zA-Z0-9_.-], so map every other character to '-'.
	baseName := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			return r
		}
		return '-'
	}, repoName)

	// digest format is typically "sha256:abc123..."
	parts := strings.Split(digest, ":")
	if len(parts) == 2 {
		// Use first 12 chars of the hash
		hash := parts[1]
		if len(hash) > 12 {
			hash = hash[:12]
		}
		return baseName + "-" + hash
	}
	// Fallback if digest format is unexpected
	return baseName + "-" + strings.ReplaceAll(digest, ":", "-")
}

// getImageDigest returns the digest (sha256 hash) of a pulled image.
func (b *DockerBackend) getImageDigest(ctx context.Context, imageName string) (string, error) {
	inspect, err := b.dockerClient.ImageInspect(ctx, imageName)
	if err != nil {
		return "", fmt.Errorf("failed to inspect image %s: %w", imageName, err)
	}

	// RepoDigests contains the digest from the registry. It's in the format "repo@sha256:hash"
	if len(inspect.RepoDigests) > 0 {
		// Extract just the digest part (sha256:hash)
		parts := strings.Split(inspect.RepoDigests[0], "@")
		if len(parts) == 2 {
			return parts[1], nil
		}
	}

	// Fallback to the image ID if RepoDigests is not available (this can happen for locally built images)
	if inspect.ID != "" {
		return inspect.ID, nil
	}

	return "", fmt.Errorf("no digest found for image %s", imageName)
}

// getAuthConfigKey special-cases Docker Hub's credential key and returns the registry hostname for private registries.
func getAuthConfigKey(domainName string) string {
	if domainName == "docker.io" || domainName == "index.docker.io" {
		return dockerHubAuthConfigKey
	}
	return domainName
}
