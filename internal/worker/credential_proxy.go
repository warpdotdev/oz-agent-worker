package worker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

const credentialProxyImage = "infisical/cli:0.45.8@sha256:ef5cd9bef770e1d13acfa18414a65719a262c4b7681735086416852f1574d019"
const credentialProxyCAPath = "/tmp/oz-credential-proxy/ca.pem"
const credentialProxyBundlePath = "/tmp/oz-credential-proxy-roots.pem"

func credentialProxyRedactions(proxy *types.CredentialProxy) map[string]string {
	return map[string]string{
		"PROXY_TOKEN":        proxy.ProxyToken,
		"SESSION_TOKEN":      proxy.SessionToken,
		"SESSION_BASIC_AUTH": base64.StdEncoding.EncodeToString([]byte("x-agent-vault:" + proxy.SessionToken)),
	}
}

func credentialProxyDomain(proxy *types.CredentialProxy, fallback string) (*url.URL, error) {
	domain := proxy.Domain
	if domain == "" {
		domain = fallback
	}
	u, err := url.Parse(domain)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid credential proxy domain")
	}
	ip := net.ParseIP(u.Hostname())
	local := u.Hostname() == "host.docker.internal" || u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && (u.Scheme != "http" || !local) {
		return nil, errors.New("credential proxy domain requires HTTPS except for explicit local development hosts")
	}
	u.Path = ""
	return u, nil
}

// Docker Desktop's host alias resolves in containers, but not on the host.
// Native macOS workers use the same local port; container workers keep the alias.
func (b *DockerBackend) credentialAdapterDomain(proxy *types.CredentialProxy) (*url.URL, error) {
	domain, err := credentialProxyDomain(proxy, b.config.ServerRootURL)
	if err != nil {
		return nil, err
	}
	if domain.Scheme == "http" && domain.Hostname() == "host.docker.internal" {
		if runtime.GOOS == "darwin" {
			local := *domain
			local.Host = "127.0.0.1"
			if domain.Port() != "" {
				local.Host = net.JoinHostPort("127.0.0.1", domain.Port())
			}
			return &local, nil
		}
	}
	return domain, nil
}

func (b *DockerBackend) credentialAdapterClient() *http.Client {
	c := proxyReadyClient()
	if b.credentialProxyTransport != nil {
		c.Transport = b.credentialProxyTransport
	}
	return c
}

func validateCredentialProxyAssignment(backend string, proxy *types.CredentialProxy, executionID, standbyID string, env []string, fallback string) error {
	if proxy == nil {
		return nil
	}
	if backend != "" && backend != "docker" {
		return errors.New("credential proxy requires the Docker backend")
	}
	if standbyID != "" || executionID == "" {
		return errors.New("credential proxy requires a task execution, not a standby")
	}
	if proxy.Image != credentialProxyImage || proxy.ProxyToken == "" || !strings.HasPrefix(proxy.SessionToken, "agv_") ||
		len(proxy.SessionToken) <= 4 || proxy.ProxyToken == proxy.SessionToken || !proxy.ExpiresAt.After(time.Now()) {
		return errors.New("invalid credential proxy descriptor")
	}
	for _, token := range []string{proxy.ProxyToken, proxy.SessionToken} {
		if len(token) > 8192 || strings.ContainsAny(token, " \t\r\n\x00") {
			return errors.New("invalid credential proxy credential")
		}
	}
	if _, err := credentialProxyDomain(proxy, fallback); err != nil {
		return err
	}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "GIT_SSL_CAINFO", "INFISICAL_AGENT_VAULT_PROXY_TOKEN", "INFISICAL_DOMAIN":
			return errors.New("credential proxy conflicts with a reserved environment variable")
		}
		if strings.Contains(value, proxy.ProxyToken) {
			return errors.New("worker-only proxy credential cannot be in the task environment")
		}
	}
	return nil
}

