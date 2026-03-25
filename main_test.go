package main

import (
	"testing"
	"time"

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
	CLI.IdleOnComplete = ""
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
