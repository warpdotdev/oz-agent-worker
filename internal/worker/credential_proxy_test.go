package worker

import (
	"archive/tar"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func testCredentialProxy() *types.CredentialProxy {
	return &types.CredentialProxy{
		Image: credentialProxyImage, ProxyToken: "synthetic-worker-only-capability",
		SessionToken: "agv_synthetic-session", ExpiresAt: time.Now().Add(time.Hour), Domain: "https://control.example",
	}
}

func TestCredentialProxyStandbyRejectedBeforeExecution(t *testing.T) {
	backend := newStandbyTestBackend(false)
	w := newStandbyTestWorker(t, backend, Config{})
	create := standbyCreate("sb-proxy")
	create.CredentialProxy = testCredentialProxy()
	w.handlePrewarmCreate(create)
	awaitStandbyMessage(t, w, types.MessageTypePrewarmRejected)
	if len(backend.executed) != 0 {
		t.Fatal("standby with proxy reached backend")
	}
}

func TestCredentialProxyAssignmentForwardingAndWire(t *testing.T) {
	p := testCredentialProxy()
	assignment := &types.TaskAssignmentMessage{TaskID: "task", ExecutionID: "exec", Task: &types.Task{ID: "task"}, CredentialProxy: p}
	raw, err := json.Marshal(assignment)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	var descriptor map[string]json.RawMessage
	if err := json.Unmarshal(parsed["credential_proxy"], &descriptor); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"image", "proxy_token", "session_token", "expires_at", "domain"} {
		if _, ok := descriptor[key]; !ok {
			t.Errorf("descriptor missing wire field %s", key)
		}
	}
	w := &Worker{ctx: context.Background(), config: Config{ServerRootURL: "https://control.example"}}
	if params := w.prepareTaskParams(assignment); params.CredentialProxy != p {
		t.Fatal("assignment proxy not forwarded to backend")
	}
	assignment.CredentialProxy = nil
	raw, err = json.Marshal(assignment)
	if err != nil || strings.Contains(string(raw), "credential_proxy") {
		t.Fatal("legacy task gained a proxy descriptor")
	}
}

func TestCredentialProxyHostReadinessOrigin(t *testing.T) {
	b := &DockerBackend{config: DockerBackendConfig{ServerRootURL: "http://localhost:8080"}}
	p := testCredentialProxy()
	p.Domain = "http://host.docker.internal:8080"
	got, err := b.credentialAdapterDomain(p)
	want := p.Domain
	if runtime.GOOS == "darwin" {
		want = "http://127.0.0.1:8080"
	}
	if err != nil || got.String() != want {
		t.Fatal("host readiness did not use matching loopback origin")
	}
	domain, err := credentialProxyDomain(p, b.config.ServerRootURL)
	if err != nil || domain.String() != p.Domain {
		t.Fatal("container origin was changed")
	}
	p.Domain = "http://host.docker.internal:9999"
	got, err = b.credentialAdapterDomain(p)
	want = p.Domain
	if runtime.GOOS == "darwin" {
		want = "http://127.0.0.1:9999"
	}
	if err != nil || got.String() != want {
		t.Fatal("readiness silently redirected capability to a different port")
	}
}

func TestDockerCredentialProxyAuthorityBounds(t *testing.T) {
	for _, mode := range []string{"session expiry", "adapter outage"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := &credentialProxyEngine{t: t, creates: make(map[string]proxyCreateSpec), archives: make(map[string]map[string][]byte), agentStarted: make(chan struct{}), proxyExit: make(chan struct{}), agentBlock: true}
				b := newStandbyDockerBackend(t, e)
				b.config.NoCleanup = true
				b.credentialProxyTransport = credentialProxyRoundTripper(func(r *http.Request) (*http.Response, error) {
					select {
					case <-e.agentStarted:
						if mode == "adapter outage" {
							return nil, errors.New("synthetic adapter outage")
						}
					default:
					}
					return mockEngineResponse(r, 200, "application/json", ""), nil
				})
				p := testCredentialProxy()
				if mode == "session expiry" {
					p.ExpiresAt = time.Now().Add(10 * time.Second)
				}
				start := time.Now()
				done := make(chan ExecuteResult, 1)
				go func() {
					done <- b.ExecuteTask(context.Background(), &TaskParams{TaskID: "task", ExecutionID: "exec", DockerImage: "task-image", CredentialProxy: p})
				}()
				<-e.agentStarted
				synctest.Wait()
				time.Sleep(20 * time.Second)
				synctest.Wait()
				select {
				case result := <-done:
					if result.Error == nil {
						t.Fatal("expired or disconnected authority completed successfully")
					}
				default:
					t.Fatal("cached proxy authority exceeded the 20 second tested bound")
				}
				if elapsed := time.Since(start); elapsed != 20*time.Second {
					t.Fatal("test did not use the controlled clock")
				}
				for _, resource := range []string{"/containers/proxy", "/containers/agent", "/networks/private-network"} {
					if !strings.Contains(strings.Join(e.removed, "\n"), resource) {
						t.Errorf("authority shutdown retained %s", resource)
					}
				}
			})
		})
	}
}

