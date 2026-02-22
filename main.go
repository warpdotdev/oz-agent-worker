package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/worker"
)

var CLI struct {
	APIKey        string   `help:"API key for authentication" env:"WARP_API_KEY" required:""`
	WorkerID      string   `help:"Worker host identifier" required:""`
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

	if strings.HasPrefix(CLI.WorkerID, "warp") {
		log.Fatalf(ctx, "Invalid worker-id: values starting with 'warp' are reserved and cannot be used")
	}

	log.SetLevel(CLI.LogLevel)

	envMap, err := parseEnvFlags(CLI.Env)
	if err != nil {
		log.Fatalf(ctx, "%v", err)
	}

	config := worker.Config{
		APIKey:        CLI.APIKey,
		WorkerID:      CLI.WorkerID,
		WebSocketURL:  CLI.WebSocketURL,
		ServerRootURL: CLI.ServerRootURL,
		LogLevel:      CLI.LogLevel,
		NoCleanup:     CLI.NoCleanup,
		Volumes:       CLI.Volumes,
		Env:           envMap,
	}

	w, err := worker.New(ctx, config)
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
