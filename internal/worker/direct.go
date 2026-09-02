package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"go.opentelemetry.io/otel/attribute"
)

const defaultWorkspaceRoot = "/var/lib/oz/workspaces"

// directBackendTypeName is the value this backend reports for OZ_WORKER_BACKEND.
const directBackendTypeName = "direct"

// directSetupEnvVars and directTeardownEnvVars return exactly the worker-owned variables this
// backend adds to the operator's setup and teardown hooks. TestBackendEnvPairsEveryOZName runs
// its pairing assertion over these, so a variable added here under only one of its two names
// fails the build. GIT_CONFIG_GLOBAL has no OZ_/WARP_ spelling and is left unpaired.
func directSetupEnvVars(workspaceDir, taskID, environmentFile string) []string {
	return concatEnvVars(
		workspaceRootEnvVars(workspaceDir),
		workerBackendEnvVars(directBackendTypeName),
		runIDEnvVars(taskID),
		environmentFileEnvVars(environmentFile),
	)
}

func directTeardownEnvVars(workspaceDir, gitConfigPath, taskID string) []string {
	return concatEnvVars(
		workspaceRootEnvVars(workspaceDir),
		[]string{fmt.Sprintf("GIT_CONFIG_GLOBAL=%s", gitConfigPath)},
		workerBackendEnvVars(directBackendTypeName),
		runIDEnvVars(taskID),
	)
}

// validateTaskIDForPath ensures task IDs are safe to use as a single path component.
func validateTaskIDForPath(taskID string) error {
	if taskID == "" || taskID == "." || taskID == ".." {
		return fmt.Errorf("invalid task ID")
	}
	if strings.Contains(taskID, "/") || strings.Contains(taskID, "\\") {
		return fmt.Errorf("invalid task ID")
	}
	if filepath.Base(taskID) != taskID {
		return fmt.Errorf("invalid task ID")
	}
	return nil
}

// defaultInheritedEnvVars are the host environment variables passed through to
// tasks and scripts by default. Sensitive worker credentials are intentionally
// excluded; additional variables can be opted in via the backend config.
var defaultInheritedEnvVars = []string{"HOME", "TMPDIR", "PATH"}

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

// prepareTaskGitConfig returns the path to use as the task's global git config
// (GIT_CONFIG_GLOBAL) along with a cleanup function. Redirecting only git's
// global config keeps writes like `git config --global url.<x>.insteadOf` out of
// the developer's real ~/.gitconfig (and $XDG_CONFIG_HOME/git/config) without
// repointing HOME for every tool the agent runs.
func prepareTaskGitConfig(workspaceDir string, usingTargetDir bool) (string, func(), error) {
	if !usingTargetDir {
		return filepath.Join(workspaceDir, ".gitconfig"), func() {}, nil
	}

	// In shared target-dir mode the workspace is the user's real checkout, so keep
	// the throwaway global config in a temporary directory outside of it.
	dir, err := os.MkdirTemp("", "oz-gitconfig-")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temporary git config directory: %w", err)
	}
	return filepath.Join(dir, ".gitconfig"), func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Warnf(context.Background(), "Failed to remove temporary git config dir %s: %v", dir, err)
		}
	}, nil
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
	// HarnessConfigDirs maps a harness name (e.g. "claude", "codex") to a host
	// directory path. When set, the backend copies the specified host directory
	// into the workspace's per-task harness config directory before task
	// execution. This makes local plugins and user settings available to the
	// harness inside each isolated task workspace.
	//
	// If the source directory does not exist the seed step is silently skipped
	// so that the worker can be configured ahead of any actual plugin install.
	HarnessConfigDirs map[string]string
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
		if err := os.MkdirAll(config.WorkspaceRoot, 0700); err != nil {
			return nil, fmt.Errorf("failed to create workspace root %s: %w", config.WorkspaceRoot, err)
		}
	}

	return &DirectBackend{
		config: config,
		ozPath: ozPath,
	}, nil
}