func TestCredentialProxyValidation(t *testing.T) {
	tests := []struct {
		name, backend, execution, standby string
		change                            func(*types.CredentialProxy)
		env                               []string
		valid                             bool
	}{
		{name: "Docker", backend: "docker", execution: "exec", valid: true},
		{name: "default Docker", execution: "exec", valid: true},
		{name: "direct", backend: "direct", execution: "exec"},
		{name: "Kubernetes", backend: "kubernetes", execution: "exec"},
		{name: "command", backend: "command", execution: "exec"},
		{name: "standby", execution: "exec", standby: "sb"},
		{name: "missing execution"},
		{name: "mutable image", execution: "exec", change: func(p *types.CredentialProxy) { p.Image = "infisical/cli:latest" }},
		{name: "missing private cap", execution: "exec", change: func(p *types.CredentialProxy) { p.ProxyToken = "" }},
		{name: "wrong session prefix", execution: "exec", change: func(p *types.CredentialProxy) { p.SessionToken = "wrong" }},
		{name: "same authority", execution: "exec", change: func(p *types.CredentialProxy) { p.ProxyToken = p.SessionToken }},
		{name: "expired", execution: "exec", change: func(p *types.CredentialProxy) { p.ExpiresAt = time.Now().Add(-time.Minute) }},
		{name: "plaintext remote", execution: "exec", change: func(p *types.CredentialProxy) { p.Domain = "http://remote.example" }},
		{name: "local development", execution: "exec", change: func(p *types.CredentialProxy) { p.Domain = "http://host.docker.internal:8080" }, valid: true},
		{name: "domain userinfo", execution: "exec", change: func(p *types.CredentialProxy) { p.Domain = "https://user@control.example" }},
		{name: "domain query", execution: "exec", change: func(p *types.CredentialProxy) { p.Domain += "?token=x" }},
		{name: "proxy env", execution: "exec", env: []string{"https_proxy=http://other"}},
		{name: "trust env", execution: "exec", env: []string{"SSL_CERT_FILE=/other"}},
		{name: "private cap in env", execution: "exec", env: []string{"OTHER=synthetic-worker-only-capability"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testCredentialProxy()
			if tt.change != nil {
				tt.change(p)
			}
			err := validateCredentialProxyAssignment(tt.backend, p, tt.execution, tt.standby, tt.env, "")
			if (err == nil) != tt.valid {
				t.Fatalf("validation success = %v, want %v", err == nil, tt.valid)
			}
		})
	}
	p := testCredentialProxy()
	p.Domain = ""
	u, err := credentialProxyDomain(p, "https://fallback.example")
	if err != nil || u.Host != "fallback.example" {
		t.Fatal("server root URL fallback was not used")
	}
	if err := validateCredentialProxyAssignment("direct", nil, "", "", []string{"HTTPS_PROXY=legacy"}, ""); err != nil {
		t.Fatal("legacy task changed")
	}
}

func TestCredentialProxyRejectsOtherBackendEntryPoints(t *testing.T) {
	for name, backend := range map[string]Backend{"direct": &DirectBackend{}, "kubernetes": &KubernetesBackend{}, "command": &CommandBackend{}} {
		t.Run(name, func(t *testing.T) {
			if result := backend.ExecuteTask(context.Background(), &TaskParams{CredentialProxy: testCredentialProxy()}); result.Error == nil {
				t.Fatal("backend accepted credential proxy")
			}
		})
	}
}

