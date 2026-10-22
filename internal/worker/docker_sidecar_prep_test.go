package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/client"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func newSidecarPrepBackend(t *testing.T, transport cancellationDockerTransport) *DockerBackend {
	t.Helper()
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	dockerClient, err := client.New(client.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dockerClient.Close() })
	return &DockerBackend{
		dockerClient: dockerClient,
		config:       DockerBackendConfig{ImagePullPolicy: PullPolicyNever},
		platform:     "linux/amd64",
	}
}

// inspectedImage extracts the image reference from an ImageInspect request path.
func inspectedImage(req *http.Request) (string, bool) {
	if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/json") {
		return "", false
	}
	_, rest, ok := strings.Cut(req.URL.Path, "/images/")
	if !ok {
		return "", false
	}
	name, err := url.PathUnescape(strings.TrimSuffix(rest, "/json"))
	if err != nil {
		return "", false
	}
	return name, true
}

func imageIDFor(image string) string {
	sum := sha256.Sum256([]byte(image))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestPrepareSidecarsRunsConcurrentlyInOrder(t *testing.T) {
	const sidecarCount = 6
	sidecars := make([]types.SidecarMount, 0, sidecarCount)
	for i := range sidecarCount {
		sidecars = append(sidecars, types.SidecarMount{
			Image:     fmt.Sprintf("example.com/sidecar-%d:v1", i),
			MountPath: fmt.Sprintf("/mnt/sidecar-%d", i),
		})
	}
	// The last two sidecars share an image and must resolve to the same volume.
	sidecars[sidecarCount-1].Image = sidecars[sidecarCount-2].Image

	var mu sync.Mutex
	inflight, maxInflight, arrived := 0, 0, 0
	gate := make(chan struct{})
	transport := cancellationDockerTransport(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Path == "/_ping":
			return mockEngineResponse(req, http.StatusOK, "text/plain", "OK"), nil
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/volumes/"):
			return mockEngineResponse(req, http.StatusOK, "application/json", `{}`), nil
		}
		image, ok := inspectedImage(req)
		if !ok {
			t.Errorf("unexpected Docker request: %s %s", req.Method, req.URL.Path)
			return mockEngineErrorResponse(t, req, http.StatusInternalServerError, "unexpected request"), nil
		}
		// Hold the first wave of inspections until sidecarPrepConcurrency callers
		// are blocked at once, which proves the preparations overlap; the errgroup
		// limit keeps the count from ever exceeding the bound.
		mu.Lock()
		inflight++
		arrived++
		if inflight > maxInflight {
			maxInflight = inflight
		}
		if arrived == sidecarPrepConcurrency {
			close(gate)
		}
		mu.Unlock()
		<-gate
		mu.Lock()
		inflight--
		mu.Unlock()
		return mockImageInspectResponse(t, req, "linux", "amd64", imageIDFor(image)), nil
	})
	backend := newSidecarPrepBackend(t, transport)

	binds, err := backend.prepareSidecars(t.Context(), backend.dockerClient, sidecars, nil)
	if err != nil {
		t.Fatal(err)
	}
	if maxInflight != sidecarPrepConcurrency {
		t.Fatalf("max concurrent sidecar preparations = %d, want %d", maxInflight, sidecarPrepConcurrency)
	}
	if len(binds) != sidecarCount {
		t.Fatalf("binds = %v, want %d entries", binds, sidecarCount)
	}
	for i, sidecar := range sidecars {
		wantVolume := sanitizeVolumeName(sidecar.Image, imageIDFor(sidecar.Image))
		if binds[i] != wantVolume+":"+sidecar.MountPath+":ro" {
			t.Fatalf("binds[%d] = %q, want volume %q mounted at %q", i, binds[i], wantVolume, sidecar.MountPath)
		}
	}
	sharedA, _, _ := strings.Cut(binds[sidecarCount-2], ":")
	sharedB, _, _ := strings.Cut(binds[sidecarCount-1], ":")
	if sharedA != sharedB {
		t.Fatalf("sidecars sharing an image used different volumes: %q vs %q", sharedA, sharedB)
	}
}

func TestPrepareSidecarsFirstErrorCancelsRest(t *testing.T) {
	sidecars := []types.SidecarMount{
		{Image: "example.com/failing:v1", MountPath: "/mnt/failing"},
		{Image: "example.com/slow:v1", MountPath: "/mnt/slow"},
	}
	slowStarted := make(chan struct{})
	var slowOnce sync.Once
	var mu sync.Mutex
	slowCancelled := false
	transport := cancellationDockerTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/_ping" {
			return mockEngineResponse(req, http.StatusOK, "text/plain", "OK"), nil
		}
		image, ok := inspectedImage(req)
		if !ok {
			t.Errorf("unexpected Docker request: %s %s", req.Method, req.URL.Path)
			return mockEngineErrorResponse(t, req, http.StatusInternalServerError, "unexpected request"), nil
		}
		switch image {
		case sidecars[0].Image:
			// Fail only once the other sidecar is blocked, so its cancellation can
			// only come from the failure propagating through the group.
			<-slowStarted
			return mockEngineErrorResponse(t, req, http.StatusInternalServerError, "engine unavailable"), nil
		default:
			slowOnce.Do(func() { close(slowStarted) })
			<-req.Context().Done()
			mu.Lock()
			slowCancelled = true
			mu.Unlock()
			return nil, req.Context().Err()
		}
	})
	backend := newSidecarPrepBackend(t, transport)

	_, err := backend.prepareSidecars(t.Context(), backend.dockerClient, sidecars, nil)
	if err == nil || !strings.Contains(err.Error(), sidecars[0].Image) || !strings.Contains(err.Error(), "failed to inspect local image") {
		t.Fatalf("error = %v, want the failing sidecar's inspection error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slowCancelled {
		t.Fatal("the remaining sidecar preparation was not cancelled after the first failure")
	}
}

func TestPrepareSidecarsValidatesBeforeContactingDocker(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	transport := cancellationDockerTransport(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		requests++
		mu.Unlock()
		return mockEngineErrorResponse(t, req, http.StatusInternalServerError, "unexpected request"), nil
	})
	backend := newSidecarPrepBackend(t, transport)

	for _, tc := range []struct {
		name      string
		sidecars  []types.SidecarMount
		wantError string
	}{
		{
			name:      "empty image",
			sidecars:  []types.SidecarMount{{Image: "example.com/a:v1", MountPath: "/mnt/a"}, {MountPath: "/mnt/b"}},
			wantError: "empty image",
		},
		{
			name:      "empty mount path",
			sidecars:  []types.SidecarMount{{Image: "example.com/a:v1", MountPath: "/mnt/a"}, {Image: "example.com/b:v1"}},
			wantError: "empty mount path",
		},
		{
			name:      "duplicate mount path",
			sidecars:  []types.SidecarMount{{Image: "example.com/a:v1", MountPath: "/mnt/a"}, {Image: "example.com/b:v1", MountPath: "/mnt/a"}},
			wantError: "duplicate mount path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := backend.prepareSidecars(context.Background(), backend.dockerClient, tc.sidecars, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want %q", err, tc.wantError)
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Fatalf("invalid sidecar lists reached Docker %d times", requests)
	}
}
