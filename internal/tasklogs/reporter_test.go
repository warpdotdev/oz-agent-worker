package tasklogs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/attribute"
	collector "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

func assignment(endpoint, runID string) *types.TaskAssignmentMessage {
	return &types.TaskAssignmentMessage{
		TaskID: runID, ExecutionID: "execution-" + runID,
		EnvVars: map[string]string{"WARP_API_KEY": "key-" + runID, "WARP_WORKLOAD_TOKEN": "workload-" + runID},
		TelemetryCollection: &types.TelemetryCollectionConfig{
			Endpoint: endpoint + "/agent/otlp/", LoggingEnabled: true,
			BootstrapToken: &types.TelemetryBootstrapToken{Token: "trace-bootstrap"},
		},
	}
}

func TestAuthenticatedSameOriginRedirects(t *testing.T) {
	var identity, collectorCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/graphql/v2":
			http.Redirect(w, req, "/identity", http.StatusTemporaryRedirect)
		case "/identity":
			identity.Add(1)
			if req.Method != http.MethodPost || req.Header.Get("Authorization") != "Bearer key-run" ||
				req.Header.Get("X-Warp-Ambient-Workload-Token") != "workload-run" {
				t.Error("redirected identity request lost its method or credentials")
			}
			writeToken(w, "issued-token", time.Now().Add(time.Hour))
		case "/agent/otlp/v1/logs":
			http.Redirect(w, req, "/logs", http.StatusPermanentRedirect)
		case "/logs":
			collectorCalls.Add(1)
			if req.Method != http.MethodPost || req.Header.Get("Authorization") != "Bearer issued-token" {
				t.Error("redirected collector request lost its method or authorization")
			}
			var payload collector.ExportLogsServiceRequest
			data, _ := io.ReadAll(req.Body)
			if proto.Unmarshal(data, &payload) != nil || len(payload.ResourceLogs) != 1 {
				t.Error("redirected collector request lost its payload")
			}
		default:
			t.Errorf("unexpected redirected path: %s", req.URL.Path)
		}
	}))
	defer server.Close()
	r, err := New(t.Context(), server.URL, "worker", "direct", assignment(server.URL, "run"))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, r)
	r.Event(t.Context(), "setup", time.Now(), time.Now(), false)
	if err := r.provider.ForceFlush(t.Context()); err != nil || identity.Load() != 1 || collectorCalls.Load() != 1 {
		t.Fatalf("same-origin redirect failed: identity=%d collector=%d err=%v", identity.Load(), collectorCalls.Load(), err)
	}
}

func TestCollectorRedirectDoesNotForwardBearer(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/graphql/v2" {
			writeToken(w, "issued-token", time.Now().Add(time.Hour))
			return
		}
		http.Redirect(w, req, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	r, err := New(t.Context(), server.URL, "worker", "direct", assignment(server.URL, "run"))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, r)
	r.Event(t.Context(), "setup", time.Now(), time.Now(), false)
	if err := r.provider.ForceFlush(t.Context()); err == nil || redirected.Load() != 0 {
		t.Fatal("collector bearer credential was forwarded across origins")
	}
}

func TestCollectorErrorDoesNotExposeAuthorization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/graphql/v2" {
			writeToken(w, "private-log-token", time.Now().Add(time.Hour))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, req.Header.Get("Authorization"))
	}))
	defer server.Close()
	r, err := New(t.Context(), server.URL, "worker", "direct", assignment(server.URL, "run"))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, r)
	r.Event(t.Context(), "setup", time.Now(), time.Now(), false)
	err = r.provider.ForceFlush(t.Context())
	if err == nil || err.Error() != "task log export failed" {
		t.Fatalf("collector error was not sanitized: %v", err)
	}
}

func TestRedactionDoesNotRescanReplacementText(t *testing.T) {
	var r redactor
	for _, value := range []string{"R", "E", "D", "A", "C", "T", "[", "]"} {
		r.add(value)
	}
	if got := r.redact(strings.Repeat("R", 1024)); got != strings.Repeat("[REDACTED]", 1024) {
		t.Fatalf("replacement text was rescanned: %d bytes", len(got))
	}
	if got := r.redact(strings.Repeat("R", maxLineBytes)); got != "[REDACTED]" {
		t.Fatalf("expanded redaction exceeded the record budget: %d bytes", len(got))
	}
}

