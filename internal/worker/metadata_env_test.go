package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
	script := strings.Replace(kubernetesTaskWrapperScript(), `exec /agent/entrypoint.sh "$@"`, "exit 0", 1)
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "OZ_ENVIRONMENT_FILE="+envFile, "WARP_METADATA_EMPTY=")
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "WARP_METADATA_EMPTY") {
		t.Fatalf("expected readonly metadata failure, err=%v output=%s", err, output)
	}
}
