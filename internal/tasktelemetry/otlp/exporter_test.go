package otlp

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"golang.org/x/oauth2"
)

func TestLogExportErrorStaysInWorkerLog(t *testing.T) {
	const diagnostic = "collector rejected the export: diagnostic detail"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		http.Error(w, diagnostic, http.StatusForbidden)
	}))
	defer server.Close()

	var workerOutput, taskOutput bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&workerOutput)
	t.Cleanup(func() { log.Logger = previous })
	ctx := zerolog.New(&taskOutput).WithContext(t.Context())
	config, err := NewExportConfig(ctx, "", &types.TaskAssignmentMessage{
		TelemetryCollection: &types.TelemetryCollectionConfig{
			Endpoint: server.URL,
			BootstrapToken: &types.TelemetryBootstrapToken{
				Token: "task-token", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339Nano),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	exporter, err := config.LogExporter()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exporter.Shutdown(context.Background()) }()
	var record sdklog.Record
	record.SetBody(attribute.StringValue("task output"))
	err = exporter.Export(ctx, []sdklog.Record{record})
	if err == nil || err.Error() != "task log export failed" {
		t.Fatalf("exporter returned an unsafe error: %v", err)
	}
	if taskOutput.Len() != 0 {
		t.Fatalf("exporter diagnostic reached the task logger: %s", &taskOutput)
	}
	var event struct {
		Level   string `json:"level"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(workerOutput.Bytes(), &event); err != nil {
		t.Fatalf("missing worker diagnostic: %v", err)
	}
	if event.Level != "error" || !strings.Contains(event.Error, diagnostic) || event.Message != "task log export failed" {
		t.Fatalf("worker diagnostic lost the original error: %+v", event)
	}
	if requests.Load() != 1 {
		t.Fatalf("exporter made %d requests, want one failed export", requests.Load())
	}
}

func TestHTTPSExport(t *testing.T) {
	for _, signal := range []string{"traces", "logs"} {
		t.Run(signal, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.TLS == nil || req.URL.Path != "/otlp/v1/"+signal || req.Header.Get("Authorization") != "Bearer task-token" {
					t.Error("export did not retain HTTPS, task routing and authentication")
				}
				requests.Add(1)
			}))
			defer server.Close()
			cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			certPath := filepath.Join(t.TempDir(), "operator-ca.pem")
			if err := os.WriteFile(certPath, cert, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", certPath)
			config, err := NewExportConfig(t.Context(), "", &types.TaskAssignmentMessage{
				TelemetryCollection: &types.TelemetryCollectionConfig{
					Endpoint: server.URL + "/otlp",
					BootstrapToken: &types.TelemetryBootstrapToken{
						Token: "task-token", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339Nano),
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var export func() error
			if signal == "traces" {
				exporter, err := config.TraceExporter()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = exporter.Shutdown(context.Background()) }()
				export = func() error {
					return exporter.ExportSpans(t.Context(), []sdktrace.ReadOnlySpan{
						tracetest.SpanStub{Name: "test", Resource: resource.Empty()}.Snapshot(),
					})
				}
			} else {
				exporter, err := config.LogExporter()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = exporter.Shutdown(context.Background()) }()
				var record sdklog.Record
				record.SetBody(attribute.StringValue("test"))
				export = func() error { return exporter.Export(t.Context(), []sdklog.Record{record}) }
			}
			if err := export(); err == nil || requests.Load() != 0 {
				t.Fatal("task export accepted an untrusted certificate from operator configuration")
			}
			// Trust only the fixture CA in the task client; verification remains enabled.
			config.client.Transport.(*oauth2.Transport).Base = server.Client().Transport
			if err := export(); err != nil || requests.Load() != 1 {
				t.Fatalf("HTTPS export with trusted CA: requests=%d err=%v", requests.Load(), err)
			}
		})
	}
}
