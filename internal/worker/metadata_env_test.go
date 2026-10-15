package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestMetadataEnvConflicts(t *testing.T) {
	t.Run("empty and case-insensitive configured variables", func(t *testing.T) {
		err := validateMetadataEnvConflicts([]string{"WARP_METADATA_TICKET_ID="}, []string{"warp_metadata_ticket_id=config"})
		if err == nil || !strings.Contains(err.Error(), "WARP_METADATA_TICKET_ID") {
			t.Fatalf("expected metadata conflict, got %v", err)
		}
	})
	t.Run("unrelated variables", func(t *testing.T) {
		if err := validateMetadataEnvConflicts([]string{"WARP_METADATA_TICKET_ID=value"}, []string{"OTHER=value"}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestDirectMetadataEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name      string
		config    map[string]string
		setup     string
		wantError bool
	}{
		{name: "available before setup", setup: `test "$WARP_METADATA_TICKET_ID" = "ABC-123" && test "${WARP_METADATA_EMPTY+x}" = x && test -z "$WARP_METADATA_EMPTY"`},
		{name: "configured collision", config: map[string]string{"WARP_METADATA_TICKET_ID": "configured"}, wantError: true},
		{name: "setup output collision", setup: `printf '%s\n' 'WARP_METADATA_TICKET_ID=changed' > "$OZ_ENVIRONMENT_FILE"`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			oz := filepath.Join(dir, "oz")
			if err := os.WriteFile(oz, []byte("#!/bin/sh\ntest \"$WARP_METADATA_TICKET_ID\" = ABC-123\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			backend, err := NewDirectBackend(context.Background(), DirectBackendConfig{
				OzPath: oz, WorkspaceRoot: filepath.Join(dir, "workspace"), Env: tc.config, SetupCommand: tc.setup,
			})
			if err != nil {
				t.Fatal(err)
			}
			result := backend.ExecuteTask(context.Background(), &TaskParams{
				TaskID: "metadata-test", EnvVars: []string{"WARP_METADATA_TICKET_ID=ABC-123", "WARP_METADATA_EMPTY="},
			})
			if (result.Error != nil) != tc.wantError {
				t.Fatalf("error = %v, wantError %v", result.Error, tc.wantError)
			}
			if tc.wantError && !strings.Contains(result.Error.Error(), "conflicts with configured environment") {
				t.Fatalf("unexpected error: %v", result.Error)
			}
		})
	}
}

func TestBackendsRejectMetadataConfigBeforeLaunch(t *testing.T) {
	t.Run("docker", func(t *testing.T) {
		backend := &DockerBackend{config: DockerBackendConfig{Env: map[string]string{"WARP_METADATA_KEY": ""}}}
		result := backend.ExecuteTask(context.Background(), &TaskParams{EnvVars: []string{"WARP_METADATA_KEY=value"}})
		if result.Error == nil || !strings.Contains(result.Error.Error(), "WARP_METADATA_KEY") {
			t.Fatalf("expected conflict before Docker API call, got %v", result.Error)
		}
	})
	t.Run("kubernetes", func(t *testing.T) {
		backend := &KubernetesBackend{config: KubernetesBackendConfig{TaskEnv: map[string]string{"WARP_METADATA_KEY": ""}}}
		result := backend.ExecuteTask(context.Background(), &TaskParams{EnvVars: []string{"WARP_METADATA_KEY=value"}})
		if result.Error == nil || !strings.Contains(result.Error.Error(), "WARP_METADATA_KEY") {
			t.Fatalf("expected conflict before Kubernetes API call, got %v", result.Error)
		}
	})
}

func TestKubernetesPodTemplateMetadataConflict(t *testing.T) {
	backend := &KubernetesBackend{config: KubernetesBackendConfig{PodTemplate: &corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: kubernetesTaskContainerName,
			Env: []corev1.EnvVar{{Name: "WARP_METADATA_KEY", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{Key: "key"},
			}}},
		}},
	}}}
	result := backend.ExecuteTask(context.Background(), &TaskParams{EnvVars: []string{"WARP_METADATA_KEY=value"}})
	if result.Error == nil || !strings.Contains(result.Error.Error(), "WARP_METADATA_KEY") {
		t.Fatalf("expected conflict before Kubernetes API call, got %v", result.Error)
	}
}