// ExecuteTask runs the agent directly on the host.
func (b *DirectBackend) ExecuteTask(ctx context.Context, params *TaskParams) ExecuteResult {
	taskID := params.TaskID
	if err := validateTaskIDForPath(taskID); err != nil {
		return executeError(newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonWorkspaceSetup, fmt.Errorf("invalid task ID for workspace path: %w", err)))
	}

	// Determine working directory: shared target dir or per-task workspace.
	var workspaceDir string
	usingTargetDir := b.config.TargetDir != ""

	if usingTargetDir {
		workspaceDir = b.config.TargetDir
	} else {
		// Create per-task workspace directory.
		workspaceDir = filepath.Join(b.config.WorkspaceRoot, taskID)
		if err := os.MkdirAll(workspaceDir, 0700); err != nil {
			return executeError(newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonWorkspaceSetup, fmt.Errorf("failed to create workspace directory: %w", err)))
		}
		log.Infof(ctx, "Created workspace: %s", workspaceDir)
	}
	gitConfigPath, cleanupGitConfig, err := prepareTaskGitConfig(workspaceDir, usingTargetDir)
	if err != nil {
		return executeError(err)
	}
	defer cleanupGitConfig()
	gitConfigEnv := []string{fmt.Sprintf("GIT_CONFIG_GLOBAL=%s", gitConfigPath)}

	defer func() {
		if usingTargetDir {
			// Don't clean up the shared target directory.
			b.runTeardownIfConfigured(ctx, taskID, workspaceDir, gitConfigPath)
			return
		}
		if b.config.NoCleanup {
			log.Infof(ctx, "Skipping cleanup for workspace: %s", workspaceDir)
			return
		}
		b.cleanup(ctx, taskID, workspaceDir, gitConfigPath)
	}()

	// 2. Create temp environment file for setup script to write to.
	envFile, err := os.CreateTemp(workspaceDir, "oz-env-*")
	if err != nil {
		return executeError(newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonWorkspaceSetup, fmt.Errorf("failed to create environment file: %w", err)))
	}
	envFilePath := envFile.Name()
	if err := envFile.Close(); err != nil {
		return executeError(newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonWorkspaceSetup, fmt.Errorf("failed to close environment file: %w", err)))
	}
	defer func() {
		if err := os.Remove(envFilePath); err != nil && !os.IsNotExist(err) {
			log.Warnf(ctx, "Failed to remove environment file %s: %v", envFilePath, err)
		}
	}()

	// 3. Build environment variables: common + config-level.
	envVars := make([]string, len(params.EnvVars))
	copy(envVars, params.EnvVars)
	for key, value := range b.config.Env {
		envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
	}
	envVars = mergeEnvVars(envVars, harnessEnvVars(workspaceDir, params))
	envVars = mergeEnvVars(envVars, gitConfigEnv)

	// 3a. Seed harness config dir from host if configured.
	if !usingTargetDir {
		if err := b.seedHarnessConfigDir(ctx, workspaceDir, params); err != nil {
			return executeError(newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonWorkspaceSetup, err))
		}
	}

	// 4. Run setup command if configured.
	if b.config.SetupCommand != "" {
		setupEnv := append(envVars, directSetupEnvVars(workspaceDir, taskID, envFilePath)...)

		log.Infof(ctx, "Running setup command: %s", b.config.SetupCommand)
		if err := b.runCommand(ctx, b.config.SetupCommand, workspaceDir, setupEnv); err != nil {
			if ctx.Err() != nil {
				return executeError(newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonTaskCancelled, ctx.Err()))
			}
			return executeError(newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonSetupCommand, fmt.Errorf("setup command failed: %w", err)))
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
	envVars = mergeEnvVars(envVars, gitConfigEnv)

	// 6. Invoke oz CLI with base args.
	// Start from a minimal host base (HOME, TMPDIR, PATH) and overlay task env vars,
	// so sensitive worker credentials are never exposed to tasks.
	cmd := exec.CommandContext(ctx, b.ozPath, params.BaseArgs...) // #nosec G204 -- ozPath is resolved at backend startup and args are generated by the worker.
	cmd.Dir = workspaceDir
	cmd.Env = mergeEnvVars(hostBaseEnv(), envVars)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	log.Infof(ctx, "Running oz agent in workspace %s", workspaceDir)
	log.Debugf(ctx, "Command: %s %s", b.ozPath, strings.Join(params.BaseArgs, " "))

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return executeError(newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonTaskCancelled, ctx.Err()))
		}
		wrapped := fmt.Errorf("oz agent exited with error: %w", err)
		if exitCode, ok := agentExitCode(err); ok {
			return executeError(newBackendFailureWithExitCode(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonAgentInvocation, wrapped, exitCode))
		}
		return executeError(newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonAgentInvocation, wrapped))
	}

	log.Infof(ctx, "Task %s execution completed successfully", taskID)
	return executeCompleted()
}

// agentExitCode extracts the agent subprocess's exit code from a cmd.Run error.
// os/exec reports signal deaths as exit code -1 rather than a signal-coded
// status, so the signal is recovered from the wait status and normalized to
// 128+signal — the form failure-cause classification keys on to tell crashes
// and operator shutdowns apart from ordinary failures. ok is false when the
// error does not carry a process exit status (e.g. the binary never launched).
func agentExitCode(err error) (exitCode int, ok bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	status, isWaitStatus := exitErr.Sys().(syscall.WaitStatus)
	if !isWaitStatus {
		return 0, false
	}
	if status.Signaled() {
		return 128 + int(status.Signal()), true
	}
	return status.ExitStatus(), true
}

// CancelTask is a no-op: cancelling the ExecuteTask context fully stops a
// direct-backend task.
func (b *DirectBackend) CancelTask(context.Context, *CancelParams) error { return nil }

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

func (b *DirectBackend) PreservesTasksOnShutdown() bool {
	return false
}

