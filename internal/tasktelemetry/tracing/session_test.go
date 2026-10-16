package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type spanExporter struct {
	mu        sync.Mutex
	spans     []sdktrace.ReadOnlySpan
	shutdowns atomic.Int32
}

func (e *spanExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *spanExporter) Shutdown(context.Context) error {
	e.shutdowns.Add(1)
	return nil
}

type flushProcessor struct {
	sdktrace.SpanProcessor
	flushes atomic.Int32
}

func (p *flushProcessor) ForceFlush(ctx context.Context) error {
	p.flushes.Add(1)
	return p.SpanProcessor.ForceFlush(ctx)
}

func installOperator(t *testing.T, exporter sdktrace.SpanExporter) (*flushProcessor, func(context.Context) error) {
	t.Helper()
	processor := &flushProcessor{SpanProcessor: sdktrace.NewBatchSpanProcessor(exporter)}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(processor))
	previous := operator.Swap(&operatorPipeline{processor: processor, resource: resource.NewSchemaless(
		attribute.String("service.version", "test-version"),
		attribute.String("operator.private", "local-only"),
	)})
	t.Cleanup(func() {
		operator.Store(previous)
		_ = provider.Shutdown(context.Background())
	})
	return processor, provider.Shutdown
}

type collectorServer struct {
	server   *httptest.Server
	mu       sync.Mutex
	payloads []*collector.ExportTraceServiceRequest
}

func newCollector(t *testing.T, bearer string) *collectorServer {
	t.Helper()
	c := &collectorServer{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/otlp/v1/traces" || req.Header.Get("Authorization") != "Bearer "+bearer {
			t.Error("trace collector received wrong route or task credentials")
		}
		data, _ := io.ReadAll(req.Body)
		payload := &collector.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(data, payload); err != nil {
			t.Error(err)
		}
		c.mu.Lock()
		c.payloads = append(c.payloads, payload)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(c.server.Close)
	return c
}

func traceAssignment(endpoint, run string) *types.TaskAssignmentMessage {
	return &types.TaskAssignmentMessage{
		TaskID: run, ExecutionID: "execution-" + run, Task: &types.Task{Title: "private title"},
		TelemetryCollection: &types.TelemetryCollectionConfig{
			Endpoint: endpoint + "/otlp", TracingEnabled: true,
			BootstrapToken: &types.TelemetryBootstrapToken{Token: "token-" + run, ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339Nano)},
		},
	}
}

func startSession(t *testing.T, assignment *types.TaskAssignmentMessage) (context.Context, *Session) {
	t.Helper()
	ctx, session, err := Start(t.Context(), "", "worker", "direct", assignment, time.Now())
	if err != nil || session == nil {
		t.Fatalf("start task tracing: %v", err)
	}
	t.Cleanup(func() {
		session.End("interrupted", nil)
		session.Shutdown(context.Background())
	})
	return ctx, session
}

func (c *collectorServer) spans() []*tracepb.Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	var spans []*tracepb.Span
	for _, payload := range c.payloads {
		for _, res := range payload.ResourceSpans {
			for _, scope := range res.ScopeSpans {
				spans = append(spans, scope.Spans...)
			}
		}
	}
	return spans
}

func TestTaskTraceDualExport(t *testing.T) {
	exporter := &spanExporter{}
	processor, shutdown := installOperator(t, exporter)
	warp := newCollector(t, "token-run")
	for range 2 {
		ctx, session := startSession(t, traceAssignment(warp.server.URL, "run"))
		StartPhase(ctx, "execution").End(nil)
		session.End("succeeded", nil)
		if err := session.ForceFlush(t.Context()); err != nil {
			t.Fatal(err)
		}
		session.Shutdown(t.Context())
		if exporter.shutdowns.Load() != 0 {
			t.Fatal("task closed the process-owned exporter")
		}
	}
	if processor.flushes.Load() != 4 {
		t.Fatal("task ForceFlush did not reach the operator processor")
	}
	spans := warp.spans()
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if len(spans) != 4 || len(exporter.spans) != 4 {
		t.Fatalf("dual export counts: warp=%d operator=%d", len(spans), len(exporter.spans))
	}
	for i, span := range spans {
		local := exporter.spans[i]
		sc := local.SpanContext()
		id, traceID, parent := sc.SpanID(), sc.TraceID(), local.Parent().SpanID()
		if !bytes.Equal(span.SpanId, id[:]) || !bytes.Equal(span.TraceId, traceID[:]) ||
			(local.Parent().IsValid() && !bytes.Equal(span.ParentSpanId, parent[:])) {
			t.Fatal("destinations received different IDs or parentage")
		}
		if local.Name() == "self_hosted_worker.execution" && !local.Parent().IsValid() {
			t.Fatal("phase was not attached to the task root")
		}
	}
	if err := shutdown(t.Context()); err != nil || exporter.shutdowns.Load() != 1 {
		t.Fatalf("process shutdown did not close exporter once: %v", err)
	}
}

