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

// oomScoreAdjOzAgent is the OOM score adjustment applied to the oz agent
// process after it is spawned. A negative value makes the process less likely
// to be OOM-killed by the kernel, so memory-hungry child processes (e.g.
// `cargo build`) are targeted first. The range is [-1000, 1000]; negative
// values require CAP_SYS_RESOURCE. This adjustment is best-effort: if the
// worker lacks the capability, a warning is logged and execution continues.
const oomScoreAdjOzAgent = -300

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
func (b *DirectBackend) ExecuteTask(ctx context.Context, params *TaskParams) error {
	taskID := params.TaskID
	if err := validateTaskIDForPath(taskID); err != nil {
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonWorkspaceSetup, fmt.Errorf("invalid task ID for workspace path: %w", err))
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
			return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonWorkspaceSetup, fmt.Errorf("failed to create workspace directory: %w", err))
		}
		log.Infof(ctx, "Created workspace: %s", workspaceDir)
	}
	gitConfigPath, cleanupGitConfig, err := prepareTaskGitConfig(workspaceDir, usingTargetDir)
	if err != nil {
		return err
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
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonWorkspaceSetup, fmt.Errorf("failed to create environment file: %w", err))
	}
	envFilePath := envFile.Name()
	if err := envFile.Close(); err != nil {
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonWorkspaceSetup, fmt.Errorf("failed to close environment file: %w", err))
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
			if ctx.Err() != nil {
				return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonTaskCancelled, ctx.Err())
			}
			return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonSetupCommand, fmt.Errorf("setup command failed: %w", err))
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

	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonTaskCancelled, ctx.Err())
		}
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonAgentInvocation, fmt.Errorf("oz agent failed to start: %w", err))
	}

	// Protect the oz agent process from OOM killing. Child processes that the
	// agent spawns (e.g. cargo build) may use far more memory and should be
	// targeted by the kernel's OOM killer first.
	setOOMScoreAdj(ctx, cmd.Process.Pid, oomScoreAdjOzAgent)

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonTaskCancelled, ctx.Err())
		}
		reason, msg := classifyAgentExitError(ctx, err)
		return newBackendFailure(metrics.TaskFailurePhaseBackend, reason, fmt.Errorf("%s: %w", msg, err))
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

func (b *DirectBackend) PreservesTasksOnShutdown() bool {
	return false
}

// runTeardownIfConfigured runs the teardown command if one is configured.
func (b *DirectBackend) runTeardownIfConfigured(ctx context.Context, taskID, workspaceDir, gitConfigPath string) {
	if b.config.TeardownCommand == "" {
		return
	}
	teardownEnv := []string{
		fmt.Sprintf("OZ_WORKSPACE_ROOT=%s", workspaceDir),
		fmt.Sprintf("GIT_CONFIG_GLOBAL=%s", gitConfigPath),
		"OZ_WORKER_BACKEND=direct",
		fmt.Sprintf("OZ_RUN_ID=%s", taskID),
	}
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

// setOOMScoreAdj writes the Linux OOM score adjustment for the given pid.
// A negative score makes the process less likely to be OOM-killed; writing
// negative values requires CAP_SYS_RESOURCE. This function is best-effort:
// on non-Linux systems or when the worker lacks the required capability, the
// error is logged as a warning and execution continues uninterrupted.
func setOOMScoreAdj(ctx context.Context, pid int, score int) {
	path := fmt.Sprintf("/proc/%d/oom_score_adj", pid)
	data := fmt.Sprintf("%d\n", score)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		log.Warnf(ctx, "Failed to set OOM score adj for oz agent (pid %d) to %d: %v"+
			" — the agent process is unprotected from OOM killing", pid, score, err)
		return
	}
	log.Infof(ctx, "Set OOM score adj for oz agent (pid %d) to %d"+
		" (child processes that use more memory will be killed first)", pid, score)
}

// classifyAgentExitError inspects the exit error of the oz agent process and
// returns the appropriate failure reason constant and a user-facing message.
// It detects SIGKILL exits, which may indicate the oz agent process itself was
// OOM-killed, and records a dedicated span event for observability.
func classifyAgentExitError(ctx context.Context, err error) (reason, message string) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() && status.Signal() == syscall.SIGKILL {
				log.Errorf(ctx, "oz agent process was killed by SIGKILL — possible OOM kill.\n"+
					"Hint: the kernel may have targeted the agent process instead of a memory-hungry\n"+
					"child (e.g. `cargo build`). Check kernel logs (dmesg) for OOM kill events.")
				metrics.AddTaskEvent(ctx, "agent.sigkill_detected",
					attribute.String("signal", "SIGKILL"),
					attribute.String("hint", "possible_oom_kill"),
				)
				return metrics.TaskFailureReasonAgentOOM,
					"The agent process was unexpectedly killed (SIGKILL — possible out-of-memory)." +
						" Check VM memory usage and kernel OOM logs (dmesg). Consider reducing" +
						" parallelism (e.g. CARGO_BUILD_JOBS=1) or increasing available memory."
			}
		}
	}
	return metrics.TaskFailureReasonAgentInvocation, "oz agent exited with error"
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
