package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/distribution/reference"
	cliconfig "github.com/docker/cli/cli/config"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/registry"
	"github.com/rs/zerolog"
	"github.com/warpdotdev/oz-agent-worker/internal/common"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// DockerBackendConfig holds configuration specific to the Docker backend.
type DockerBackendConfig struct {
	ServerRootURL string
	NoCleanup     bool
	Volumes       []string
	Env           map[string]string
}

// DockerBackend executes tasks in Docker containers.
type DockerBackend struct {
	config       DockerBackendConfig
	dockerClient *client.Client
	platform     string // Docker daemon platform (e.g., "linux/amd64" or "linux/arm64")
}

// NewDockerBackend creates a new Docker backend, connecting to the Docker daemon.
func NewDockerBackend(ctx context.Context, config DockerBackendConfig) (*DockerBackend, error) {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pingCancel()

	// Ping the Docker daemon to ensure it's reachable, as we depend on this.
	if _, err := dockerClient.Ping(pingCtx); err != nil {
		if closeErr := dockerClient.Close(); closeErr != nil {
			log.Warnf(ctx, "Failed to close Docker client: %v", closeErr)
		}
		return nil, fmt.Errorf("failed to reach Docker daemon: %w", err)
	}

	// Get the Docker daemon version to determine its platform.
	versionInfo, err := dockerClient.ServerVersion(ctx)
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
	}, nil
}

// ExecuteTask runs the agent in a Docker container.
func (b *DockerBackend) ExecuteTask(ctx context.Context, assignment *types.TaskAssignmentMessage) error {
	task := assignment.Task
	dockerClient := b.dockerClient

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

	authStr := b.getRegistryAuth(ctx, imageName)
	if err := b.pullImage(ctx, imageName, authStr); err != nil {
		return err
	}

	if assignment.SidecarImage == "" {
		return fmt.Errorf("no sidecar image specified in assignment")
	}

	// Sidecar images are public, so no auth is needed
	if err := b.pullImage(ctx, assignment.SidecarImage, ""); err != nil {
		return err
	}

	// Get the concrete image digest to ensure volume is rebuilt when the image changes
	sidecarDigest, err := b.getImageDigest(ctx, assignment.SidecarImage)
	if err != nil {
		return fmt.Errorf("failed to get sidecar image digest: %w", err)
	}

	volumeName := sanitizeVolumeName(assignment.SidecarImage, sidecarDigest)
	log.Debugf(ctx, "Using shared volume: %s", volumeName)

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

		if err := b.copySidecarFilesystemToVolume(ctx, dockerClient, assignment.SidecarImage, volumeName); err != nil {
			return fmt.Errorf("failed to copy sidecar to volume: %w", err)
		}
	}

	// Prepare additional sidecar volumes (e.g., xvfb for computer use).
	additionalSidecarBinds, err := b.prepareAdditionalSidecars(ctx, dockerClient, assignment.AdditionalSidecars)
	if err != nil {
		return err
	}

	envVars := []string{
		fmt.Sprintf("TASK_ID=%s", task.ID),
		"GIT_TERMINAL_PROMPT=0",
		"GH_PROMPT_DISABLED=1",
	}

	for key, value := range assignment.EnvVars {
		envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
	}

	// Append user-specified CLI env vars last so they take precedence.
	for key, value := range b.config.Env {
		envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
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
		"--sandboxed",
		"--server-root-url",
		b.config.ServerRootURL,
	}

	cmd = common.AugmentArgsForTask(task, cmd)

	log.Debugf(ctx, "Creating Docker container with image=%s", imageName)

	containerConfig := &container.Config{
		Image:      imageName,
		Cmd:        cmd,
		Env:        envVars,
		WorkingDir: "/workspace",
	}

	binds := []string{
		fmt.Sprintf("%s:/agent:ro", volumeName),
	}
	// Add additional sidecar volumes.
	binds = append(binds, additionalSidecarBinds...)
	// Add user-configured volumes.
	binds = append(binds, b.config.Volumes...)

	hostConfig := &container.HostConfig{
		Binds: binds,
	}

	resp, err := dockerClient.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	containerID := resp.ID
	log.Debugf(ctx, "Created Docker container: %s", containerID)

	defer func() {
		if containerID != "" && !b.config.NoCleanup {
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
			return fmt.Errorf("container exited with non-zero status: %d", status.StatusCode)
		}
	}

	log.Infof(ctx, "Task %s execution completed successfully", task.ID)
	return nil
}

