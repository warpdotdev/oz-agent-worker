package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	return path
}

// The Helm chart renders exactly this shape, so parsing it here keeps the chart
// and the worker's schema from drifting apart.
const chartRenderedConfig = `
worker_id: "ci-worker"
cleanup: true
max_concurrent_tasks: 0
idle_on_complete: "90m"
debug_log_capture:
  directory: "/var/lib/oz/debug-logs"
  max_total_bytes: 1073741824
  max_execution_bytes: 67108864
  max_concurrent_uploads: 2
backend:
  kubernetes:
    namespace: "agents"
    ttl_seconds_after_finished: 7200
`

func TestLoadParsesTheChartRenderedDebugLogCaptureBlock(t *testing.T) {
	cfg, err := Load(writeConfig(t, chartRenderedConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	capture := cfg.DebugLogCapture
	if capture == nil {
		t.Fatal("debug_log_capture was not parsed")
	}
	if capture.Directory != "/var/lib/oz/debug-logs" {
		t.Errorf("directory = %q, want the chart's capture root", capture.Directory)
	}
	if capture.MaxTotalBytes == nil || *capture.MaxTotalBytes != 1073741824 {
		t.Errorf("max_total_bytes = %v, want 1073741824", capture.MaxTotalBytes)
	}
	if capture.MaxExecutionBytes == nil || *capture.MaxExecutionBytes != 67108864 {
		t.Errorf("max_execution_bytes = %v, want 67108864", capture.MaxExecutionBytes)
	}
	if capture.MaxConcurrentUploads == nil || *capture.MaxConcurrentUploads != 2 {
		t.Errorf("max_concurrent_uploads = %v, want 2", capture.MaxConcurrentUploads)
	}

	// Retention comes from the existing cleanup grace, so the block must carry
	// no duration of its own.
	if cfg.IdleOnComplete == nil || *cfg.IdleOnComplete != "90m" {
		t.Errorf("idle_on_complete = %v, want 90m", cfg.IdleOnComplete)
	}
}

func TestLoadOmittingDebugLogCaptureLeavesItUnset(t *testing.T) {
	cfg, err := Load(writeConfig(t, "worker_id: \"w-1\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DebugLogCapture != nil {
		t.Fatalf("debug_log_capture = %+v, want it unset so defaults apply", cfg.DebugLogCapture)
	}
}

func TestLoadRejectsAnArchiveRetentionSetting(t *testing.T) {
	// Retention is deliberately not configurable here: strict field parsing is
	// what stops an operator from silently setting a second cleanup clock.
	_, err := Load(writeConfig(t, `
worker_id: "w-1"
debug_log_capture:
  retention: "2h"
`))
	if err == nil {
		t.Fatal("expected an unknown debug_log_capture field to be rejected")
	}
	if !strings.Contains(err.Error(), "retention") {
		t.Fatalf("error = %v, want it to name the unknown field", err)
	}
}