func TestCredentialProxyCAAndAgentEnvironment(t *testing.T) {
	p := testCredentialProxy()
	certPEM, keyPEM, err := newCredentialProxyCA(p.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA || cert.PublicKeyAlgorithm != x509.ECDSA {
		t.Fatal("expected an ECC certificate authority")
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if _, err := x509.ParseECPrivateKey(keyBlock.Bytes); err != nil {
		t.Fatal(err)
	}
	if cert.NotAfter.After(p.ExpiresAt) {
		t.Fatal("CA outlives descriptor")
	}
	env := credentialProxyAgentEnv(p, []string{"https://control.example", "wss://sharing.example/ws"})
	for _, entry := range env {
		if strings.Contains(entry, p.ProxyToken) {
			t.Fatal("private proxy cap leaked into agent environment")
		}
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"HTTP_PROXY=http://x-agent-vault:agv_synthetic-session@credential-proxy:17323", "https_proxy=", "control.example", "sharing.example", "CURL_CA_BUNDLE="} {
		if !strings.Contains(joined, want) {
			t.Errorf("agent env missing %s", want)
		}
	}
	if !strings.Contains(credentialProxyStartup, "/etc/ssl/certs/ca-certificates.crt") {
		t.Fatal("startup discards task system roots")
	}
}

type proxyCreateSpec struct {
	container.Config
	HostConfig container.HostConfig
}

type credentialProxyEngine struct {
	t                             *testing.T
	mu                            sync.Mutex
	creates                       map[string]proxyCreateSpec
	archives                      map[string]map[string][]byte
	removed, disconnected, starts []string
	failAgentStart                bool
	agentBlock                    bool
	agentStarted                  chan struct{}
	proxyExit                     chan struct{}
}

func (e *credentialProxyEngine) RoundTrip(r *http.Request) (*http.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	path := r.URL.Path
	switch {
	case path == "/_ping":
		return mockEngineResponse(r, 200, "text/plain", "OK"), nil
	case strings.Contains(path, "/images/"):
		return mockImageInspectResponse(e.t, r, "linux", "amd64", ""), nil
	case strings.HasSuffix(path, "/networks/create"):
		return mockEngineResponse(r, 201, "application/json", `{"Id":"private-network"}`), nil
	case strings.HasSuffix(path, "/containers/create"):
		var spec proxyCreateSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			return nil, err
		}
		id := "agent"
		if spec.Image == credentialProxyImage {
			id = "proxy"
		}
		e.creates[id] = spec
		return mockEngineResponse(r, 201, "application/json", `{"Id":"`+id+`"}`), nil
	case strings.HasSuffix(path, "/archive"):
		id := "agent"
		if strings.Contains(path, "/proxy/") {
			id = "proxy"
		}
		files := make(map[string][]byte)
		tr := tar.NewReader(r.Body)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			files[h.Name] = data
			if h.Name == "ca-key.pem" && h.Mode != 0600 {
				e.t.Error("proxy private key is not private")
			}
		}
		e.archives[id] = files
		return mockEngineResponse(r, 200, "application/json", ""), nil
	case strings.HasSuffix(path, "/exec"):
		return mockEngineResponse(r, 201, "application/json", `{"Id":"readiness"}`), nil
	case strings.HasSuffix(path, "/exec/readiness/json"):
		return mockEngineResponse(r, 200, "application/json", `{"Running":false,"ExitCode":0}`), nil
	case strings.HasSuffix(path, "/start"):
		if strings.Contains(path, "/agent/") {
			if e.failAgentStart {
				return mockEngineErrorResponse(e.t, r, 500, "synthetic start failure"), nil
			}
			e.starts = append(e.starts, "agent")
			close(e.agentStarted)
		} else if strings.Contains(path, "/proxy/") {
			e.starts = append(e.starts, "proxy")
		}
		return mockEngineResponse(r, 204, "application/json", ""), nil
	case strings.HasSuffix(path, "/wait"):
		isProxy := strings.Contains(path, "/proxy/")
		if isProxy || e.agentBlock {
			e.mu.Unlock()
			proxyExit := e.proxyExit
			if !isProxy {
				proxyExit = nil
			}
			select {
			case <-r.Context().Done():
				e.mu.Lock()
				return nil, r.Context().Err()
			case <-proxyExit:
				e.mu.Lock()
				return mockEngineResponse(r, 200, "application/json", `{"StatusCode":1}`), nil
			}
		}
		return mockEngineResponse(r, 200, "application/json", `{"StatusCode":0}`), nil
	case strings.HasSuffix(path, "/disconnect"):
		e.disconnected = append(e.disconnected, "agent")
		return mockEngineResponse(r, 200, "application/json", ""), nil
	case r.Method == http.MethodDelete:
		e.removed = append(e.removed, path)
		return mockEngineResponse(r, 204, "application/json", ""), nil
	default:
		e.t.Errorf("unexpected Docker request %s %s", r.Method, path)
		return mockEngineErrorResponse(e.t, r, 500, "unexpected Docker request"), nil
	}
}

