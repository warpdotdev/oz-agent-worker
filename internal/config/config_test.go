package config

import (
	"os"
	"path/filepath"
	"testing"
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

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
