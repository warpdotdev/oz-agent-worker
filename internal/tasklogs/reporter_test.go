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

	"github.com/warpdotdev/oz-agent-worker/internal/types"
	collector "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
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
		if _, _, err := r.source.get(t.Context()); err != nil {
			t.Fatal(err)
		}
		r.AddEnv(map[string]string{"CUSTOM_CREDENTIAL": "overlap-secret", "OTHER": "secret"})
		r.Log(t.Context(), "warn", "worker key-"+runID+" log-token-"+runID+" trace-bootstrap")
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
					if record.Body.GetStringValue() == "setup_worker_image_pull" && !attributes(record.Attributes)["is_error"].GetBoolValue() {
						t.Error("setup failure attribute missing")
					}
				}
			}
			text := strings.Join(bodies, "\n")
			for _, secret := range []string{"key-" + runID, "log-token-" + runID, "trace-bootstrap", "overlap-secret"} {
				if strings.Contains(text, secret) {
					t.Errorf("credential leaked for %s", runID)
				}
			}
			if !strings.Contains(text, "after-overlong") || !strings.Contains(text, "last") || strings.Contains(text, strings.Repeat("x", 100)) {
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
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/graphql/v2" {
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
		t.Error("shutdown ignored deadline or failed to stop refresh")
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

func TestIdentityRefreshAndExpiredToken(t *testing.T) {
	var calls atomic.Int32
	var reject atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := calls.Add(1)
		if reject.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeToken(w, fmt.Sprintf("issued-%d", n), time.Now().Add(time.Hour))
	}))
	defer server.Close()
	var redactor redactor
	s := newTokenSource(server.URL, "run", assignment(server.URL, "run").EnvVars, &redactor)
	first, _, err := s.get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.refreshAt = time.Now().Add(-time.Second)
	second, _, err := s.get(t.Context())
	if err != nil || first == second || redactor.redact(first+" "+second) != "[REDACTED] [REDACTED]" {
		t.Fatal("refresh or retained-token redaction failed")
	}
	reject.Store(true)
	s.refreshAt = time.Now().Add(-time.Second)
	if token, _, err := s.get(t.Context()); err != nil || token != second {
		t.Fatal("failed refresh discarded valid token")
	}
	s.expiresAt, s.refreshAt = time.Now().Add(-time.Second), time.Now().Add(-time.Second)
	if _, _, err := s.get(t.Context()); err == nil {
		t.Fatal("expired token was reused")
	}
}

func TestProactiveIdentityRefresh(t *testing.T) {
	renewed := make(chan bool, 1)
	var expiry atomic.Int64
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			firstExpiry := time.Now().Add(1500 * time.Millisecond)
			expiry.Store(firstExpiry.UnixNano())
			writeToken(w, "first", firstExpiry)
			return
		}
		renewed <- time.Now().UnixNano() < expiry.Load()
		writeToken(w, "second", time.Now().Add(time.Hour))
	}))
	defer server.Close()
	var redactor redactor
	s := newTokenSource(server.URL, "run", assignment(server.URL, "run").EnvVars, &redactor)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.run(ctx)
	select {
	case beforeExpiry := <-renewed:
		if !beforeExpiry {
			t.Error("refresh occurred after token expiry")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle reporter did not refresh")
	}
	cancel()
	<-s.done
}

func TestIdentityRedirectDoesNotForwardCredentials(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	s := newTokenSource(server.URL, "run", assignment(server.URL, "run").EnvVars, &redactor{})
	if _, _, err := s.get(t.Context()); err == nil || redirected.Load() != 0 {
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
			s := newTokenSource(server.URL, "run", assignment(server.URL, "run").EnvVars, &redactor{})
			if _, _, err := s.get(t.Context()); err == nil || strings.Contains(err.Error(), "private credential") {
				t.Fatal("identity error response accepted or exposed")
			}
		})
	}
}