func TestKubernetesSetupOutputCannotOverwriteMetadata(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envFile, []byte("WARP_METADATA_EMPTY=changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := strings.Replace(kubernetesTaskWrapperScript([]string{"WARP_METADATA_EMPTY="}), `exec /agent/entrypoint.sh "$@"`, "exit 0", 1)
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "OZ_ENVIRONMENT_FILE="+envFile, "WARP_METADATA_EMPTY=")
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "WARP_METADATA_EMPTY") {
		t.Fatalf("expected readonly metadata failure, err=%v output=%s", err, output)
	}
}

func TestKubernetesMetadataValuesRemainLiteral(t *testing.T) {
	values := map[string]string{
		"WARP_METADATA_REFERENCE": "$$(TASK_ID)",
		"WARP_METADATA_DOLLARS":   "$$$$",
		"WARP_METADATA_EMPTY":     "",
		"WARP_METADATA_MULTILINE": "first\n$$(TASK_ID)=last",
		"WARP_METADATA_OPERATOR":  "$(TASK_ID)",
	}
	fakeClient := fake.NewSimpleClientset()
	var createdJob *batchv1.Job
	stop := errors.New("stop after job construction")
	fakeClient.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createdJob = action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		return true, nil, stop
	})
	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			Namespace: "agents", SetupCommand: "true",
			TaskEnv: map[string]string{"WARP_METADATA_OPERATOR": "$(TASK_ID)"},
		},
		clientset: fakeClient,
	}
	result := backend.ExecuteTask(context.Background(), &TaskParams{
		TaskID: "task-1", DockerImage: "image",
		EnvVars: []string{
			"WARP_METADATA_REFERENCE=$(TASK_ID)",
			"WARP_METADATA_DOLLARS=$$",
			"WARP_METADATA_EMPTY=",
			"WARP_METADATA_MULTILINE=first\n$(TASK_ID)=last",
		},
	})
	if !errors.Is(result.Error, stop) || createdJob == nil {
		t.Fatalf("expected constructed job, got %v", result.Error)
	}
	for _, container := range append(createdJob.Spec.Template.Spec.InitContainers, createdJob.Spec.Template.Spec.Containers...) {
		t.Run(container.Name, func(t *testing.T) {
			got := make(map[string]string)
			for _, env := range container.Env {
				got[env.Name] = env.Value
			}
			for name, want := range values {
				value, exists := got[name]
				if !exists || value != want {
					t.Errorf("%s = %q (present %v), want %q", name, value, exists, want)
				}
			}
		})
	}
}

func TestKubernetesSetupCanAssignNonMetadataNames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		taskEnv []string
	}{
		{name: "no metadata", taskEnv: []string{"UNRELATED=first\nWARP_METADATA_PHANTOM=x"}},
		{name: "with metadata", taskEnv: []string{"UNRELATED=first\nWARP_METADATA_PHANTOM=x", "WARP_METADATA_REAL=value"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envFile := filepath.Join(t.TempDir(), "env")
			if err := os.WriteFile(envFile, []byte("WARP_METADATA_PHANTOM=configured\nWARP_METADATA_OPERATOR=changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			script := strings.Replace(kubernetesTaskWrapperScript(tc.taskEnv), `exec /agent/entrypoint.sh "$@"`,
				`test "$WARP_METADATA_PHANTOM" = configured && test "$WARP_METADATA_OPERATOR" = changed`, 1)
			cmd := exec.Command("/bin/sh", "-c", script)
			cmd.Env = append(os.Environ(), tc.taskEnv...)
			cmd.Env = append(cmd.Env, "OZ_ENVIRONMENT_FILE="+envFile, "WARP_METADATA_OPERATOR=before")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("setup rejected an uninjected name: err=%v output=%s", err, output)
			}
		})
	}
}
