package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}
	return path
}

func TestLoadValidDockerConfig(t *testing.T) {
	path := writeTestConfig(t, `
worker_id: "my-worker"
cleanup: false
backend:
  docker:
    volumes:
      - "/data:/data:ro"
      - "/cache:/cache"
    environment:
      - name: FOO
        value: "bar"
      - name: BAZ
        value: "qux"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.WorkerID != "my-worker" {
		t.Errorf("worker_id = %q, want %q", cfg.WorkerID, "my-worker")
	}
	if cfg.Cleanup == nil || *cfg.Cleanup != false {
		t.Errorf("cleanup = %v, want false", cfg.Cleanup)
	}
	if cfg.Backend.Docker == nil {
		t.Fatal("expected docker backend to be set")
	}
	if len(cfg.Backend.Docker.Volumes) != 2 {
		t.Errorf("volumes count = %d, want 2", len(cfg.Backend.Docker.Volumes))
	}
	if len(cfg.Backend.Docker.Environment) != 2 {
		t.Errorf("environment count = %d, want 2", len(cfg.Backend.Docker.Environment))
	}
}

func TestLoadMinimalConfig(t *testing.T) {
	path := writeTestConfig(t, `
worker_id: "minimal"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.WorkerID != "minimal" {
		t.Errorf("worker_id = %q, want %q", cfg.WorkerID, "minimal")
	}
	if cfg.Cleanup != nil {
		t.Errorf("cleanup should be nil when not specified, got %v", *cfg.Cleanup)
	}
	if cfg.Backend.Docker != nil {
		t.Error("docker backend should be nil when not specified")
	}
}

func TestLoadCleanupDefaultTrue(t *testing.T) {
	path := writeTestConfig(t, `
worker_id: "test"
cleanup: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Cleanup == nil || *cfg.Cleanup != true {
		t.Errorf("cleanup = %v, want true", cfg.Cleanup)
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	path := writeTestConfig(t, `
worker_id: "test"
  bad_indent: true
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoadUnknownField(t *testing.T) {
	path := writeTestConfig(t, `
worker_id: "test"
unknown_field: "value"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestLoadEmptyEnvName(t *testing.T) {
	path := writeTestConfig(t, `
backend:
  docker:
    environment:
      - name: ""
        value: "bar"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty env name")
	}
}

func TestLoadEnvNameWithWhitespace(t *testing.T) {
	path := writeTestConfig(t, `
backend:
  docker:
    environment:
      - name: "MY VAR"
        value: "bar"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for env name with whitespace")
	}
}

func TestLoadEnvInheritFromHost(t *testing.T) {
	path := writeTestConfig(t, `
backend:
  docker:
    environment:
      - name: MY_HOST_VAR
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	env := cfg.Backend.Docker.Environment[0]
	if env.Name != "MY_HOST_VAR" {
		t.Errorf("name = %q, want %q", env.Name, "MY_HOST_VAR")
	}
	if env.Value != nil {
		t.Errorf("value should be nil for host-inherited var, got %q", *env.Value)
	}
}

func TestResolveEnv(t *testing.T) {
	explicit := "explicit_value"
	entries := []EnvEntry{
		{Name: "EXPLICIT", Value: &explicit},
		{Name: "FROM_HOST"},
	}

	t.Setenv("FROM_HOST", "host_value")

	result := ResolveEnv(entries)

	if result["EXPLICIT"] != "explicit_value" {
		t.Errorf("EXPLICIT = %q, want %q", result["EXPLICIT"], "explicit_value")
	}
	if result["FROM_HOST"] != "host_value" {
		t.Errorf("FROM_HOST = %q, want %q", result["FROM_HOST"], "host_value")
	}
}

func TestResolveEnvMissingHostVar(t *testing.T) {
	entries := []EnvEntry{
		{Name: "NONEXISTENT_VAR_12345"},
	}

	result := ResolveEnv(entries)

	if result["NONEXISTENT_VAR_12345"] != "" {
		t.Errorf("expected empty string for missing host var, got %q", result["NONEXISTENT_VAR_12345"])
	}
}

func TestLoadValidDirectConfig(t *testing.T) {
	path := writeTestConfig(t, `
worker_id: "direct-worker"
backend:
  direct:
    workspace_root: "/tmp/oz-workspaces"
    setup_command: "/opt/setup.sh"
    teardown_command: "/opt/teardown.sh"
    environment:
      - name: MY_VAR
        value: "hello"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Backend.Direct == nil {
		t.Fatal("expected direct backend to be set")
	}
	if cfg.Backend.Docker != nil {
		t.Error("docker backend should be nil")
	}
	if cfg.Backend.Direct.WorkspaceRoot != "/tmp/oz-workspaces" {
		t.Errorf("workspace_root = %q, want %q", cfg.Backend.Direct.WorkspaceRoot, "/tmp/oz-workspaces")
	}
	if cfg.Backend.Direct.SetupCommand != "/opt/setup.sh" {
		t.Errorf("setup_command = %q, want %q", cfg.Backend.Direct.SetupCommand, "/opt/setup.sh")
	}
	if cfg.Backend.Direct.TeardownCommand != "/opt/teardown.sh" {
		t.Errorf("teardown_command = %q, want %q", cfg.Backend.Direct.TeardownCommand, "/opt/teardown.sh")
	}
	if len(cfg.Backend.Direct.Environment) != 1 {
		t.Errorf("environment count = %d, want 1", len(cfg.Backend.Direct.Environment))
	}
}

func TestLoadDirectConfigWithTargetDir(t *testing.T) {
	path := writeTestConfig(t, `
worker_id: "direct-worker"
backend:
  direct:
    target_dir: "/home/user/myrepo"
    setup_command: "/opt/setup.sh"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Backend.Direct == nil {
		t.Fatal("expected direct backend to be set")
	}
	if cfg.Backend.Direct.TargetDir != "/home/user/myrepo" {
		t.Errorf("target_dir = %q, want %q", cfg.Backend.Direct.TargetDir, "/home/user/myrepo")
	}
	if cfg.Backend.Direct.WorkspaceRoot != "" {
		t.Errorf("workspace_root should be empty, got %q", cfg.Backend.Direct.WorkspaceRoot)
	}
}

func TestLoadValidKubernetesConfig(t *testing.T) {
	path := writeTestConfig(t, `
worker_id: "kubernetes-worker"
backend:
  kubernetes:
    namespace: "agents"
    kubeconfig: "/tmp/kubeconfig"
    image_pull_policy: "IfNotPresent"
    use_image_volumes: true
    preflight_image: "registry.internal/platform/preflight:1.0"
    setup_command: "printf 'SETUP=done\n' > \"$OZ_ENVIRONMENT_FILE\""
    teardown_command: "rm -rf \"$OZ_WORKSPACE_ROOT/tmp\""
    extra_labels:
      team: "platform"
    extra_annotations:
      owner: "oz"
    active_deadline_seconds: 1800
    workspace_size_limit: "10Gi"
    unschedulable_timeout: "2m"
    pod_template:
      serviceAccountName: "oz-agent-worker"
      imagePullSecrets:
        - name: "registry-creds"
      nodeSelector:
        workload: agents
      containers:
        - name: task
          resources:
            requests:
              cpu: "500m"
              memory: "512Mi"
            limits:
              cpu: "1"
              memory: "1Gi"
          env:
            - name: SHARED_SECRET
              value: "abc123"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Backend.Kubernetes == nil {
		t.Fatal("expected kubernetes backend to be set")
	}
	if cfg.Backend.Kubernetes.Namespace != "agents" {
		t.Errorf("namespace = %q, want %q", cfg.Backend.Kubernetes.Namespace, "agents")
	}
	if cfg.Backend.Kubernetes.ImagePullPolicy != "IfNotPresent" {
		t.Errorf("image_pull_policy = %q, want %q", cfg.Backend.Kubernetes.ImagePullPolicy, "IfNotPresent")
	}
	if !cfg.Backend.Kubernetes.UseImageVolumes {
		t.Fatal("expected use_image_volumes to be true")
	}
	if cfg.Backend.Kubernetes.PreflightImage != "registry.internal/platform/preflight:1.0" {
		t.Errorf("preflight_image = %q, want %q", cfg.Backend.Kubernetes.PreflightImage, "registry.internal/platform/preflight:1.0")
	}
	if cfg.Backend.Kubernetes.WorkspaceSizeLimit != "10Gi" {
		t.Fatalf("workspace_size_limit = %v, want 10Gi", cfg.Backend.Kubernetes.WorkspaceSizeLimit)
	}
	if cfg.Backend.Kubernetes.UnschedulableTimeout == nil || *cfg.Backend.Kubernetes.UnschedulableTimeout != "2m" {
		t.Fatalf("unschedulable_timeout = %v, want 2m", cfg.Backend.Kubernetes.UnschedulableTimeout)
	}
	if cfg.Backend.Kubernetes.PodTemplate == nil {
		t.Fatal("expected pod_template to be non-nil")
	}
	podTemplateYAML, err := yaml.Marshal(cfg.Backend.Kubernetes.PodTemplate.Node)
	if err != nil {
		t.Fatalf("failed to marshal pod_template: %v", err)
	}
	if !strings.Contains(string(podTemplateYAML), "serviceAccountName: \"oz-agent-worker\"") {
		t.Fatalf("expected pod_template to retain serviceAccountName, got:\n%s", string(podTemplateYAML))
	}
}

func TestLoadInvalidKubernetesPullPolicy(t *testing.T) {
	path := writeTestConfig(t, `
worker_id: "kubernetes-worker"
backend:
  kubernetes:
    image_pull_policy: "Sometimes"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid kubernetes image_pull_policy")
	}
}

func TestLoadBothBackendsError(t *testing.T) {
	path := writeTestConfig(t, `
backend:
  docker:
    volumes: []
  direct:
    workspace_root: "/tmp"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error when both backends are set")
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadIdleOnComplete(t *testing.T) {
	t.Run("parses idle_on_complete when set", func(t *testing.T) {
		path := writeTestConfig(t, `
worker_id: "test"
idle_on_complete: "10m"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.IdleOnComplete == nil {
			t.Fatal("expected idle_on_complete to be set")
		}
		if *cfg.IdleOnComplete != "10m" {
			t.Errorf("idle_on_complete = %q, want %q", *cfg.IdleOnComplete, "10m")
		}
	})

	t.Run("idle_on_complete is nil when not set", func(t *testing.T) {
		path := writeTestConfig(t, `
worker_id: "test"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.IdleOnComplete != nil {
			t.Errorf("expected idle_on_complete to be nil, got %q", *cfg.IdleOnComplete)
		}
	})
}

func TestLoadSkillsDirs(t *testing.T) {
	t.Run("parses skills_dirs when set", func(t *testing.T) {
		path := writeTestConfig(t, `
worker_id: "test"
skills_dirs:
  - /opt/skills
  - /home/user/my-skills
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.SkillsDirs) != 2 {
			t.Fatalf("skills_dirs count = %d, want 2", len(cfg.SkillsDirs))
		}
		if cfg.SkillsDirs[0] != "/opt/skills" {
			t.Errorf("skills_dirs[0] = %q, want %q", cfg.SkillsDirs[0], "/opt/skills")
		}
		if cfg.SkillsDirs[1] != "/home/user/my-skills" {
			t.Errorf("skills_dirs[1] = %q, want %q", cfg.SkillsDirs[1], "/home/user/my-skills")
		}
	})

	t.Run("skills_dirs is nil when not set", func(t *testing.T) {
		path := writeTestConfig(t, `
worker_id: "test"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.SkillsDirs != nil {
			t.Errorf("expected skills_dirs to be nil, got %v", cfg.SkillsDirs)
		}
	})
}

func TestLoadValidKubernetesPodTemplateConfig(t *testing.T) {
	path := writeTestConfig(t, `
worker_id: "k8s-worker"
backend:
  kubernetes:
    namespace: "agents"
    pod_template:
      nodeSelector:
        workload: agents
      containers:
        - name: task
          env:
            - name: SECRET_VALUE
              valueFrom:
                secretKeyRef:
                  name: my-secret
                  key: value
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Backend.Kubernetes == nil {
		t.Fatal("expected kubernetes backend to be set")
	}
	if cfg.Backend.Kubernetes.PodTemplate == nil {
		t.Fatal("expected pod_template to be non-nil")
	}
}

func TestLoadKubernetesDefaultImage(t *testing.T) {
	t.Run("parses default_image when set", func(t *testing.T) {
		path := writeTestConfig(t, `
worker_id: "k8s-worker"
backend:
  kubernetes:
    default_image: "my-registry.io/custom-image:latest"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Backend.Kubernetes == nil {
			t.Fatal("expected kubernetes backend to be set")
		}
		if cfg.Backend.Kubernetes.DefaultImage != "my-registry.io/custom-image:latest" {
			t.Errorf("default_image = %q, want %q", cfg.Backend.Kubernetes.DefaultImage, "my-registry.io/custom-image:latest")
		}
	})

	t.Run("default_image is empty when not set", func(t *testing.T) {
		path := writeTestConfig(t, `
worker_id: "k8s-worker"
backend:
  kubernetes:
    namespace: "agents"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Backend.Kubernetes.DefaultImage != "" {
			t.Errorf("expected default_image to be empty, got %q", cfg.Backend.Kubernetes.DefaultImage)
		}
	})

	t.Run("rejects default_image with whitespace", func(t *testing.T) {
		path := writeTestConfig(t, `
worker_id: "k8s-worker"
backend:
  kubernetes:
    default_image: "my image:latest"
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for default_image with whitespace")
		}
	})
}

func TestLoadKubernetesSidecarImage(t *testing.T) {
	t.Run("parses sidecar_image when set", func(t *testing.T) {
		path := writeTestConfig(t, `
worker_id: "k8s-worker"
backend:
  kubernetes:
    sidecar_image: "my-registry.io/warpdotdev/warp-agent:latest"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Backend.Kubernetes == nil {
			t.Fatal("expected kubernetes backend to be set")
		}
		if cfg.Backend.Kubernetes.SidecarImage != "my-registry.io/warpdotdev/warp-agent:latest" {
			t.Errorf("sidecar_image = %q, want %q", cfg.Backend.Kubernetes.SidecarImage, "my-registry.io/warpdotdev/warp-agent:latest")
		}
	})

	t.Run("sidecar_image is empty when not set", func(t *testing.T) {
		path := writeTestConfig(t, `
worker_id: "k8s-worker"
backend:
  kubernetes:
    namespace: "agents"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Backend.Kubernetes.SidecarImage != "" {
			t.Errorf("expected sidecar_image to be empty, got %q", cfg.Backend.Kubernetes.SidecarImage)
		}
	})

	t.Run("rejects sidecar_image with whitespace", func(t *testing.T) {
		path := writeTestConfig(t, `
worker_id: "k8s-worker"
backend:
  kubernetes:
    sidecar_image: "my image:latest"
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for sidecar_image with whitespace")
		}
	})
}

func TestLoadLegacyKubernetesFieldRejected(t *testing.T) {
	tests := []string{
		"image_pull_secret",
		"service_account",
		"node_selector",
		"tolerations",
		"resources",
		"termination_grace_period_seconds",
		"environment",
	}

	for _, field := range tests {
		t.Run(field, func(t *testing.T) {
			path := writeTestConfig(t, `
worker_id: "k8s-worker"
backend:
  kubernetes:
    `+field+`: {}
`)

			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error for removed kubernetes field %q", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("expected error to mention %q, got %v", field, err)
			}
		})
	}
}

func TestLoadMaxConcurrentTasks(t *testing.T) {
	t.Run("parses max_concurrent_tasks when set", func(t *testing.T) {
		path := writeTestConfig(t, `
worker_id: "test"
max_concurrent_tasks: 5
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.MaxConcurrentTasks == nil {
			t.Fatal("expected max_concurrent_tasks to be set")
		}
		if *cfg.MaxConcurrentTasks != 5 {
			t.Errorf("max_concurrent_tasks = %d, want 5", *cfg.MaxConcurrentTasks)
		}
	})

	t.Run("max_concurrent_tasks is nil when not set", func(t *testing.T) {
		path := writeTestConfig(t, `
worker_id: "test"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.MaxConcurrentTasks != nil {
			t.Errorf("expected max_concurrent_tasks to be nil, got %d", *cfg.MaxConcurrentTasks)
		}
	})
}