func writeToken(w http.ResponseWriter, token string, expiry time.Time) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issueTaskIdentityToken": map[string]any{
		"__typename": "IssueTaskIdentityTokenOutput", "token": token, "expiresAt": expiry.UTC().Format(time.RFC3339Nano),
	}}})
}

func shutdown(t *testing.T, r *Reporter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	r.Shutdown(ctx)
}

func TestOTLPTaskIsolationRedactionAndFinalFlush(t *testing.T) {
	var mu sync.Mutex
	var payloads []*collector.ExportLogsServiceRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/graphql/v2":
			runID := req.Header.Get("X-Warp-Cloud-Agent-ID")
			if req.Header.Get("Authorization") != "Bearer key-"+runID || req.Header.Get("X-Warp-Ambient-Workload-Token") != "workload-"+runID {
				t.Error("identity request did not use task credentials")
			}
			var body struct {
				Query     string
				Variables struct {
					Input struct {
						Audience string
						Duration int `json:"requestedDurationSeconds"`
					}
				}
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if !strings.Contains(body.Query, "osContext: {}, clientContext: {}") || body.Variables.Input.Audience != "warp-cloud-agent-otel" || body.Variables.Input.Duration != 10800 {
				t.Error("identity mutation does not match the server schema")
			}
			writeToken(w, "log-token-"+runID, time.Now().Add(time.Hour))
		case "/agent/otlp/v1/logs":
			if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer log-token-") {
				t.Error("collector used something other than the task identity token")
			}
			data, err := io.ReadAll(req.Body)
			if err != nil {
				t.Error(err)
			}
			payload := &collector.ExportLogsServiceRequest{}
			if err := proto.Unmarshal(data, payload); err != nil {
				t.Error(err)
			}
			for _, resource := range payload.ResourceLogs {
				runID := attributes(resource.Resource.Attributes)["run_id"].GetStringValue()
				if req.Header.Get("Authorization") != "Bearer log-token-"+runID {
					t.Error("collector authorization crossed task boundaries")
				}
			}
			mu.Lock()
			payloads = append(payloads, payload)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/x-protobuf")
		default:
			t.Errorf("unexpected endpoint %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	reporters := make([]*Reporter, 2)
	for i, runID := range []string{"alpha", "beta"} {
		r, err := New(t.Context(), server.URL, "worker", "direct", assignment(server.URL, runID))
		if err != nil {
			t.Fatal(err)
		}
		reporters[i] = r
		if runID == "beta" && r.redactor.redact("key-alpha") != "key-alpha" {
			t.Error("redaction state crossed task boundaries")
		}
		r.AddEnv(map[string]string{"CUSTOM_CREDENTIAL": "overlap-secret", "OTHER": "secret"})
		ctx := log.WithOutput(t.Context(), r.ZerologWriter(t.Context()))
		log.Warnf(ctx, "worker %s key-%s", runID, runID)
		oversized, _ := json.Marshal(map[string]string{"message": strings.Repeat("x", maxLineBytes) + "key-" + runID})
		_, _ = r.ZerologWriter(ctx).Write(oversized)
		writerAttrs := []attribute.KeyValue{attribute.String("test.stream", runID)}
		writer := r.Writer(t.Context(), "agent.stdout", writerAttrs...)
		writerAttrs[0] = attribute.String("test.stream", "changed")
		_, _ = writer.Write([]byte("output overlap-"))
		_, _ = writer.Write([]byte("secret key-" + runID + "\n"))
		_, _ = writer.Write([]byte(strings.Repeat("x", maxLineBytes) + "key-" + runID))
		_, _ = writer.Write([]byte("\nafter-overlong\nlast"))
		start := time.Now().Add(-time.Second)
		r.Event(t.Context(), "setup_worker_image_pull", start, time.Now(), true)
	}
	var wg sync.WaitGroup
	for _, r := range reporters {
		wg.Go(func() { shutdown(t, r) })
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, payload := range payloads {
		for _, resource := range payload.ResourceLogs {
			attrs := attributes(resource.Resource.Attributes)
			runID := attrs["run_id"].GetStringValue()
			if attrs["execution_id"].GetStringValue() != "execution-"+runID {
				t.Error("execution resource label missing")
			}
			seen[runID] = true
			var bodies []string
			for _, scope := range resource.ScopeLogs {
				for _, record := range scope.LogRecords {
					bodies = append(bodies, record.Body.GetStringValue())
					recordAttrs := attributes(record.Attributes)
					if recordAttrs["log.source"].GetStringValue() == "agent.stdout" {
						if recordAttrs["test.stream"].GetStringValue() != runID {
							t.Error("writer record attributes missing or changed after writer creation")
						}
					} else if recordAttrs["test.stream"] != nil {
						t.Error("writer attributes leaked to other streams")
					}
					if strings.HasPrefix(record.Body.GetStringValue(), "worker ") &&
						(record.Body.GetStringValue() != "worker "+runID+" [REDACTED]" || record.SeverityNumber != logspb.SeverityNumber_SEVERITY_NUMBER_WARN || record.SeverityText != "warn") {
						t.Error("task logger context or warning severity was lost")
					}
					if record.Body.GetStringValue() == "setup_worker_image_pull" && !attributes(record.Attributes)["is_error"].GetBoolValue() {
						t.Error("setup failure attribute missing")
					}
				}
			}
			text := strings.Join(bodies, "\n")
			for _, secret := range []string{"key-" + runID, "overlap-secret"} {
				if strings.Contains(text, secret) {
					t.Errorf("credential leaked for %s", runID)
				}
			}
			if !strings.Contains(text, "worker "+runID+" [REDACTED]") || !strings.Contains(text, "after-overlong") || !strings.Contains(text, "last") || strings.Contains(text, strings.Repeat("x", 100)) {
				t.Error("line bounds or unterminated-line flush broken")
			}
		}
	}
	if len(seen) != 2 {
		t.Fatalf("flushed %d tasks, want both", len(seen))
	}
}

func attributes(values []*commonpb.KeyValue) map[string]*commonpb.AnyValue {
	result := make(map[string]*commonpb.AnyValue)
	for _, value := range values {
		result[value.Key] = value.Value
	}
	return result
}

func TestStructuredEventsRetainMetadataAndRedactCompleteCredentials(t *testing.T) {
	var mu sync.Mutex
	var records []*logspb.LogRecord
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/graphql/v2" {
			writeToken(w, "issued-token", time.Now().Add(time.Hour))
			return
		}
		data, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
		}
		var payload collector.ExportLogsServiceRequest
		if err := proto.Unmarshal(data, &payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, resource := range payload.ResourceLogs {
			for _, scope := range resource.ScopeLogs {
				records = append(records, scope.LogRecords...)
			}
		}
	}))
	defer server.Close()
	root, cancel := context.WithCancel(t.Context())
	defer cancel()
	r, err := New(root, server.URL, "worker", "direct", assignment(server.URL, "run"))
	if err != nil {
		t.Fatal(err)
	}
	r.AddEnv(map[string]string{
		"MULTILINE": "first-secret\nsecond-secret",
		"QUOTED":    `quoted"<>&credential`,
	})
	logger := zerolog.New(r.ZerologWriter(t.Context())).With().Str("component", "backend").Logger()
	logger.Error().
		Str("phase", "first-secret").
		Str("error", `quoted"<>&credential`).
		Str("key-run", "redacted key").
		Uint64("sequence", 9007199254740993).
		Uint64("unsigned", 18446744073709551615).
		RawJSON("decimal", []byte("0.12345678901234567890123456789")).
		Interface("nested", map[string]any{
			"credential":           "first-secret\nsecond-secret",
			"retry":                true,
			`quoted"<>&credential`: "nested key",
			"items":                []any{1.25, nil, `quoted"<>&credential`},
		}).
		Msg("processing")
	_, _ = r.Writer(t.Context(), "agent.stdout").Write([]byte("first-secret\nsecond-secret\n"))
	logger.Info().
		Str("first", strings.Repeat("key-run ", 1200)).
		Str("second", strings.Repeat("key-run ", 1200)).
		Msg("oversized")
	cancel()
	if r.Context().Err() != context.Canceled {
		t.Error("reporter did not inherit worker-root cancellation")
	}
	shutdown(t, r)
	mu.Lock()
	defer mu.Unlock()
	if len(records) != 4 {
		t.Fatalf("exported %d records, want structured event, two raw lines, and masked event", len(records))
	}
	attrs := attributes(records[0].Attributes)
	nested := attributes(attrs["nested"].GetKvlistValue().GetValues())
	if records[0].Body.GetStringValue() != "processing" || attrs["message"] != nil ||
		attrs["component"].GetStringValue() != "backend" || attrs["phase"].GetStringValue() != "first-secret" ||
		attrs["error"].GetStringValue() != "[REDACTED]" || attrs["[REDACTED]"].GetStringValue() != "redacted key" ||
		attrs["sequence"].GetIntValue() != 9007199254740993 ||
		attrs["unsigned"].GetStringValue() != "18446744073709551615" ||
		attrs["decimal"].GetStringValue() != "0.12345678901234567890123456789" ||
		nested["credential"].GetStringValue() != "[REDACTED]" || !nested["retry"].GetBoolValue() ||
		nested["[REDACTED]"].GetStringValue() != "nested key" {
		t.Fatalf("structured metadata or credential redaction changed: %v", records[0])
	}
	items := nested["items"].GetArrayValue().GetValues()
	if len(items) != 3 || items[0].GetDoubleValue() != 1.25 || items[1].Value != nil ||
		items[2].GetStringValue() != "[REDACTED]" {
		t.Fatalf("nested array values changed: %v", items)
	}
	encoded, _ := proto.Marshal(records[0])
	for _, secret := range []string{"key-run", "first-secret\nsecond-secret", `quoted"<>&credential`} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("credential leaked in the structured body or attribute keys/values")
		}
	}
	if records[0].SeverityNumber != logspb.SeverityNumber_SEVERITY_NUMBER_ERROR || records[0].SeverityText != "error" {
		t.Fatal("structured error severity was lost")
	}
	for _, record := range records[1:3] {
		if record.Body.GetStringValue() != "[REDACTED]" ||
			attributes(record.Attributes)["log.source"].GetStringValue() != "agent.stdout" {
			t.Fatal("raw multiline credential fragments were not redacted")
		}
	}
	if records[3].Body.GetStringValue() != "[REDACTED]" || len(records[3].Attributes) != 0 {
		t.Fatal("structured body and attributes did not share the redaction output budget")
	}
}

