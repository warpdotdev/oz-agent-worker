package types

import (
	"encoding/json"
	"testing"
)

func TestTaskTelemetryAssignment(t *testing.T) {
	var assignment TaskAssignmentMessage
	err := json.Unmarshal([]byte(`{"task_id":"run","execution_id":"execution","telemetry_collection":{"endpoint":"https://collector.example/agent/otlp","logging_enabled":true,"tracing_enabled":false,"bootstrap_token":{"token":"tracing-only","expires_at":"2026-10-06T20:00:00Z"}}}`), &assignment)
	if err != nil {
		t.Fatal(err)
	}
	config := assignment.TelemetryCollection
	if config == nil || !config.LoggingEnabled || config.TracingEnabled || config.BootstrapToken.Token != "tracing-only" {
		t.Fatalf("telemetry assignment did not decode: %+v", config)
	}
}

func TestSidecarMountFallbackImageStaysOffTheWire(t *testing.T) {
	data, err := json.Marshal(SidecarMount{Image: "acp", MountPath: "/mnt/x", FallbackImage: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"image":"acp","mount_path":"/mnt/x","read_write":false}` {
		t.Fatalf("marshaled %s; the fallback must not be serialized", data)
	}
	var mount SidecarMount
	if err := json.Unmarshal([]byte(`{"image":"acp","mount_path":"/mnt/x","FallbackImage":"a","fallback_image":"b"}`), &mount); err != nil {
		t.Fatal(err)
	}
	if mount.FallbackImage != "" {
		t.Fatalf("unmarshaled FallbackImage %q; the server must not be able to set it", mount.FallbackImage)
	}
}