func TestTaskTraceConcurrentIsolation(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "operator.secret=operator-only")
	installOperator(t, &spanExporter{})
	var collectors []*collectorServer
	var wg sync.WaitGroup
	for _, run := range []string{"alpha", "beta"} {
		c := newCollector(t, "token-"+run)
		collectors = append(collectors, c)
		ctx, session := startSession(t, traceAssignment(c.server.URL, run))
		wg.Go(func() {
			for range 64 {
				StartPhase(context.WithoutCancel(ctx), "execution").End(nil)
			}
			session.End("succeeded", nil)
			session.Shutdown(t.Context())
		})
	}
	wg.Wait()
	for i, run := range []string{"alpha", "beta"} {
		c := collectors[i]
		if len(c.spans()) != 65 {
			t.Fatalf("%s spans were lost or routed to the wrong collector", run)
		}
		c.mu.Lock()
		for _, payload := range c.payloads {
			for _, res := range payload.ResourceSpans {
				attrs := attributes(res.Resource.Attributes)
				if attrs["run_id"] != run || attrs["execution_id"] != "execution-"+run || attrs["operator.private"] != "" || attrs["operator.secret"] != "" {
					t.Error("task resource or operator metadata crossed the Warp boundary")
				}
			}
		}
		c.mu.Unlock()
	}
}

func attributes(attrs []*commonpb.KeyValue) map[string]string {
	result := make(map[string]string)
	for _, attr := range attrs {
		result[attr.Key] = attr.Value.GetStringValue()
	}
	return result
}