func TestRedactionFragmentAlsoRegisteredAsCompleteValue(t *testing.T) {
	for _, values := range [][]string{
		{"first-secret\nsecond-secret", "first-secret"},
		{"first-secret", "first-secret\nsecond-secret"},
	} {
		var r redactor
		for _, value := range values {
			r.add(value)
		}
		if got := r.redact("first-secret"); got != "[REDACTED]" {
			t.Fatalf("complete credential was treated as a raw-only fragment: %q", got)
		}
	}
}

func TestNewLoggingGateAndCredentials(t *testing.T) {
	for _, tc := range []struct {
		name      string
		edit      func(*types.TaskAssignmentMessage)
		wantError bool
	}{
		{"omitted", func(a *types.TaskAssignmentMessage) { a.TelemetryCollection = nil }, false},
		{"tracing only", func(a *types.TaskAssignmentMessage) {
			a.TelemetryCollection.LoggingEnabled = false
			a.TelemetryCollection.TracingEnabled = true
		}, false},
		{"missing workload identity", func(a *types.TaskAssignmentMessage) { delete(a.EnvVars, "WARP_WORKLOAD_TOKEN") }, true},
		{"invalid collector", func(a *types.TaskAssignmentMessage) { a.TelemetryCollection.Endpoint = "file:///tmp/logs" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := assignment("https://collector.example", "run")
			tc.edit(a)
			r, err := New(t.Context(), "https://server.example", "worker", "docker", a)
			if r != nil || (err != nil) != tc.wantError {
				t.Fatalf("New = %v, %v", r, err)
			}
		})
	}
}

