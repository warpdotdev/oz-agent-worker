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

	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
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
	r, err := New(server.URL, "worker", "direct", assignment(server.URL, "run"))
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
		r, err := New(server.URL, "worker", "direct", assignment(server.URL, runID))
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
		writer := r.Writer(t.Context(), "agent.stdout")
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
			r, err := New("https://server.example", "worker", "docker", a)
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
			r, err := New(server.URL, "worker", "docker", assignment(server.URL, "run"))
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
	for _, text := range []string{"first-secret", `quote\"secret`, "quote%22secret"} {
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
	s := newTokenSource(server.URL, "run", assignment(server.URL, "run").EnvVars)
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
	s := newTokenSource(server.URL, "run", assignment(server.URL, "run").EnvVars)
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
			s := newTokenSource(server.URL, "run", assignment(server.URL, "run").EnvVars)
			if _, err := s.Token(); err == nil || strings.Contains(err.Error(), "private credential") {
				t.Fatal("identity error response accepted or exposed")
			}
		})
	}
}