func TestTaskTraceAuthAndPrivacy(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	certificateServer := httptest.NewTLSServer(nil)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateServer.Certificate().Raw})
	certificateServer.Close()
	certificatePath := filepath.Join(t.TempDir(), "operator-ca.pem")
	if err := os.WriteFile(certificatePath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", certificatePath)
	for _, tc := range []struct {
		name             string
		bootstrap        *types.TelemetryBootstrapToken
		refresh          bool
		collectorFailure bool
		identityFailure  bool
	}{
		{name: "bootstrap", bootstrap: &types.TelemetryBootstrapToken{Token: "bootstrap-secret", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339Nano)}},
		{name: "refresh before expiry", bootstrap: &types.TelemetryBootstrapToken{Token: "bootstrap-secret", ExpiresAt: time.Now().Add(30 * time.Second).Format(time.RFC3339Nano)}, refresh: true},
		{name: "missing bootstrap", refresh: true},
		{name: "invalid bootstrap", bootstrap: &types.TelemetryBootstrapToken{Token: "bootstrap-secret", ExpiresAt: "invalid"}, refresh: true},
		{name: "identity failure", refresh: true, identityFailure: true},
		{name: "hostile collector", refresh: true, collectorFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var identities atomic.Int32
			var payload string
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/graphql/v2" {
					identities.Add(1)
					if req.Header.Get("Authorization") != "Bearer task-api-key" || req.Header.Get("X-Warp-Ambient-Workload-Token") != "task-workload-token" || req.Header.Get("X-Warp-Cloud-Agent-ID") != "run" {
						t.Error("identity refresh did not use task credentials")
					}
					if tc.identityFailure {
						http.Error(w, "sentinel-secret", http.StatusForbidden)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issueTaskIdentityToken": map[string]any{
						"__typename": "IssueTaskIdentityTokenOutput", "token": "refreshed-secret", "expiresAt": time.Now().Add(time.Hour),
					}}})
					return
				}
				want := "Bearer bootstrap-secret"
				if tc.refresh {
					want = "Bearer refreshed-secret"
				}
				if req.Header.Get("Authorization") != want {
					t.Error("incorrect task identity bearer")
				}
				if tc.collectorFailure {
					http.Error(w, "sentinel-secret "+req.Header.Get("Authorization"), http.StatusForbidden)
					return
				}
				data, _ := io.ReadAll(req.Body)
				var p collector.ExportTraceServiceRequest
				if err := proto.Unmarshal(data, &p); err != nil {
					t.Error(err)
				}
				text, _ := protojson.Marshal(&p)
				mu.Lock()
				payload += string(text)
				mu.Unlock()
			}))
			defer server.Close()
			a := traceAssignment(server.URL, "run")
			a.TelemetryCollection.BootstrapToken = tc.bootstrap
			if tc.refresh {
				a.EnvVars = map[string]string{"WARP_API_KEY": "task-api-key", "WARP_WORKLOAD_TOKEN": "task-workload-token"}
			}
			ctx, session, err := Start(t.Context(), server.URL, "worker", "direct", a, time.Now())
			if err != nil || session == nil {
				t.Fatalf("trace auth initialization: %v", err)
			}
			rawError := errors.New("raw backend error /task/path")
			StartPhase(ctx, "execution").End(rawError)
			session.End("failed", rawError)
			err = session.ForceFlush(t.Context())
			session.Shutdown(t.Context())
			if tc.identityFailure || tc.collectorFailure {
				if err == nil || err.Error() != "task trace export failed" {
					t.Fatalf("unsafe exporter/auth error: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				if !strings.Contains(payload, rawError.Error()) || strings.Contains(payload, a.Task.Title) || strings.Contains(payload, "task.title") {
					t.Error("task error was removed or title was exported")
				}
				mu.Unlock()
			}
			if tc.refresh != (identities.Load() > 0) {
				t.Fatal("bootstrap use or refresh timing changed")
			}
		})
	}
}

func TestTaskTraceBoundedShutdown(t *testing.T) {
	for _, blocked := range []string{"warp", "operator"} {
		t.Run(blocked, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				once.Do(func() { close(started) })
				<-release
			}))
			defer stalled.Close()
			defer close(release)
			good := newCollector(t, "token-run")
			var op *spanExporter
			warpEndpoint := good.server.URL
			if blocked == "warp" {
				op = &spanExporter{}
				installOperator(t, op)
				warpEndpoint = stalled.URL
			} else {
				t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
				t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf")
				t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", stalled.URL)
				prev := otel.GetTracerProvider()
				shutdown, err := InitOperator(t.Context(), resource.Empty())
				if err != nil {
					t.Fatal(err)
				}
				defer otel.SetTracerProvider(prev)
				defer func() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					_ = shutdown(ctx)
				}()
			}
			ctx, session := startSession(t, traceAssignment(warpEndpoint, "run"))
			for range 64 {
				StartPhase(ctx, "execution").End(nil)
			}
			flushCtx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			flushed := make(chan error, 1)
			go func() { flushed <- session.ForceFlush(flushCtx) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("blocked exporter did not start")
			}
			begin := time.Now()
			for range 2048 {
				StartPhase(ctx, "execution").End(nil)
			}
			if time.Since(begin) > time.Second {
				t.Fatal("queue overflow blocked task instrumentation")
			}
			<-flushed
			session.End("interrupted", nil)
			shutdownCtx, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
			begin = time.Now()
			session.Shutdown(shutdownCtx)
			stop()
			if time.Since(begin) > time.Second {
				t.Fatal("task shutdown exceeded its deadline")
			}
			if StartPhase(ctx, "cleanup") != nil {
				t.Fatal("late phase started after task closed")
			}
			if blocked == "operator" && len(good.spans()) == 0 {
				t.Fatal("operator stall prevented Warp export")
			}
			if op != nil {
				op.mu.Lock()
				got := len(op.spans)
				op.mu.Unlock()
				if got == 0 {
					t.Fatal("Warp stall prevented operator export")
				}
			}
		})
	}
}