func TestSlowCollectorDoesNotBlockOutputAndShutdownHasDeadline(t *testing.T) {
	for _, stallIdentity := range []bool{false, true} {
		t.Run(map[bool]string{false: "collector", true: "identity"}[stallIdentity], func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/graphql/v2" && !stallIdentity {
					writeToken(w, "issued-token", time.Now().Add(time.Hour))
					return
				}
				once.Do(func() { close(started) })
				<-release
			}))
			defer server.Close()
			defer close(release)
			r, err := New(t.Context(), server.URL, "worker", "docker", assignment(server.URL, "run"))
			if err != nil {
				t.Fatal(err)
			}
			writer := r.Writer(t.Context(), "output")
			for range 128 {
				_, _ = writer.Write([]byte("line\n"))
			}
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("collector export did not start")
			}
			begin := time.Now()
			for range 4096 {
				_, _ = writer.Write([]byte("line\n"))
			}
			if time.Since(begin) > time.Second {
				t.Error("collector backpressure blocked task output")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			begin = time.Now()
			r.Shutdown(ctx)
			if time.Since(begin) > time.Second || r.Context().Err() == nil {
				t.Error("shutdown ignored deadline or failed to cancel streams")
			}
		})
	}
}

func TestRedactionVariantsAndOverflow(t *testing.T) {
	var r redactor
	r.add("first-secret\nsecond-secret")
	r.add(`quote"secret`)
	for _, text := range []string{"first-secret\nsecond-secret", `quote\"secret`, "quote%22secret"} {
		if got := r.redact(text); got != "[REDACTED]" {
			t.Errorf("redaction variant was not removed: %q", got)
		}
	}
	r.add(strings.Repeat("s", 1024*1024+1))
	if r.redact("unknown") != "[REDACTED]" {
		t.Error("redaction must fail closed on credential overflow")
	}
}