// The CA key exists only in worker memory and the proxy's private filesystem.
func newCredentialProxyCA(expires time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "Oz execution credential proxy"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: expires,
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

func credentialProxyArchive(files map[string][]byte) (*bytes.Reader, error) {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	if _, ok := files["oz-credential-proxy/ca.pem"]; ok {
		if err := tw.WriteHeader(&tar.Header{Name: "oz-credential-proxy/", Mode: 0755, Typeflag: tar.TypeDir}); err != nil {
			return nil, err
		}
	}
	for name, data := range files {
		mode := int64(0644)
		if name == "ca-key.pem" {
			mode = 0600
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(out.Bytes()), nil
}

type credentialProxyResources struct {
	mu                          sync.Mutex
	networkID, proxyID, agentID string
}

func (b *DockerBackend) cleanupCredentialProxy(ctx context.Context, key taskExecution) error {
	value, ok := b.credentialProxies.Load(key)
	if !ok {
		return nil
	}
	r := value.(*credentialProxyResources)
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	if r.proxyID != "" {
		if _, err := b.dockerClient.ContainerRemove(ctx, r.proxyID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, errors.New("credential proxy removal failed"))
		} else {
			r.proxyID = ""
		}
	}
	if r.networkID != "" {
		if r.agentID != "" {
			_, err := b.dockerClient.NetworkDisconnect(ctx, r.networkID, client.NetworkDisconnectOptions{Container: r.agentID, Force: true})
			if err != nil && !cerrdefs.IsNotFound(err) {
				errs = append(errs, errors.New("credential proxy network disconnect failed"))
			}
		}
		if _, err := b.dockerClient.NetworkRemove(ctx, r.networkID, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, errors.New("credential proxy network removal failed"))
		} else {
			r.networkID = ""
		}
	}
	if len(errs) == 0 {
		b.credentialProxies.Delete(key)
	}
	return errors.Join(errs...)
}

// No redirect or ambient forward proxy may receive the worker-only capability.
func proxyReadyClient() *http.Client {
	return &http.Client{
		Timeout:       3 * time.Second,
		Transport:     &http.Transport{},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func probeCredentialAdapter(ctx context.Context, httpClient *http.Client, domain *url.URL, token string) bool {
	status := probeCredentialAdapterStatus(ctx, httpClient, domain, token)
	return status == http.StatusOK
}

func probeCredentialAdapterStatus(ctx context.Context, httpClient *http.Client, domain *url.URL, token string) int {
	status, _ := probeCredentialAdapterDiagnostic(ctx, httpClient, domain, token)
	return status
}

func probeCredentialAdapterDiagnostic(ctx context.Context, httpClient *http.Client, domain *url.URL, token string) (int, string) {
	u := *domain
	u.Path = "/api/v1/agent-vault/proxy/ready"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, "request"
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, credentialProxyTransportCategory(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, ""
}

func credentialProxyTransportCategory(err error) string {
	var dns *net.DNSError
	var network *net.OpError
	var timeout net.Error
	var certificate x509.UnknownAuthorityError
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &dns):
		return "dns"
	case errors.As(err, &certificate):
		return "tls_authority"
	case errors.As(err, &timeout) && timeout.Timeout():
		return "timeout"
	case errors.As(err, &network):
		return "network"
	default:
		return "transport"
	}
}

func waitCredentialAdapter(ctx context.Context, httpClient *http.Client, domain *url.URL, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lastHTTPStatus, responses, failures := 0, 0, 0
	lastTransport := "none"
	for {
		status, transport := probeCredentialAdapterDiagnostic(ctx, httpClient, domain, token)
		if status == http.StatusOK {
			return nil
		}
		if status != 0 {
			lastHTTPStatus = status
			responses++
		} else {
			lastTransport = transport
			failures++
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("credential proxy adapter did not become ready (origin %s://%s; last HTTP status %d; HTTP responses %d; transport failures %d; last transport %s)", domain.Scheme, domain.Host, lastHTTPStatus, responses, failures, lastTransport)
		case <-ticker.C:
		}
	}
}

func (b *DockerBackend) startCredentialProxy(ctx context.Context, params *TaskParams, r *credentialProxyResources) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	proxy := params.CredentialProxy
	domain, err := credentialProxyDomain(proxy, b.config.ServerRootURL)
	if err != nil {
		return nil, err
	}
	httpClient := b.credentialAdapterClient()
	defer httpClient.CloseIdleConnections()
	probeDomain, err := b.credentialAdapterDomain(proxy)
	if err != nil {
		return nil, err
	}
	if err := waitCredentialAdapter(ctx, httpClient, probeDomain, proxy.ProxyToken); err != nil {
		return nil, err
	}
	cert, privateKey, err := newCredentialProxyCA(proxy.ExpiresAt)
	if err != nil {
		return nil, errors.New("credential proxy CA generation failed")
	}
	labels := map[string]string{"oz-task-id": params.TaskID, "oz-execution-id": params.ExecutionID, "warp.credential-proxy": "true"}
	r.networkID = "oz-proxy-" + uuid.NewString()
	netResult, err := b.dockerClient.NetworkCreate(ctx, r.networkID, client.NetworkCreateOptions{Driver: "bridge", Labels: labels})
	if err != nil {
		return nil, errors.New("credential proxy network creation failed")
	}
	r.networkID = netResult.ID
	if err := b.prepareImage(ctx, proxy.Image, b.getRegistryAuth(ctx, proxy.Image), nil); err != nil {
		return nil, errors.New("credential proxy image preparation failed")
	}
	r.proxyID = "oz-proxy-" + uuid.NewString()
	created, err := b.dockerClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: r.proxyID,
		Config: &container.Config{
			Image: proxy.Image, Labels: labels,
			Env: []string{"INFISICAL_DOMAIN=" + domain.String(), "INFISICAL_AGENT_VAULT_PROXY_TOKEN=" + proxy.ProxyToken},
			Cmd: []string{"agent-vault", "proxy", "--telemetry=false", "--listen-host", "0.0.0.0", "--ca-cert", "/tmp/ca.pem", "--ca-key", "/tmp/ca-key.pem"},
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(r.networkID),
			Resources:   container.Resources{Memory: 256 << 20, MemorySwap: 256 << 20, NanoCPUs: 1_000_000_000},
			CapDrop:     []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
		},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{r.networkID: {Aliases: []string{"credential-proxy"}}}},
	})
	if err != nil {
		return nil, errors.New("credential proxy container creation failed")
	}
	r.proxyID = created.ID
	archive, err := credentialProxyArchive(map[string][]byte{"ca.pem": cert, "ca-key.pem": privateKey})
	if err != nil {
		return nil, err
	}
	if _, err := b.dockerClient.CopyToContainer(ctx, r.proxyID, client.CopyToContainerOptions{DestinationPath: "/tmp", Content: archive}); err != nil {
		return nil, errors.New("credential proxy CA delivery failed")
	}
	if _, err := b.dockerClient.ContainerStart(ctx, r.proxyID, client.ContainerStartOptions{}); err != nil {
		return nil, errors.New("credential proxy start failed")
	}
	if err := b.waitCredentialProxyListener(ctx, r.proxyID, proxy.SessionToken); err != nil {
		return nil, err
	}
	return cert, nil
}

