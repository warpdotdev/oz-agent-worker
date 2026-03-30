package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
)

const defaultWorkspaceRoot = "/var/lib/oz/workspaces"

// defaultInheritedEnvVars are the host environment variables passed through to
// tasks and scripts by default. Sensitive worker credentials are intentionally
// excluded; additional variables can be opted in via the backend config.
// Proxy variables are included so the oz CLI can route traffic through HTTP
// proxies configured on the host (e.g. egress proxies in restricted networks).
var defaultInheritedEnvVars = []string{
	"HOME", "TMPDIR", "PATH",
	// HTTP proxy configuration (both cases for portability).
	"HTTPS_PROXY", "https_proxy",
	"HTTP_PROXY", "http_proxy",
	"ALL_PROXY", "all_proxy",
	"NO_PROXY", "no_proxy",
}

// hostBaseEnv builds a minimal env slice from the host, containing only the
// keys listed in defaultInheritedEnvVars.
func hostBaseEnv() []string {
	var base []string
	for _, key := range defaultInheritedEnvVars {
		if val, ok := os.LookupEnv(key); ok {
			base = append(base, fmt.Sprintf("%s=%s", key, val))
		}
	}
	return base
}

// DirectBackendConfig holds configuration specific to the direct (non-containerized) backend.
type DirectBackendConfig struct {
	WorkspaceRoot   string
	TargetDir       string // If set, run all tasks in this directory instead of creating per-task workspaces.
	OzPath          string // Path to the oz CLI binary. If empty, looks up "oz" in PATH.
	SetupCommand    string
	TeardownCommand string
	NoCleanup       bool
	Env             map[string]string
}

// DirectBackend executes tasks directly on the host without Docker.
type DirectBackend struct {
	config DirectBackendConfig
	ozPath string // resolved path to the oz CLI
}

// NewDirectBackend creates a new direct backend, verifying the oz CLI is available.
func NewDirectBackend(ctx context.Context, config DirectBackendConfig) (*DirectBackend, error) {
	ozPath := config.OzPath
	if ozPath == "" {
		var err error
		ozPath, err = exec.LookPath("oz")
		if err != nil {
			return nil, fmt.Errorf("oz CLI not found in PATH: %w", err)
		}
	}
	log.Infof(ctx, "Using oz CLI at: %s", ozPath)

	if config.TargetDir != "" {
		// Validate that the target directory exists.
		info, err := os.Stat(config.TargetDir)
		if err != nil {
			return nil, fmt.Errorf("target directory %s does not exist: %w", config.TargetDir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("target directory %s is not a directory", config.TargetDir)
		}
		log.Infof(ctx, "Using shared target directory: %s (per-task workspace isolation disabled)", config.TargetDir)
	} else {
		if config.WorkspaceRoot == "" {
			config.WorkspaceRoot = defaultWorkspaceRoot
		}

		// Ensure workspace root exists.
		if err := os.MkdirAll(config.WorkspaceRoot, 0755); err != nil {
			return nil, fmt.Errorf("failed to create workspace root %s: %w", config.WorkspaceRoot, err)
		}
	}

	return &DirectBackend{
		config: config,
		ozPath: ozPath,
	}, nil
}

// ExecuteTask runs the agent directly on the host.
func (b *DirectBackend) ExecuteTask(ctx context.Context, params *TaskParams) error {
	taskID := params.TaskID

	// Determine working directory: shared target dir or per-task workspace.
	var workspaceDir string
	usingTargetDir := b.config.TargetDir != ""

	if usingTargetDir {
		workspaceDir = b.config.TargetDir
	} else {
		// Create per-task workspace directory.
		workspaceDir = filepath.Join(b.config.WorkspaceRoot, taskID)
		if err := os.MkdirAll(workspaceDir, 0755); err != nil {
			return fmt.Errorf("failed to create workspace directory: %w", err)
		}
		log.Infof(ctx, "Created workspace: %s", workspaceDir)
	}

	defer func() {
		if usingTargetDir {
			// Don't clean up the shared target directory.
			b.runTeardownIfConfigured(ctx, taskID, workspaceDir)
			return
		}
		if b.config.NoCleanup {
			log.Infof(ctx, "Skipping cleanup for workspace: %s", workspaceDir)
			return
		}
		b.cleanup(ctx, taskID, workspaceDir)
	}()

	// 2. Create temp environment file for setup script to write to.
	envFile, err := os.CreateTemp(workspaceDir, "oz-env-*")
	if err != nil {
		return fmt.Errorf("failed to create environment file: %w", err)
	}
	envFilePath := envFile.Name()
	if err := envFile.Close(); err != nil {
		return fmt.Errorf("failed to close environment file: %w", err)
	}
	defer os.Remove(envFilePath)

	// 3. Build environment variables: common + config-level.
	envVars := make([]string, len(params.EnvVars))
	copy(envVars, params.EnvVars)
	for key, value := range b.config.Env {
		envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
	}

	// 4. Run setup command if configured.
	if b.config.SetupCommand != "" {
		setupEnv := append(envVars,
			fmt.Sprintf("OZ_WORKSPACE_ROOT=%s", workspaceDir),
			"OZ_WORKER_BACKEND=direct",
			fmt.Sprintf("OZ_RUN_ID=%s", taskID),
			fmt.Sprintf("OZ_ENVIRONMENT_FILE=%s", envFilePath),
		)

		log.Infof(ctx, "Running setup command: %s", b.config.SetupCommand)
		if err := b.runCommand(ctx, b.config.SetupCommand, workspaceDir, setupEnv); err != nil {
			return fmt.Errorf("setup command failed: %w", err)
		}
	}

	// 5. Parse environment file for KEY=VALUE pairs written by setup script.
	// Use merge semantics so setup script vars can override YAML config vars.
	setupScriptEnv, err := parseEnvFile(envFilePath)
	if err != nil {
		log.Warnf(ctx, "Failed to parse environment file: %v", err)
	}
	var setupScriptVars []string
	for key, value := range setupScriptEnv {
		setupScriptVars = append(setupScriptVars, fmt.Sprintf("%s=%s", key, value))
	}
	envVars = mergeEnvVars(envVars, setupScriptVars)

	// 6. Invoke oz CLI with base args.
	// Start from a minimal host base (HOME, TMPDIR, PATH) and overlay task env vars,
	// so sensitive worker credentials are never exposed to tasks.
	cmd := exec.CommandContext(ctx, b.ozPath, params.BaseArgs...)
	cmd.Dir = workspaceDir
	cmd.Env = mergeEnvVars(hostBaseEnv(), envVars)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	log.Infof(ctx, "Running oz agent in workspace %s", workspaceDir)
	log.Debugf(ctx, "Command: %s %s", b.ozPath, strings.Join(params.BaseArgs, " "))

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("oz agent exited with error: %w", err)
	}

	log.Infof(ctx, "Task %s execution completed successfully", taskID)
	return nil
}