func TestIdentityLazyRefreshAndConcurrentReuse(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := calls.Add(1)
		expiry := time.Now().Add(time.Hour)
		if n == 1 {
			expiry = time.Now().Add(30 * time.Second)
		}
		writeToken(w, fmt.Sprintf("issued-%d", n), expiry)
	}))
	defer server.Close()
	s := newTokenSource(t.Context(), server.URL, "run", assignment(server.URL, "run").EnvVars)
	if calls.Load() != 0 {
		t.Fatal("token requested before export")
	}
	first, err := s.Token()
	if err != nil || first.AccessToken != "issued-1" {
		t.Fatalf("initial token unavailable: %v", err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			token, err := s.Token()
			if err != nil || token.AccessToken != "issued-2" {
				t.Errorf("concurrent token refresh failed: %v", err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("made %d requests, want one initial request and one refresh", calls.Load())
	}
}

func TestIdentityRedirectDoesNotForwardCredentials(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	s := newTokenSource(t.Context(), server.URL, "run", assignment(server.URL, "run").EnvVars)
	if _, err := s.Token(); err == nil || redirected.Load() != 0 {
		t.Fatal("identity credentials were forwarded through a redirect")
	}
}

func TestIdentityResponseValidation(t *testing.T) {
	for _, body := range []string{
		`{"data":{"issueTaskIdentityToken":{"__typename":"UserFacingError"}}}`,
		`{"errors":[{"message":"private credential"}]}`,
		`{"data":{"issueTaskIdentityToken":{"__typename":"IssueTaskIdentityTokenOutput","token":"expired","expiresAt":"2000-01-01T00:00:00Z"}}}`,
		`{"data":`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			s := newTokenSource(t.Context(), server.URL, "run", assignment(server.URL, "run").EnvVars)
			if _, err := s.Token(); err == nil || strings.Contains(err.Error(), "private credential") {
				t.Fatal("identity error response accepted or exposed")
			}
		})
	}
}
