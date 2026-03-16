package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/warpdotdev/oz-agent-worker/internal/config"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/worker"
)

var CLI struct {
	ConfigFile    string   `help:"Path to YAML config file" type:"path"`
	APIKey        string   `help:"API key for authentication" env:"WARP_API_KEY" required:""`
	WorkerID      string   `help:"Worker host identifier (required via flag or config file)"`
	WebSocketURL  string   `default:"wss://oz.warp.dev/api/v1/selfhosted/worker/ws" hidden:""`
	ServerRootURL string   `default:"https://app.warp.dev" hidden:""`
	LogLevel      string   `help:"Log level (debug, info, warn, error)" default:"info" enum:"debug,info,warn,error"`
	NoCleanup     bool     `help:"Do not remove containers after execution (for debugging)"`
	Volumes       []string `help:"Volume mounts for task containers (format: HOST_PATH:CONTAINER_PATH or HOST_PATH:CONTAINER_PATH:MODE)" short:"v"`
	Env           []string `help:"Environment variables for task containers (format: KEY=VALUE or KEY to pass through from host)" short:"e"`
}

func main() {
	ctx := context.Background()

	kong.Parse(&CLI,
		kong.Name("oz-agent-worker"),
		kong.Description("Self-hosted worker for Warp ambient agents."),
		kong.UsageOnError(),
		kong.Vars{},
	)

	log.SetLevel(CLI.LogLevel)

	// Parse config file if provided.
	var fileConfig *config.FileConfig
	if CLI.ConfigFile != "" {
		var err error
		fileConfig, err = config.Load(CLI.ConfigFile)
		if err != nil {
			log.Fatalf(ctx, "%v", err)
		}
	}

	workerConfig, err := mergeConfig(fileConfig)
	if err != nil {
		log.Fatalf(ctx, "%v", err)
	}

	w, err := worker.New(ctx, workerConfig)
	if err != nil {
		log.Fatalf(ctx, "Failed to create worker: %v", err)
	}

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start worker in background
	go func() {
		if err := w.Start(); err != nil {
			log.Errorf(ctx, "Worker stopped with error: %v", err)
		}
	}()

	// Wait for signal
	sig := <-sigChan
	log.Infof(ctx, "Received signal %v, shutting down gracefully...", sig)

	w.Shutdown()

	log.Infof(ctx, "Worker shutdown complete")
}

// mergeConfig merges CLI flags with an optional config file.
// Priority: CLI flags > config file > defaults.
func mergeConfig(fileConfig *config.FileConfig) (worker.Config, error) {
	// Merge worker_id: CLI > config file.
	workerID := CLI.WorkerID
	if workerID == "" && fileConfig != nil {
		workerID = fileConfig.WorkerID
	}
	if workerID == "" {
		return worker.Config{}, fmt.Errorf("worker-id is required (via --worker-id flag or config file)")
	}
	if strings.HasPrefix(workerID, "warp") {
		return worker.Config{}, fmt.Errorf("invalid worker-id: values starting with 'warp' are reserved and cannot be used")
	}

	// Merge cleanup: --no-cleanup flag > config file cleanup > default (cleanup=true).
	noCleanup := CLI.NoCleanup
	if !noCleanup && fileConfig != nil && fileConfig.Cleanup != nil {
		noCleanup = !*fileConfig.Cleanup
	}

	// Parse CLI env flags.
	cliEnv, err := parseEnvFlags(CLI.Env)
	if err != nil {
		return worker.Config{}, err
	}

	// Merge env: config file first, then CLI entries overlay (CLI wins on key conflict).
	mergedEnv := make(map[string]string)
	if fileConfig != nil && fileConfig.Backend.Docker != nil {
		mergedEnv = config.ResolveEnv(fileConfig.Backend.Docker.Environment)
	}
	for k, v := range cliEnv {
		mergedEnv[k] = v
	}

	// Merge volumes: config file + CLI (concatenated).
	var volumes []string
	if fileConfig != nil && fileConfig.Backend.Docker != nil {
		volumes = append(volumes, fileConfig.Backend.Docker.Volumes...)
	}
	volumes = append(volumes, CLI.Volumes...)

	return worker.Config{
		APIKey:        CLI.APIKey,
		WorkerID:      workerID,
		WebSocketURL:  CLI.WebSocketURL,
		ServerRootURL: CLI.ServerRootURL,
		LogLevel:      CLI.LogLevel,
		NoCleanup:     noCleanup,
		Volumes:       volumes,
		Env:           mergedEnv,
	}, nil
}

// parseEnvFlags parses -e/--env flag values into a map.
// "KEY=VALUE" is used as-is; bare "KEY" inherits from the host environment.
// Empty keys and keys containing whitespace are rejected.
func parseEnvFlags(raw []string) (map[string]string, error) {
	result := make(map[string]string, len(raw))
	for _, entry := range raw {
		if entry == "" {
			return nil, fmt.Errorf("invalid --env flag: empty value")
		}

		key, value, hasEquals := strings.Cut(entry, "=")
		if key == "" {
			return nil, fmt.Errorf("invalid --env flag: missing key in %q", entry)
		}
		if strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("invalid --env flag: key contains whitespace in %q", entry)
		}

		if hasEquals {
			result[key] = value
		} else {
			result[key] = os.Getenv(key)
		}
	}
	return result, nil
}