func newCredentialProxyTest(t *testing.T) (*DockerBackend, *credentialProxyEngine, *TaskParams) {
	t.Helper()
	e := &credentialProxyEngine{t: t, creates: make(map[string]proxyCreateSpec), archives: make(map[string]map[string][]byte), agentStarted: make(chan struct{}), proxyExit: make(chan struct{})}
	b := newStandbyDockerBackend(t, e)
	p := testCredentialProxy()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent-vault/proxy/ready" || r.Header.Get("Authorization") != "Bearer "+p.ProxyToken {
			t.Error("readiness used wrong path or capability")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	p.Domain = server.URL
	return b, e, &TaskParams{TaskID: "task", ExecutionID: "exec", DockerImage: "task-image", CredentialProxy: p}
}

func TestDockerCredentialProxyResources(t *testing.T) {
	for _, noCleanup := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanup", true: "retain agent"}[noCleanup], func(t *testing.T) {
			b, e, params := newCredentialProxyTest(t)
			b.config.NoCleanup = noCleanup
			result := b.ExecuteTask(context.Background(), params)
			if result.Error != nil {
				t.Fatal(result.Error)
			}
			proxy, agent := e.creates["proxy"], e.creates["agent"]
			if proxy.Labels["warp.credential-proxy"] != "true" || proxy.Labels["oz-execution-id"] != "exec" || proxy.Labels["oz-task-id"] != "task" {
				t.Fatal("proxy missing execution labels")
			}
			if proxy.HostConfig.Memory != 256<<20 || proxy.HostConfig.NanoCPUs != 1e9 || len(proxy.HostConfig.PortBindings) != 0 || len(proxy.HostConfig.Binds) != 0 {
				t.Fatal("proxy resource isolation is incorrect")
			}
			if len(proxy.Env) != 2 || !strings.Contains(strings.Join(proxy.Env, "\n"), "INFISICAL_DOMAIN=") {
				t.Fatal("proxy has unexpected environment")
			}
			if proxy.HostConfig.NetworkMode != "private-network" || agent.HostConfig.NetworkMode != "private-network" {
				t.Fatal("execution does not use its own network")
			}
			if !reflect.DeepEqual(e.starts, []string{"proxy", "agent"}) {
				t.Fatal("agent started before proxy")
			}
			if strings.Contains(strings.Join(agent.Env, "\n"), params.CredentialProxy.ProxyToken) {
				t.Fatal("private capability leaked to agent")
			}
			if len(e.archives["proxy"]["ca-key.pem"]) == 0 || len(e.archives["agent"]["ca-key.pem"]) != 0 {
				t.Fatal("private CA key not isolated")
			}
			if len(e.archives["agent"]["oz-credential-proxy/ca.pem"]) == 0 {
				t.Fatal("agent has no public CA")
			}
			removed := strings.Join(e.removed, "\n")
			wantDisconnects := 0
			if noCleanup {
				wantDisconnects = 1
			}
			if !strings.Contains(removed, "/containers/proxy") || !strings.Contains(removed, "/networks/private-network") || len(e.disconnected) != wantDisconnects {
				t.Fatal("proxy/network retained")
			}
			if strings.Contains(removed, "/containers/agent") == noCleanup {
				t.Fatal("NoCleanup task retention changed")
			}
		})
	}
}

