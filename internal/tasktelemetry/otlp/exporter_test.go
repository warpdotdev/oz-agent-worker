package otlp

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"golang.org/x/oauth2"
)

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
