package tracing

import "testing"

func TestShouldInitTracesRequiresExporter(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "")
	if shouldInitTraces() {
		t.Fatal("expected empty OTEL_TRACES_EXPORTER to disable traces")
	}

	t.Setenv("OTEL_TRACES_EXPORTER", " none ")
	if shouldInitTraces() {
		t.Fatal("expected OTEL_TRACES_EXPORTER=none to disable traces")
	}

	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	if !shouldInitTraces() {
		t.Fatal("expected non-none OTEL_TRACES_EXPORTER to enable traces")
	}
}