func TestDockerCredentialProxyFailureCleanup(t *testing.T) {
	for _, mode := range []string{"start failure", "cancel", "proxy exit"} {
		t.Run(mode, func(t *testing.T) {
			b, e, params := newCredentialProxyTest(t)
			b.config.NoCleanup = true
			e.failAgentStart = mode == "start failure"
			e.agentBlock = mode != "start failure"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan ExecuteResult, 1)
			go func() { done <- b.ExecuteTask(ctx, params) }()
			if mode != "start failure" {
				select {
				case <-e.agentStarted:
				case <-time.After(2 * time.Second):
					t.Fatal("agent did not start")
				}
				if mode == "cancel" {
					cancel()
				} else {
					close(e.proxyExit)
				}
			}
			select {
			case result := <-done:
				if result.Error == nil {
					t.Fatal("failure completed successfully")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("failure did not stop execution")
			}
			for _, want := range []string{"/containers/proxy", "/containers/agent", "/networks/private-network"} {
				if !strings.Contains(strings.Join(e.removed, "\n"), want) {
					t.Errorf("cleanup missed %s", want)
				}
			}
		})
	}
}

type credentialProxyRoundTripper func(*http.Request) (*http.Response, error)

func (f credentialProxyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCredentialProxyStartupPublicBundlePermissions(t *testing.T) {
	dir := t.TempDir()
	systemRoots := filepath.Join(dir, "system-roots.pem")
	proxyRoot := filepath.Join(dir, "proxy-root.pem")
	bundle := filepath.Join(dir, "combined-roots.pem")
	for path, content := range map[string]string{systemRoots: "synthetic system roots\n", proxyRoot: "synthetic public proxy CA\n"} {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	script := strings.NewReplacer(
		"/etc/ssl/certs/ca-certificates.crt", systemRoots,
		credentialProxyCAPath, proxyRoot,
		credentialProxyBundlePath, bundle,
	).Replace(credentialProxyStartup)
	if err := exec.CommandContext(context.Background(), "/bin/sh", "-c", script, "startup", "/usr/bin/true").Run(); err != nil {
		t.Fatalf("startup wrapper failed: %v", err)
	}
	info, err := os.Stat(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("public trust bundle mode = %o, want 0644 for access after a UID change", info.Mode().Perm())
	}
	contents, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "synthetic system roots\nsynthetic public proxy CA\n" {
		t.Fatal("startup did not preserve the system roots and public execution CA")
	}
}

func TestCredentialProxyReadinessDiagnosticsSafe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		client := proxyReadyClient()
		client.Transport = credentialProxyRoundTripper(func(r *http.Request) (*http.Response, error) {
			requests++
			if requests == 1 {
				return mockEngineResponse(r, http.StatusServiceUnavailable, "application/json", ""), nil
			}
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		domain, err := url.Parse("http://127.0.0.1:8080")
		if err != nil {
			t.Fatal(err)
		}
		err = waitCredentialAdapter(context.Background(), client, domain, "synthetic-private-proxy-token")
		if err == nil {
			t.Fatal("unavailable readiness succeeded")
		}
		for _, want := range []string{"origin http://127.0.0.1:8080", "last HTTP status 503", "HTTP responses 1", "last transport timeout"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("readiness diagnostic missing %q", want)
			}
		}
		if strings.Contains(err.Error(), "synthetic-private-proxy-token") {
			t.Fatal("readiness diagnostic exposed the capability")
		}
	})
	privateError := &url.Error{Op: "Get", URL: "https://private.invalid/?token=synthetic-private", Err: errors.New("secret raw transport detail")}
	if category := credentialProxyTransportCategory(privateError); category != "transport" {
		t.Fatalf("unexpected safe error category %q", category)
	}
}

func TestCredentialProxyReadyDoesNotFollowRedirect(t *testing.T) {
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	u, err := url.Parse(redirect.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := proxyReadyClient()
	defer c.CloseIdleConnections()
	if probeCredentialAdapter(context.Background(), c, u, "synthetic-cap") || reached {
		t.Fatal("readiness followed a capability-bearing redirect")
	}
}