// Shutdown cleans up any workspace directories left behind under the workspace root.
func (b *DirectBackend) Shutdown(ctx context.Context) {
	if b.config.WorkspaceRoot == "" {
		return
	}
	entries, err := os.ReadDir(b.config.WorkspaceRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf(ctx, "Failed to read workspace root %s during shutdown: %v", b.config.WorkspaceRoot, err)
		}
		return
	}
	for _, entry := range entries {
		path := filepath.Join(b.config.WorkspaceRoot, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			log.Warnf(ctx, "Failed to remove workspace %s during shutdown: %v", path, err)
		} else {
			log.Infof(ctx, "Removed lingering workspace on shutdown: %s", path)
		}
	}
}

// runTeardownIfConfigured runs the teardown command if one is configured.
func (b *DirectBackend) runTeardownIfConfigured(ctx context.Context, taskID, workspaceDir string) {
	if b.config.TeardownCommand == "" {
		return
	}
	teardownEnv := []string{
		fmt.Sprintf("OZ_WORKSPACE_ROOT=%s", workspaceDir),
		"OZ_WORKER_BACKEND=direct",
		fmt.Sprintf("OZ_RUN_ID=%s", taskID),
	}
	log.Infof(ctx, "Running teardown command: %s", b.config.TeardownCommand)
	if err := b.runCommand(ctx, b.config.TeardownCommand, workspaceDir, teardownEnv); err != nil {
		log.Warnf(ctx, "Teardown command failed: %v", err)
	}
}

// cleanup runs the teardown command (if configured) and removes the workspace directory.
func (b *DirectBackend) cleanup(ctx context.Context, taskID, workspaceDir string) {
	b.runTeardownIfConfigured(ctx, taskID, workspaceDir)

	log.Infof(ctx, "Removing workspace: %s", workspaceDir)
	if err := os.RemoveAll(workspaceDir); err != nil {
		log.Warnf(ctx, "Failed to remove workspace %s: %v", workspaceDir, err)
	}
}

// runCommand executes a shell command with the given working directory and environment.
// Setup/teardown commands inherit the full worker environment so they can access
// tools and credentials (e.g. aws, docker) needed for workspace provisioning.
func (b *DirectBackend) runCommand(ctx context.Context, command, dir string, env []string) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = mergeEnvVars(os.Environ(), env)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// mergeEnvVars merges base and override env var slices (KEY=VALUE format).
// Override entries take precedence over base entries with the same key.
func mergeEnvVars(base, override []string) []string {
	envMap := make(map[string]string, len(base)+len(override))
	var keys []string

	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, exists := envMap[key]; !exists {
			keys = append(keys, key)
		}
		envMap[key] = entry
	}

	for _, entry := range override {
		key, _, _ := strings.Cut(entry, "=")
		if _, exists := envMap[key]; !exists {
			keys = append(keys, key)
		}
		envMap[key] = entry
	}

	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, envMap[key])
	}
	return result
}

// parseEnvFile reads a dotenv-format file and returns a map of KEY=VALUE pairs.
// It supports quoted values, comments, and other standard dotenv syntax via godotenv.
func parseEnvFile(path string) (map[string]string, error) {
	return godotenv.Read(path)
}
