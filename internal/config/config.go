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
	WorkerID           string `yaml:"worker_id"`
	Cleanup            *bool  `yaml:"cleanup"`
	MaxConcurrentTasks *int   `yaml:"max_concurrent_tasks"`
	// IdleOnComplete controls how long the oz CLI process stays alive after a task's
	// conversation finishes, to allow follow-up interactions via the shared session.
	// Uses humantime format (e.g. "45m", "10m", "0s"). When nil, the oz CLI default
	// of 45 minutes is used.
	IdleOnComplete *string       `yaml:"idle_on_complete"`
	Backend        BackendConfig `yaml:"backend"`
}

type KubernetesTolerationConfig struct {
	Key               string `yaml:"key"`
	Operator          string `yaml:"operator" validate:"omitempty,oneof=Exists Equal"`
	Value             string `yaml:"value"`
	Effect            string `yaml:"effect" validate:"omitempty,oneof=NoSchedule PreferNoSchedule NoExecute"`
	TolerationSeconds *int64 `yaml:"toleration_seconds"`
}

type KubernetesResourcesConfig struct {
	Requests map[string]string `yaml:"requests"`
	Limits   map[string]string `yaml:"limits"`
}

// BackendConfig contains the backend selection.
// At most one backend field may be non-nil; configuring multiple backends simultaneously is an error.
type BackendConfig struct {
	Docker     *DockerConfig     `yaml:"docker"`
	Direct     *DirectConfig     `yaml:"direct"`
	Kubernetes *KubernetesConfig `yaml:"kubernetes"`
}

// DockerConfig holds Docker-backend-specific configuration.
type DockerConfig struct {
	Volumes     []string   `yaml:"volumes"`
	Environment []EnvEntry `yaml:"environment" validate:"dive"`
}

// DirectConfig holds direct-backend-specific configuration.
type DirectConfig struct {
	WorkspaceRoot   string     `yaml:"workspace_root"`
	TargetDir       string     `yaml:"target_dir"`
	OzPath          string     `yaml:"oz_path"`
	SetupCommand    string     `yaml:"setup_command"`
	TeardownCommand string     `yaml:"teardown_command"`
	Environment     []EnvEntry `yaml:"environment" validate:"dive"`
}

// KubernetesConfig holds Kubernetes-backend-specific configuration.
type KubernetesConfig struct {
	Namespace                     string                       `yaml:"namespace"`
	Kubeconfig                    string                       `yaml:"kubeconfig"`
	ImagePullSecret               string                       `yaml:"image_pull_secret" validate:"omitempty,no_whitespace"`
	ImagePullPolicy               string                       `yaml:"image_pull_policy" validate:"omitempty,oneof=Always Never IfNotPresent"`
	PreflightImage                string                       `yaml:"preflight_image" validate:"omitempty,no_whitespace"`
	ServiceAccount                string                       `yaml:"service_account" validate:"omitempty,no_whitespace"`
	SetupCommand                  string                       `yaml:"setup_command"`
	TeardownCommand               string                       `yaml:"teardown_command"`
	NodeSelector                  map[string]string            `yaml:"node_selector"`
	Tolerations                   []KubernetesTolerationConfig `yaml:"tolerations" validate:"dive"`
	Resources                     KubernetesResourcesConfig    `yaml:"resources"`
	ExtraLabels                   map[string]string            `yaml:"extra_labels"`
	ExtraAnnotations              map[string]string            `yaml:"extra_annotations"`
	ActiveDeadlineSeconds         *int64                       `yaml:"active_deadline_seconds"`
	TerminationGracePeriodSeconds *int64                       `yaml:"termination_grace_period_seconds"`
	WorkspaceSizeLimit            string                       `yaml:"workspace_size_limit"`
	UnschedulableTimeout          *string                      `yaml:"unschedulable_timeout"`
	Environment                   []EnvEntry                   `yaml:"environment" validate:"dive"`
	// PodTemplate holds a raw Kubernetes PodSpec that is merged with the worker's
	// required fields at runtime. When set, the legacy scheduling fields
	// (node_selector, tolerations, resources, service_account, image_pull_secret,
	// termination_grace_period_seconds) must not be set simultaneously.
	PodTemplate *RawYAMLNode `yaml:"pod_template"`
}

// RawYAMLNode captures a raw YAML sub-tree without applying KnownFields validation
// to its content. This is necessary because gopkg.in/yaml.v3's strict KnownFields
// mode would otherwise try to match Kubernetes field names against yaml.Node's own
// struct fields rather than treating the sub-tree as opaque YAML.
type RawYAMLNode struct {
	Node *yaml.Node
}

// UnmarshalYAML captures the raw YAML node, bypassing KnownFields strict validation.
func (r *RawYAMLNode) UnmarshalYAML(value *yaml.Node) error {
	r.Node = value
	return nil
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

	// Struct-level validator for KubernetesConfig: pod_template cannot be combined
	// with legacy scheduling fields.
	v.RegisterStructValidation(func(sl validator.StructLevel) {
		kc := sl.Current().Interface().(KubernetesConfig)
		if kc.PodTemplate == nil {
			return
		}
		hasLegacy := len(kc.NodeSelector) > 0 ||
			len(kc.Tolerations) > 0 ||
			len(kc.Resources.Requests) > 0 ||
			len(kc.Resources.Limits) > 0 ||
			kc.ServiceAccount != "" ||
			kc.ImagePullSecret != "" ||
			kc.TerminationGracePeriodSeconds != nil
		if hasLegacy {
			sl.ReportError(kc.PodTemplate, "PodTemplate", "PodTemplate", "pod_template_conflict", "")
		}
	}, KubernetesConfig{})

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
		case "pod_template_conflict":
			msgs = append(msgs, "pod_template cannot be combined with legacy scheduling fields (node_selector, tolerations, resources, service_account, image_pull_secret, termination_grace_period_seconds); migrate all fields into pod_template")
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