// Shutdown closes the Docker client.
func (b *DockerBackend) Shutdown(ctx context.Context) {
	if b.dockerClient != nil {
		if err := b.dockerClient.Close(); err != nil {
			log.Warnf(ctx, "Failed to close Docker client: %v", err)
		}
	}
}

// pullImage pulls a Docker image. If authStr is non-empty, it will be used for registry authentication.
// Docker only downloads changed layers, so this is efficient even if the image exists locally.
func (b *DockerBackend) pullImage(ctx context.Context, imageName string, authStr string) error {
	log.Infof(ctx, "Pulling image: %s", imageName)
	pullOptions := image.PullOptions{
		Platform:     b.platform,
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
	// architecture then what is specified in image.PullOptions.Platform
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

	// Get the registry hostname (e.g., "docker.io", "gcr.io").
	repoInfo, err := registry.ParseRepositoryInfo(ref)
	if err != nil {
		log.Warnf(ctx, "Failed to parse repository info: %v", err)
		return ""
	}

	authKey := registry.GetAuthConfigKey(repoInfo.Index)

	authConfig, err := cfg.GetAuthConfig(authKey)
	if err != nil {
		log.Warnf(ctx, "Failed to get auth config for registry %s: %v", authKey, err)
		return ""
	}
	if authConfig.Username == "" {
		return ""
	}

	authJSON, _ := json.Marshal(authConfig)
	log.Debugf(ctx, "Using Docker credentials for registry %s (username: %s)", authKey, authConfig.Username)
	return base64.URLEncoding.EncodeToString(authJSON)
}

func (b *DockerBackend) getContainerLogs(ctx context.Context, dockerClient *client.Client, containerID string) (string, error) {
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
func (b *DockerBackend) copySidecarFilesystemToVolume(ctx context.Context, dockerClient *client.Client, sidecarImage, volumeName string) error {
	log.Infof(ctx, "Creating temporary container from sidecar image")
	sidecarConfig := &container.Config{
		Image: sidecarImage,
		Cmd:   []string{"true"},
	}

	sidecarHostConfig := &container.HostConfig{
		AutoRemove: true,
	}

	sidecarResp, err := dockerClient.ContainerCreate(ctx, sidecarConfig, sidecarHostConfig, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create sidecar container: %w", err)
	}

	sidecarContainerID := sidecarResp.ID

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

	extractResp, err := dockerClient.ContainerCreate(ctx, extractConfig, extractHostConfig, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create extraction container: %w", err)
	}

	extractContainerID := extractResp.ID

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
			logOutput, _ := b.getContainerLogs(ctx, dockerClient, extractContainerID)
			return fmt.Errorf("extraction container exited with status %d. Logs: %s", status.StatusCode, logOutput)
		}
		log.Infof(ctx, "Successfully extracted sidecar filesystem to volume %s", volumeName)
	}

	return nil
}

// prepareAdditionalSidecars pulls each additional sidecar image, creates a Docker volume
// from its filesystem, and returns the list of bind mount strings to add to the container.
func (b *DockerBackend) prepareAdditionalSidecars(ctx context.Context, dockerClient *client.Client, sidecars []types.SidecarMount) ([]string, error) {
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

		_, err = dockerClient.VolumeInspect(ctx, volumeName)
		if err == nil {
			log.Debugf(ctx, "Reusing existing volume %s for additional sidecar", volumeName)
		} else {
			log.Infof(ctx, "Creating new Docker volume: %s", volumeName)
			if _, err := dockerClient.VolumeCreate(ctx, volume.CreateOptions{Name: volumeName}); err != nil {
				return nil, fmt.Errorf("failed to create volume for additional sidecar %s: %w", sidecar.Image, err)
			}

			if err := b.copySidecarFilesystemToVolume(ctx, dockerClient, sidecar.Image, volumeName); err != nil {
				// Clean up the empty volume so it isn't silently reused on retry.
				if removeErr := dockerClient.VolumeRemove(ctx, volumeName, false); removeErr != nil {
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

	// Sanitize the repository name for use in volume name
	baseName := strings.ReplaceAll(repoName, "/", "-")

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