// runTeardownIfConfigured runs the teardown command if one is configured.
func (b *DirectBackend) runTeardownIfConfigured(ctx context.Context, taskID, workspaceDir, gitConfigPath string) {
	if b.config.TeardownCommand == "" {
		return
	}
	teardownEnv := directTeardownEnvVars(workspaceDir, gitConfigPath, taskID)
	log.Infof(ctx, "Running teardown command: %s", b.config.TeardownCommand)
	if err := b.runCommand(ctx, b.config.TeardownCommand, workspaceDir, teardownEnv); err != nil {
		metrics.AddTaskEvent(ctx, "cleanup.failed",
			attribute.String("operation", "teardown"),
			attribute.String("error.message", err.Error()),
		)
		log.Warnf(ctx, "Teardown command failed: %v", err)
	}
}

// cleanup runs the teardown command (if configured) and removes the workspace directory.
func (b *DirectBackend) cleanup(ctx context.Context, taskID, workspaceDir, gitConfigPath string) {
	b.runTeardownIfConfigured(ctx, taskID, workspaceDir, gitConfigPath)

	log.Infof(ctx, "Removing workspace: %s", workspaceDir)
	if err := os.RemoveAll(workspaceDir); err != nil {
		metrics.AddTaskEvent(ctx, "cleanup.failed",
			attribute.String("operation", "remove_workspace"),
			attribute.String("error.message", err.Error()),
		)
		log.Warnf(ctx, "Failed to remove workspace %s: %v", workspaceDir, err)
	}
}

// runCommand executes a shell command with the given working directory and environment.
// Setup/teardown commands inherit the full worker environment so they can access
// tools and credentials (e.g. aws, docker) needed for workspace provisioning.
func (b *DirectBackend) runCommand(ctx context.Context, command, dir string, env []string) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command) // #nosec G204 -- setup/teardown commands are explicit operator configuration.
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

type harnessConfig struct {
	configEnvVar string
	configDir    string
}

// Used for setting configEnvVar to "workspaceDir/configDir"
var harnessConfigs = map[string]harnessConfig{
	"claude": {
		configEnvVar: "CLAUDE_CONFIG_DIR",
		configDir:    ".claude",
	},
	"codex": {
		configEnvVar: "CODEX_HOME",
		configDir:    ".codex",
	},
}

// When running with a third-party harness, set state environment variables so that the harness
// state will be written to the workspace dir rather than globally. This helps us keep concurrent tasks
// from interfering with one another.
func harnessEnvVars(workspaceDir string, params *TaskParams) []string {
	if params == nil ||
		params.Task == nil ||
		params.Task.AgentConfigSnapshot == nil ||
		params.Task.AgentConfigSnapshot.Harness == nil ||
		params.Task.AgentConfigSnapshot.Harness.Type == nil {
		return nil
	}
	config, ok := harnessConfigs[strings.TrimSpace(*params.Task.AgentConfigSnapshot.Harness.Type)]
	if !ok {
		return nil
	}
	return []string{fmt.Sprintf("%s=%s", config.configEnvVar, filepath.Join(workspaceDir, config.configDir))}
}

// seedHarnessConfigDir copies the operator-configured host harness config
// directory into the task workspace's harness config directory.
//
// This lets per-user Claude Code (or Codex) plugins and settings be available
// in each task's isolated config directory without leaking writes between
// concurrent tasks.
//
// If the configured host directory does not exist the step is silently skipped
// so that the worker can be configured ahead of any actual plugin install.
func (b *DirectBackend) seedHarnessConfigDir(ctx context.Context, workspaceDir string, params *TaskParams) error {
	if len(b.config.HarnessConfigDirs) == 0 ||
		params == nil ||
		params.Task == nil ||
		params.Task.AgentConfigSnapshot == nil ||
		params.Task.AgentConfigSnapshot.Harness == nil ||
		params.Task.AgentConfigSnapshot.Harness.Type == nil {
		return nil
	}

	harnessType := strings.TrimSpace(*params.Task.AgentConfigSnapshot.Harness.Type)
	hostSrcDir, ok := b.config.HarnessConfigDirs[harnessType]
	if !ok || hostSrcDir == "" {
		return nil
	}

	harnessConf, ok := harnessConfigs[harnessType]
	if !ok {
		return nil
	}

	// Silently skip when the host source directory does not exist.
	if _, err := os.Stat(hostSrcDir); os.IsNotExist(err) {
		log.Infof(ctx, "Harness config dir %q does not exist; skipping seed for harness %q", hostSrcDir, harnessType)
		return nil
	}

	destDir := filepath.Join(workspaceDir, harnessConf.configDir)
	log.Infof(ctx, "Seeding harness config dir for %q from %q into %q", harnessType, hostSrcDir, destDir)
	if err := os.CopyFS(destDir, os.DirFS(hostSrcDir)); err != nil {
		return fmt.Errorf("failed to seed harness config dir for %q from %q: %w", harnessType, hostSrcDir, err)
	}
	return nil
}
