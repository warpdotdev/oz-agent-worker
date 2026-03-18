package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v3"
)

// FileConfig represents the top-level YAML configuration file.
type FileConfig struct {
	WorkerID           string        `yaml:"worker_id"`
	Cleanup            *bool         `yaml:"cleanup"`
	MaxConcurrentTasks *int          `yaml:"max_concurrent_tasks"`
	// IdleOnComplete controls how long the oz CLI process stays alive after a task's
	// conversation finishes, to allow follow-up interactions via the shared session.
	// Uses humantime format (e.g. "45m", "10m", "0s"). When nil, the oz CLI default
	// of 45 minutes is used.
	IdleOnComplete *string       `yaml:"idle_on_complete"`
	Backend            BackendConfig `yaml:"backend"`
}

// BackendConfig contains the backend selection.
// At most one backend field may be non-nil; configuring multiple backends simultaneously is an error.
type BackendConfig struct {
	Docker *DockerConfig `yaml:"docker"`
	Direct *DirectConfig `yaml:"direct"`
}

// DockerConfig holds Docker-backend-specific configuration.
type DockerConfig struct {
	Volumes     []string   `yaml:"volumes"`
	Environment []EnvEntry `yaml:"environment" validate:"dive"`
}

// DirectConfig holds direct-backend-specific configuration.
type DirectConfig struct {
	WorkspaceRoot   string     `yaml:"workspace_root"`
	OzPath          string     `yaml:"oz_path"`
	SetupCommand    string     `yaml:"setup_command"`
	TeardownCommand string     `yaml:"teardown_command"`
	Environment     []EnvEntry `yaml:"environment" validate:"dive"`
}

// EnvEntry represents a single environment variable in the config file.
// If Value is nil, the variable is inherited from the host process environment.
type EnvEntry struct {
	Name  string  `yaml:"name" validate:"required,no_whitespace"`
	Value *string `yaml:"value"`
}

// configValidator is the package-level validator instance, initialized once.
var configValidator = newConfigValidator()

func newConfigValidator() *validator.Validate {
	v := validator.New()

	// no_whitespace rejects strings that contain spaces or tabs.
	_ = v.RegisterValidation("no_whitespace", func(fl validator.FieldLevel) bool {
		return !strings.ContainsAny(fl.Field().String(), " \t")
	})

	// Struct-level validator for BackendConfig: at most one backend field may be non-nil.
	// Uses reflection so that future backend fields are automatically covered.
	v.RegisterStructValidation(func(sl validator.StructLevel) {
		cfg := sl.Current()
		configured := 0
		for i := 0; i < cfg.NumField(); i++ {
			if cfg.Field(i).Kind() == reflect.Ptr && !cfg.Field(i).IsNil() {
				configured++
			}
		}
		if configured > 1 {
			sl.ReportError(sl.Current().Interface(), "Backend", "Backend", "only_one_backend", "")
		}
	}, BackendConfig{})

	return v
}

// Load reads and validates a YAML config file.
func Load(path string) (*FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg FileConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if err := configValidator.Struct(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", formatValidationErrors(err))
	}

	return &cfg, nil
}

// formatValidationErrors converts validator.ValidationErrors into a human-readable error.
func formatValidationErrors(err error) error {
	var validationErrors validator.ValidationErrors
	if !errors.As(err, &validationErrors) {
		return err
	}

	msgs := make([]string, 0, len(validationErrors))
	for _, e := range validationErrors {
		switch e.Tag() {
		case "required":
			msgs = append(msgs, fmt.Sprintf("%s is required", e.Namespace()))
		case "no_whitespace":
			msgs = append(msgs, fmt.Sprintf("%s must not contain whitespace", e.Namespace()))
		case "only_one_backend":
			msgs = append(msgs, "at most one backend may be configured")
		default:
			msgs = append(msgs, fmt.Sprintf("%s failed validation %q", e.Namespace(), e.Tag()))
		}
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}

// ResolveEnv converts environment entries to a map, resolving host-inherited values.
func ResolveEnv(entries []EnvEntry) map[string]string {
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.Value != nil {
			result[entry.Name] = *entry.Value
		} else {
			result[entry.Name] = os.Getenv(entry.Name)
		}
	}
	return result
}