func (b *DockerBackend) waitCredentialProxyListener(ctx context.Context, id, session string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		exec, err := b.dockerClient.ExecCreate(ctx, id, client.ExecCreateOptions{
			Cmd: []string{"/bin/infisical", "agent-vault", "run", "--no-skills", "--no-mcp-servers", "--telemetry=false", "--", "/bin/true"},
			Env: []string{"INFISICAL_AGENT_VAULT_SESSION_TOKEN=" + session, "INFISICAL_AGENT_VAULT_PROXY_ADDRESS=127.0.0.1:17323"},
		})
		if err == nil {
			_, err = b.dockerClient.ExecStart(ctx, exec.ID, client.ExecStartOptions{Detach: true})
			if err == nil {
				for {
					inspect, inspectErr := b.dockerClient.ExecInspect(ctx, exec.ID, client.ExecInspectOptions{})
					if inspectErr != nil || !inspect.Running {
						if inspectErr == nil && inspect.ExitCode == 0 {
							return nil
						}
						break
					}
					select {
					case <-ctx.Done():
						return errors.New("credential proxy readiness probe timed out")
					case <-ticker.C:
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("credential proxy listener did not become ready")
		case <-ticker.C:
		}
	}
}

func credentialProxyAgentEnv(proxy *types.CredentialProxy, controlURLs []string) []string {
	proxyURL := (&url.URL{Scheme: "http", Host: "credential-proxy:17323", User: url.UserPassword("x-agent-vault", proxy.SessionToken)}).String()
	noProxy := []string{"localhost", "127.0.0.1", "::1", "app.warp.dev", "staging-app.warp.dev", "oz.warp.dev", "api.warp.dev"}
	for _, raw := range controlURLs {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			noProxy = append(noProxy, u.Hostname())
		}
	}
	return []string{
		"HTTP_PROXY=" + proxyURL, "HTTPS_PROXY=" + proxyURL, "http_proxy=" + proxyURL, "https_proxy=" + proxyURL,
		"NO_PROXY=" + strings.Join(noProxy, ","), "no_proxy=" + strings.Join(noProxy, ","),
		"SSL_CERT_FILE=" + credentialProxyBundlePath, "CURL_CA_BUNDLE=" + credentialProxyBundlePath,
		"REQUESTS_CA_BUNDLE=" + credentialProxyBundlePath, "NODE_EXTRA_CA_CERTS=" + credentialProxyCAPath,
		"GIT_SSL_CAINFO=" + credentialProxyBundlePath,
	}
}

// Preserve the task image's system roots instead of replacing them with our CA.
const credentialProxyStartup = `set -eu
umask 077
cat /etc/ssl/certs/ca-certificates.crt /tmp/oz-credential-proxy/ca.pem > /tmp/oz-credential-proxy-roots.pem
chmod 0644 /tmp/oz-credential-proxy-roots.pem
exec "$@"
`

func (b *DockerBackend) monitorCredentialProxy(ctx context.Context, cancel context.CancelCauseFunc, proxy *types.CredentialProxy, id string) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	httpClient := b.credentialAdapterClient()
	defer httpClient.CloseIdleConnections()
	domain, err := b.credentialAdapterDomain(proxy)
	if err != nil {
		cancel(errors.New("invalid credential proxy domain"))
		return
	}
	exited := make(chan struct{}, 1)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		wait := b.dockerClient.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
		select {
		case <-ctx.Done():
		case <-wait.Result:
			exited <- struct{}{}
		case err := <-wait.Error:
			if err != nil && ctx.Err() != nil {
				return
			}
			exited <- struct{}{}
		}
	}()
	defer func() {
		stop()
		<-watchDone
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-exited:
			cancel(errors.New("credential proxy exited"))
			return
		case <-ticker.C:
			if probeCredentialAdapter(ctx, httpClient, domain, proxy.ProxyToken) {
				failures = 0
			} else {
				failures++
				if failures >= 3 {
					cancel(errors.New("credential proxy adapter unavailable"))
					return
				}
			}
		}
	}
}

func unsupportedCredentialProxy(_ *TaskParams) ExecuteResult {
	return executeError(errors.New("credential proxy is not supported by this backend"))
}
