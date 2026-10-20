package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/warpdotdev/oz-agent-worker/internal/config"
	"gopkg.in/yaml.v3"
)

func resetCLIForTest() {
	CLI.ConfigFile = ""
	CLI.Backend = ""
	CLI.APIKey = ""
	CLI.WorkerID = ""
	CLI.WebSocketURL = ""
	CLI.ServerRootURL = ""
	CLI.LogLevel = ""
	CLI.NoCleanup = false
	CLI.Volumes = nil
	CLI.Env = nil
	CLI.MaxConcurrentTasks = 0
	CLI.OneShot = false
	CLI.CollectTaskLogs = nil
	CLI.IdleOnComplete = ""
}

func TestCollectTaskLogsConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env string
		args            []string
		want            bool
	}{
		{name: "default enabled", want: true},
		{name: "YAML disabled", yaml: "collect_task_logs: false\n"},
		{name: "CLI disabled overrides YAML", yaml: "collect_task_logs: true\n", args: []string{"--collect-task-logs=false"}},
		{name: "CLI enabled overrides YAML", yaml: "collect_task_logs: false\n", args: []string{"--collect-task-logs"}, want: true},
		{name: "environment disabled overrides YAML", yaml: "collect_task_logs: true\n", env: "false"},
		{name: "CLI enabled overrides environment", env: "false", args: []string{"--collect-task-logs=true"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetCLIForTest()
			t.Cleanup(resetCLIForTest)
			t.Setenv("WARP_API_KEY", "test-api-key")
			t.Setenv("OZ_COLLECT_TASK_LOGS", tc.env)
			if tc.env == "" {
				if err := os.Unsetenv("OZ_COLLECT_TASK_LOGS"); err != nil {
					t.Fatal(err)
				}
			}
			parser, err := kong.New(&CLI)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parser.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("worker_id: test-worker\n"+tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			wc, err := mergeConfig(cfg)
			if err != nil || wc.CollectTaskLogs != tc.want {
				t.Fatalf("CollectTaskLogs = %t, want %t: %v", wc.CollectTaskLogs, tc.want, err)
			}
		})
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func stringPtr(v string) *string {
	return &v
}

func int64Ptr(v int64) *int64 {
	return &v
}

func intPtr(v int) *int {
	return &v
}

func rawYAMLNodeFromString(t *testing.T, content string) *config.RawYAMLNode {
	t.Helper()

	var node yaml.Node
	if err := yaml.Unmarshal([]byte(content), &node); err != nil {
		t.Fatalf("failed to unmarshal raw YAML node: %v", err)
	}
	if len(node.Content) == 0 {
		t.Fatal("expected raw YAML node content")
	}

	return &config.RawYAMLNode{Node: node.Content[0]}
}

func TestMergeConfigKubernetesFromFile(t *testing.T) {
	resetCLIForTest()
	t.Cleanup(resetCLIForTest)

	CLI.Env = []string{"CLI_ONLY=1", "OVERRIDE=cli"}

	fileConfig := &config.FileConfig{
		WorkerID: "worker-123",
		Cleanup:  boolPtr(true),
		Backend: config.BackendConfig{
			Kubernetes: &config.KubernetesConfig{
				Namespace:       "agents",
				Kubeconfig:      "/tmp/kubeconfig",
				ImagePullPolicy: "IfNotPresent",
				UseImageVolumes: true,
				PreflightImage:  "registry.internal/platform/preflight:1.0",
				SetupCommand:    "setup.sh",
				TeardownCommand: "teardown.sh",
				ExtraLabels: map[string]string{
					"team": "platform",
				},
				ExtraAnnotations: map[string]string{
					"owner": "oz",
				},
				ActiveDeadlineSeconds: int64Ptr(900),
				WorkspaceSizeLimit:    "8Gi",
				UnschedulableTimeout:  stringPtr("2m"),
				PodTemplate: rawYAMLNodeFromString(t, `
serviceAccountName: task-runner
imagePullSecrets:
  - name: registry-creds
containers:
  - name: task
    env:
      - name: FROM_TEMPLATE
        value: "1"
`),
			},
		},
	}

	wc, err := mergeConfig(fileConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if wc.BackendType != "kubernetes" {
		t.Fatalf("BackendType = %q, want %q", wc.BackendType, "kubernetes")
	}
	if wc.Kubernetes == nil {
		t.Fatal("expected kubernetes backend config")
	}
	if wc.Kubernetes.WorkerID != "worker-123" {
		t.Errorf("WorkerID = %q, want %q", wc.Kubernetes.WorkerID, "worker-123")
	}
	if wc.Kubernetes.Namespace != "agents" {
		t.Errorf("Namespace = %q, want %q", wc.Kubernetes.Namespace, "agents")
	}
	if wc.Kubernetes.PreflightImage != "registry.internal/platform/preflight:1.0" {
		t.Errorf("PreflightImage = %q, want %q", wc.Kubernetes.PreflightImage, "registry.internal/platform/preflight:1.0")
	}
	if !wc.Kubernetes.UseImageVolumes {
		t.Fatal("expected UseImageVolumes to be true")
	}
	if wc.Kubernetes.TaskEnv["CLI_ONLY"] != "1" {
		t.Errorf("CLI_ONLY = %q, want %q", wc.Kubernetes.TaskEnv["CLI_ONLY"], "1")
	}
	if wc.Kubernetes.TaskEnv["OVERRIDE"] != "cli" {
		t.Errorf("OVERRIDE = %q, want %q", wc.Kubernetes.TaskEnv["OVERRIDE"], "cli")
	}
	if wc.Kubernetes.WorkspaceSizeLimit == nil || wc.Kubernetes.WorkspaceSizeLimit.String() != "8Gi" {
		t.Fatalf("WorkspaceSizeLimit = %v, want 8Gi", wc.Kubernetes.WorkspaceSizeLimit)
	}
	if wc.Kubernetes.UnschedulableTimeout == nil || *wc.Kubernetes.UnschedulableTimeout != 2*time.Minute {
		t.Fatalf("UnschedulableTimeout = %v, want 2m", wc.Kubernetes.UnschedulableTimeout)
	}
	if wc.Kubernetes.PodTemplate == nil {
		t.Fatal("expected PodTemplate to be set")
	}
	if wc.Kubernetes.PodTemplate.ServiceAccountName != "task-runner" {
		t.Fatalf("ServiceAccountName = %q, want %q", wc.Kubernetes.PodTemplate.ServiceAccountName, "task-runner")
	}
	if len(wc.Kubernetes.PodTemplate.ImagePullSecrets) != 1 || wc.Kubernetes.PodTemplate.ImagePullSecrets[0].Name != "registry-creds" {
		t.Fatalf("ImagePullSecrets = %+v, want registry-creds", wc.Kubernetes.PodTemplate.ImagePullSecrets)
	}
}

func TestMergeConfigOneShot(t *testing.T) {
	tests := []struct {
		name       string
		cliOneShot bool
		fileValue  *bool
		maxTasks   *int
		want       bool
	}{
		{name: "disabled by default", want: false},
		{name: "enabled from file", fileValue: boolPtr(true), want: true},
		{name: "enabled from CLI", cliOneShot: true, fileValue: boolPtr(false), want: true},
		{name: "overrides configured concurrency", fileValue: boolPtr(true), maxTasks: intPtr(8), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCLIForTest()
			t.Cleanup(resetCLIForTest)
			CLI.OneShot = tt.cliOneShot
			fileConfig := &config.FileConfig{
				WorkerID:           "direct-worker",
				OneShot:            tt.fileValue,
				MaxConcurrentTasks: tt.maxTasks,
				Backend: config.BackendConfig{
					Direct: &config.DirectConfig{},
				},
			}

			wc, err := mergeConfig(fileConfig)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if wc.OneShot != tt.want {
				t.Fatalf("OneShot = %t, want %t", wc.OneShot, tt.want)
			}
			if tt.want && wc.MaxConcurrentTasks != 1 {
				t.Fatalf("MaxConcurrentTasks = %d, want 1", wc.MaxConcurrentTasks)
			}
		})
	}
}

func TestMergeConfigKubernetesCLIOverridesCleanupAndWorkerID(t *testing.T) {
	resetCLIForTest()
	t.Cleanup(resetCLIForTest)

	CLI.Backend = "kubernetes"
	CLI.WorkerID = "cli-worker"
	CLI.NoCleanup = true
	CLI.Env = []string{"FROM_CLI=1"}

	fileConfig := &config.FileConfig{
		WorkerID: "file-worker",
		Cleanup:  boolPtr(true),
		Backend: config.BackendConfig{
			Kubernetes: &config.KubernetesConfig{},
		},
	}

	wc, err := mergeConfig(fileConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if wc.Kubernetes == nil {
		t.Fatal("expected kubernetes backend config")
	}
	if wc.Kubernetes.WorkerID != "cli-worker" {
		t.Errorf("WorkerID = %q, want %q", wc.Kubernetes.WorkerID, "cli-worker")
	}
	if !wc.Kubernetes.NoCleanup {
		t.Error("expected CLI --no-cleanup to take precedence")
	}
	if wc.Kubernetes.TaskEnv["FROM_CLI"] != "1" {
		t.Errorf("FROM_CLI = %q, want %q", wc.Kubernetes.TaskEnv["FROM_CLI"], "1")
	}
}

func TestMergeConfigCommandFromFile(t *testing.T) {
	resetCLIForTest()
	t.Cleanup(resetCLIForTest)

	CLI.ServerRootURL = "https://app.warp.dev"
	CLI.Env = []string{"CLI_ONLY=1"}

	fileConfig := &config.FileConfig{
		WorkerID: "command-worker",
		Backend: config.BackendConfig{
			Command: &config.CommandConfig{
				DispatchCommand: "/opt/oz/dispatch.sh",
				CancelCommand:   "/opt/oz/cancel.sh",
				DispatchTimeout: "30s",
			},
		},
	}

	wc, err := mergeConfig(fileConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if wc.BackendType != "command" {
		t.Fatalf("BackendType = %q, want %q", wc.BackendType, "command")
	}
	if wc.Command == nil {
		t.Fatal("expected command backend config")
	}
	if wc.Command.DispatchCommand != "/opt/oz/dispatch.sh" {
		t.Errorf("DispatchCommand = %q, want %q", wc.Command.DispatchCommand, "/opt/oz/dispatch.sh")
	}
	if wc.Command.CancelCommand != "/opt/oz/cancel.sh" {
		t.Errorf("CancelCommand = %q, want %q", wc.Command.CancelCommand, "/opt/oz/cancel.sh")
	}
	if wc.Command.DispatchTimeout != 30*time.Second {
		t.Errorf("DispatchTimeout = %v, want 30s", wc.Command.DispatchTimeout)
	}
	if wc.Command.ServerRootURL != "https://app.warp.dev" {
		t.Errorf("ServerRootURL = %q, want %q", wc.Command.ServerRootURL, "https://app.warp.dev")
	}
	if wc.Command.WorkerID != "command-worker" {
		t.Errorf("WorkerID = %q, want %q", wc.Command.WorkerID, "command-worker")
	}
	if wc.Command.Env["CLI_ONLY"] != "1" {
		t.Errorf("Env[CLI_ONLY] = %q, want %q", wc.Command.Env["CLI_ONLY"], "1")
	}
}

func TestMergeConfigCommandInvalidDispatchTimeout(t *testing.T) {
	resetCLIForTest()
	t.Cleanup(resetCLIForTest)

	fileConfig := &config.FileConfig{
		WorkerID: "command-worker",
		Backend: config.BackendConfig{
			Command: &config.CommandConfig{
				DispatchCommand: "/opt/oz/dispatch.sh",
				DispatchTimeout: "nope",
			},
		},
	}

	if _, err := mergeConfig(fileConfig); err == nil {
		t.Fatal("expected error for invalid dispatch_timeout")
	}
}

func TestMergeConfigKubernetesAllowsZeroUnschedulableTimeout(t *testing.T) {
	resetCLIForTest()
	t.Cleanup(resetCLIForTest)

	fileConfig := &config.FileConfig{
		WorkerID: "worker-123",
		Backend: config.BackendConfig{
			Kubernetes: &config.KubernetesConfig{
				UnschedulableTimeout: stringPtr("0s"),
			},
		},
	}

	wc, err := mergeConfig(fileConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wc.Kubernetes == nil {
		t.Fatal("expected kubernetes backend config")
	}
	if wc.Kubernetes.UnschedulableTimeout == nil || *wc.Kubernetes.UnschedulableTimeout != 0 {
		t.Fatalf("UnschedulableTimeout = %v, want 0", wc.Kubernetes.UnschedulableTimeout)
	}
}

func TestMergeConfigDockerImagePullPolicyFromFile(t *testing.T) {
	resetCLIForTest()
	t.Cleanup(resetCLIForTest)

	fileConfig := &config.FileConfig{
		WorkerID: "docker-worker",
		Backend: config.BackendConfig{
			Docker: &config.DockerConfig{
				ImagePullPolicy: "Never",
				Volumes:         []string{"/data:/data"},
			},
		},
	}

	wc, err := mergeConfig(fileConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if wc.BackendType != "docker" {
		t.Fatalf("BackendType = %q, want %q", wc.BackendType, "docker")
	}
	if wc.Docker == nil {
		t.Fatal("expected docker backend config")
	}
	if wc.Docker.ImagePullPolicy != "Never" {
		t.Errorf("ImagePullPolicy = %q, want %q", wc.Docker.ImagePullPolicy, "Never")
	}
	if len(wc.Docker.Volumes) != 1 || wc.Docker.Volumes[0] != "/data:/data" {
		t.Errorf("Volumes = %v, want [/data:/data]", wc.Docker.Volumes)
	}
}

func TestMergeConfigDockerImagePullPolicyOmitted(t *testing.T) {
	resetCLIForTest()
	t.Cleanup(resetCLIForTest)

	fileConfig := &config.FileConfig{
		WorkerID: "docker-worker",
		Backend: config.BackendConfig{
			Docker: &config.DockerConfig{},
		},
	}

	wc, err := mergeConfig(fileConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wc.Docker == nil {
		t.Fatal("expected docker backend config")
	}
	if wc.Docker.ImagePullPolicy != "" {
		t.Errorf("ImagePullPolicy = %q, want empty (defaulting happens at the worker layer)", wc.Docker.ImagePullPolicy)
	}
}

func TestMergeConfigLegacySidecarMappings(t *testing.T) {
	for _, backend := range []string{"docker", "kubernetes"} {
		t.Run(backend, func(t *testing.T) {
			resetCLIForTest()
			t.Cleanup(resetCLIForTest)
			content := fmt.Sprintf("worker_id: test-worker\nbackend:\n  %s:\n    sidecar_image: mirror.example/agent:approved\n", backend)
			if backend == "kubernetes" {
				content += "    coding_cli_sidecars:\n      claude: mirror.example/claude\n      codex: \"\"\n"
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			wc, err := mergeConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := wc.SidecarImageMappings.Remap("docker.io/warpdotdev/warp-agent:v1"); got != "mirror.example/agent:approved" {
				t.Fatalf("main-agent mapping = %q", got)
			}
			if backend == "kubernetes" {
				for _, transport := range []string{"cli", "acp"} {
					source := fmt.Sprintf("docker.io/warpdotdev/warp-claude-%s-sidecar:v2", transport)
					if got := wc.SidecarImageMappings.Remap(source); got != "mirror.example/claude:v2" {
						t.Fatalf("%s mapping = %q", transport, got)
					}
				}
				source := "warpdotdev/warp-codex-cli-sidecar:v3"
				if got := wc.SidecarImageMappings.Remap(source); got != source {
					t.Fatalf("empty legacy image remapped %q to %q", source, got)
				}
			}
		})
	}
}

func TestMergeConfigSidecarMappingsRejectLegacyOptions(t *testing.T) {
	for _, tc := range []struct {
		backend, legacyOption, value string
	}{
		{"docker", "sidecar_image", "mirror.example/agent:approved"},
		{"kubernetes", "sidecar_image", "mirror.example/agent:approved"},
		{"kubernetes", "coding_cli_sidecars", "{claude: mirror.example/claude:approved}"},
	} {
		t.Run(tc.backend+"/"+tc.legacyOption, func(t *testing.T) {
			resetCLIForTest()
			t.Cleanup(resetCLIForTest)
			path := filepath.Join(t.TempDir(), "config.yaml")
			content := fmt.Sprintf("worker_id: test-worker\nbackend:\n  %s:\n    %s: %s\n    sidecar_image_mappings:\n      warpdotdev/warp-xvfb-sidecar: mirror.example/computer-use\n", tc.backend, tc.legacyOption, tc.value)
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = mergeConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), "backend."+tc.backend+".sidecar_image_mappings") || !strings.Contains(err.Error(), tc.legacyOption) {
				t.Fatalf("startup error = %v, want mutually exclusive config error", err)
			}
		})
	}
}

func TestSidecarImageMappingsFromYAML(t *testing.T) {
	for _, backend := range []string{"docker", "kubernetes"} {
		t.Run(backend, func(t *testing.T) {
			resetCLIForTest()
			t.Cleanup(resetCLIForTest)
			path := filepath.Join(t.TempDir(), "config.yaml")
			content := fmt.Sprintf("worker_id: test-worker\nbackend:\n  %s:\n    sidecar_image_mappings:\n      warpdotdev/warp-agent: mirror.example/agent:approved\n", backend)
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			wc, err := mergeConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := wc.SidecarImageMappings.Remap("docker.io/warpdotdev/warp-agent:v1"); got != "mirror.example/agent:approved" {
				t.Fatalf("merged mapping = %q", got)
			}
			if backend == "docker" {
				cfg.Backend.Docker.SidecarImageMappings["warpdotdev/warp-agent"] = ""
			} else {
				cfg.Backend.Kubernetes.SidecarImageMappings["warpdotdev/warp-agent"] = ""
			}
			if _, err := mergeConfig(cfg); err == nil || !strings.Contains(err.Error(), "backend."+backend+".sidecar_image_mappings") {
				t.Fatalf("startup error = %v, want contextual mapping validation error", err)
			}
		})
	}
}
