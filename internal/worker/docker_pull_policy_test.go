package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/common"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// mockEngineResponse builds a scripted *http.Response for the fake Docker Engine, mirroring the
// shape the moby/moby client package expects (a Request reference and a readable Body).
func mockEngineResponse(req *http.Request, statusCode int, contentType, body string) *http.Response {
	return &http.Response{
		Status:     fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		StatusCode: statusCode,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// mockEngineErrorResponse builds an Engine error response in the JSON shape the moby/moby client
// decodes into a typed error (e.g. a 404 becomes an error satisfying cerrdefs.IsNotFound).
func mockEngineErrorResponse(t *testing.T, req *http.Request, statusCode int, message string) *http.Response {
	t.Helper()
	body, err := json.Marshal(common.ErrorResponse{Message: message})
	if err != nil {
		t.Fatalf("failed to marshal engine error response: %v", err)
	}
	return mockEngineResponse(req, statusCode, "application/json", string(body))
}

// mockImageInspectResponse builds a successful ImageInspect response reporting the given
// platform and (optionally) image ID, used as the digest fallback by getImageDigest.
func mockImageInspectResponse(t *testing.T, req *http.Request, os, architecture, id string) *http.Response {
	t.Helper()
	body, err := json.Marshal(image.InspectResponse{Os: os, Architecture: architecture, ID: id})
	if err != nil {
		t.Fatalf("failed to marshal image inspect response: %v", err)
	}
	return mockEngineResponse(req, http.StatusOK, "application/json", string(body))
}

// fakeDockerEngine is a scripted double for the subset of the Docker Engine API that image
// preparation exercises: image inspect, image pull, and (for sidecar tests) volume inspect. It
// records every non-negotiation call it handles, in order, so tests can assert on Engine call
// ordering without needing a real Docker daemon.
type fakeDockerEngine struct {
	t *testing.T

	// inspectResponses is consumed in call order for ImageInspect; once exhausted, the last
	// entry repeats for any further calls (e.g. the digest lookup after a successful prepare).
	inspectResponses []func(req *http.Request) *http.Response
	inspectCalls     int

	// pullResponse answers every ImagePull call; nil means no pull is expected.
	pullResponse func(req *http.Request) *http.Response
	// containerCreateResponse lets ExecuteTask tests stop after image preparation without
	// exercising the rest of the container lifecycle.
	containerCreateResponse func(req *http.Request) *http.Response

	// volumeFound controls the VolumeInspect response used by sidecar tests, so they can reuse
	// an existing volume and avoid exercising the (unrelated, unchanged) volume-copy machinery.
	volumeFound bool

	calls []string
}

func (e *fakeDockerEngine) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case req.URL.Path == "/_ping":
		// Docker API version negotiation happens transparently on the first request; answer it
		// without recording a call so assertions only see the calls under test.
		return mockEngineResponse(req, http.StatusOK, "text/plain", "OK"), nil
	case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/images/") && strings.HasSuffix(req.URL.Path, "/json"):
		e.calls = append(e.calls, "inspect")
		idx := e.inspectCalls
		if idx >= len(e.inspectResponses) {
			idx = len(e.inspectResponses) - 1
		}
		e.inspectCalls++
		if idx < 0 {
			e.t.Fatalf("unexpected ImageInspect call: no responses scripted")
		}
		return e.inspectResponses[idx](req), nil
	case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/images/create"):
		e.calls = append(e.calls, "pull")
		if e.pullResponse == nil {
			e.t.Fatalf("unexpected ImagePull call: no response scripted")
		}
		return e.pullResponse(req), nil
	case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/containers/create"):
		e.calls = append(e.calls, "container_create")
		if e.containerCreateResponse == nil {
			e.t.Fatalf("unexpected ContainerCreate call: no response scripted")
		}
		return e.containerCreateResponse(req), nil
	case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/volumes/"):
		e.calls = append(e.calls, "volume_inspect")
		if e.volumeFound {
			return mockEngineResponse(req, http.StatusOK, "application/json", `{"Name":"existing"}`), nil
		}
		return mockEngineErrorResponse(e.t, req, http.StatusNotFound, "no such volume"), nil
	default:
		e.t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		return nil, nil
	}
}

// newTestDockerBackend builds a DockerBackend whose Docker client talks to engine instead of a
// real daemon, fixed to a linux/amd64 platform (matching the platform used in mocked responses).
func newTestDockerBackend(t *testing.T, engine *fakeDockerEngine, policy string) *DockerBackend {
	t.Helper()
	dockerClient, err := client.New(client.WithHTTPClient(&http.Client{Transport: engine}))
	if err != nil {
		t.Fatalf("failed to build test docker client: %v", err)
	}
	return &DockerBackend{
		config:       DockerBackendConfig{ImagePullPolicy: policy},
		dockerClient: dockerClient,
		platform:     "linux/amd64",
		platformSpec: ocispec.Platform{OS: "linux", Architecture: "amd64"},
	}
}

func TestPrepareImagePullPolicy(t *testing.T) {
	const imageName = "example.com/task-image:v1"

	pullOK := func(req *http.Request) *http.Response {
		return mockEngineResponse(req, http.StatusOK, "text/plain", "pulling image...\n")
	}

	t.Run("Always pulls unconditionally", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t:            t,
			pullResponse: pullOK,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response { return mockImageInspectResponse(t, req, "linux", "amd64", "") },
			},
		}
		backend := newTestDockerBackend(t, engine, PullPolicyAlways)
		if err := backend.prepareImage(context.Background(), imageName, "", nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantCalls := []string{"pull", "inspect"}
		if !reflect.DeepEqual(engine.calls, wantCalls) {
			t.Fatalf("engine calls = %v, want %v", engine.calls, wantCalls)
		}
	})

	t.Run("empty policy defaults to Always", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t:            t,
			pullResponse: pullOK,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response { return mockImageInspectResponse(t, req, "linux", "amd64", "") },
			},
		}
		backend := newTestDockerBackend(t, engine, "")
		if err := backend.prepareImage(context.Background(), imageName, "", nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantCalls := []string{"pull", "inspect"}
		if !reflect.DeepEqual(engine.calls, wantCalls) {
			t.Fatalf("engine calls = %v, want %v (omitted policy must default to Always)", engine.calls, wantCalls)
		}
	})

	t.Run("IfNotPresent reuses the local image when present", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t: t,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response { return mockImageInspectResponse(t, req, "linux", "amd64", "") },
			},
		}
		backend := newTestDockerBackend(t, engine, PullPolicyIfNotPresent)
		if err := backend.prepareImage(context.Background(), imageName, "", nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantCalls := []string{"inspect"}
		if !reflect.DeepEqual(engine.calls, wantCalls) {
			t.Fatalf("engine calls = %v, want %v (no pull expected)", engine.calls, wantCalls)
		}
	})

	t.Run("IfNotPresent pulls when the image is missing locally", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t: t,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response {
					return mockEngineErrorResponse(t, req, http.StatusNotFound, "no such image")
				},
				func(req *http.Request) *http.Response { return mockImageInspectResponse(t, req, "linux", "amd64", "") },
			},
			pullResponse: pullOK,
		}
		backend := newTestDockerBackend(t, engine, PullPolicyIfNotPresent)
		if err := backend.prepareImage(context.Background(), imageName, "", nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantCalls := []string{"inspect", "pull", "inspect"}
		if !reflect.DeepEqual(engine.calls, wantCalls) {
			t.Fatalf("engine calls = %v, want %v", engine.calls, wantCalls)
		}
	})

	t.Run("IfNotPresent propagates a non-not-found inspect error without pulling", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t: t,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response {
					return mockEngineErrorResponse(t, req, http.StatusInternalServerError, "engine unavailable")
				},
			},
		}
		backend := newTestDockerBackend(t, engine, PullPolicyIfNotPresent)
		err := backend.prepareImage(context.Background(), imageName, "", nil)
		if err == nil {
			t.Fatal("expected an error")
		}
		wantCalls := []string{"inspect"}
		if !reflect.DeepEqual(engine.calls, wantCalls) {
			t.Fatalf("engine calls = %v, want %v (no pull on a non-not-found inspect error)", engine.calls, wantCalls)
		}
	})

	t.Run("Never uses the local image when present", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t: t,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response { return mockImageInspectResponse(t, req, "linux", "amd64", "") },
			},
		}
		backend := newTestDockerBackend(t, engine, PullPolicyNever)
		if err := backend.prepareImage(context.Background(), imageName, "", nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantCalls := []string{"inspect"}
		if !reflect.DeepEqual(engine.calls, wantCalls) {
			t.Fatalf("engine calls = %v, want %v", engine.calls, wantCalls)
		}
	})

	t.Run("Never fails clearly and never contacts the registry when the image is missing", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t: t,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response {
					return mockEngineErrorResponse(t, req, http.StatusNotFound, "no such image")
				},
			},
		}
		backend := newTestDockerBackend(t, engine, PullPolicyNever)
		err := backend.prepareImage(context.Background(), imageName, "", nil)
		if err == nil || !strings.Contains(err.Error(), "pull policy is Never") {
			t.Fatalf("error = %v, want a message naming pull policy Never", err)
		}
		wantCalls := []string{"inspect"}
		if !reflect.DeepEqual(engine.calls, wantCalls) {
			t.Fatalf("engine calls = %v, want %v (Never must never pull)", engine.calls, wantCalls)
		}
	})

	t.Run("platform mismatch fails after an Always pull", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t:            t,
			pullResponse: pullOK,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response { return mockImageInspectResponse(t, req, "linux", "arm64", "") },
			},
		}
		backend := newTestDockerBackend(t, engine, PullPolicyAlways)
		err := backend.prepareImage(context.Background(), imageName, "", nil)
		if err == nil || !strings.Contains(err.Error(), "is for platform linux/arm64") {
			t.Fatalf("error = %v, want a platform mismatch error", err)
		}
	})

	t.Run("platform mismatch fails for a locally reused image under IfNotPresent", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t: t,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response { return mockImageInspectResponse(t, req, "linux", "arm64", "") },
			},
		}
		backend := newTestDockerBackend(t, engine, PullPolicyIfNotPresent)
		err := backend.prepareImage(context.Background(), imageName, "", nil)
		if err == nil || !strings.Contains(err.Error(), "is for platform linux/arm64") {
			t.Fatalf("error = %v, want a platform mismatch error", err)
		}
	})

	t.Run("pull failure surfaces the engine error", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t: t,
			pullResponse: func(req *http.Request) *http.Response {
				return mockEngineErrorResponse(t, req, http.StatusInternalServerError, "registry unavailable")
			},
		}
		backend := newTestDockerBackend(t, engine, PullPolicyAlways)
		err := backend.prepareImage(context.Background(), imageName, "", nil)
		if err == nil || !strings.Contains(err.Error(), "failed to pull image") {
			t.Fatalf("error = %v, want a pull failure error", err)
		}
	})
}

func TestExecuteTaskReportsImagePullOnlyWhenPullRuns(t *testing.T) {
	const imageName = "example.com/task-image:v1"

	pullOK := func(req *http.Request) *http.Response {
		return mockEngineResponse(req, http.StatusOK, "text/plain", "pulling image...\n")
	}
	containerCreateFailure := func(req *http.Request) *http.Response {
		return mockEngineErrorResponse(t, req, http.StatusInternalServerError, "stop after image preparation")
	}
	newReporter := func(t *testing.T) (*setupEventReporter, *capturedSetupEvents) {
		t.Helper()
		captured := &capturedSetupEvents{events: make(map[string]clientEventRequest)}
		server := httptest.NewServer(captured.handler())
		t.Cleanup(server.Close)
		reporter := newSetupEventReporter(server.URL, &types.TaskAssignmentMessage{
			TaskID:  "task-123",
			EnvVars: map[string]string{warpAPIKeyEnv: "api-key"},
		})
		return reporter, captured
	}
	execute := func(t *testing.T, engine *fakeDockerEngine, policy string) (*capturedSetupEvents, ExecuteResult) {
		t.Helper()
		reporter, captured := newReporter(t)
		backend := newTestDockerBackend(t, engine, policy)
		result := backend.ExecuteTask(context.Background(), &TaskParams{
			TaskID:      "task-123",
			DockerImage: imageName,
			SetupEvents: reporter,
		})
		return captured, result
	}
	assertNoPullEvent := func(t *testing.T, captured *capturedSetupEvents) {
		t.Helper()
		time.Sleep(50 * time.Millisecond)
		if _, ok := captured.get(SetupEventImagePull); ok {
			t.Fatal("unexpected image pull setup event when ImagePull was not called")
		}
	}

	t.Run("Always reports a successful pull", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t:                       t,
			pullResponse:            pullOK,
			containerCreateResponse: containerCreateFailure,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response {
					return mockImageInspectResponse(t, req, "linux", "amd64", "")
				},
			},
		}
		captured, result := execute(t, engine, PullPolicyAlways)
		if result.Error == nil {
			t.Fatal("expected the scripted container-create failure")
		}
		captured.waitForEvents(t, 2)
		event, ok := captured.get(SetupEventImagePull)
		if !ok {
			t.Fatal("missing image pull setup event")
		}
		if event.Payload.IsError {
			t.Fatal("image pull setup event is_error = true, want false")
		}
	})

	t.Run("IfNotPresent reports a pull when the image is missing", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t:                       t,
			pullResponse:            pullOK,
			containerCreateResponse: containerCreateFailure,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response {
					return mockEngineErrorResponse(t, req, http.StatusNotFound, "no such image")
				},
				func(req *http.Request) *http.Response {
					return mockImageInspectResponse(t, req, "linux", "amd64", "")
				},
			},
		}
		captured, result := execute(t, engine, PullPolicyIfNotPresent)
		if result.Error == nil {
			t.Fatal("expected the scripted container-create failure")
		}
		captured.waitForEvents(t, 2)
		if _, ok := captured.get(SetupEventImagePull); !ok {
			t.Fatal("missing image pull setup event")
		}
	})

	t.Run("IfNotPresent does not report a pull when reusing a local image", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t:                       t,
			containerCreateResponse: containerCreateFailure,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response {
					return mockImageInspectResponse(t, req, "linux", "amd64", "")
				},
			},
		}
		captured, result := execute(t, engine, PullPolicyIfNotPresent)
		if result.Error == nil {
			t.Fatal("expected the scripted container-create failure")
		}
		captured.waitForEvents(t, 1)
		assertNoPullEvent(t, captured)
	})

	t.Run("Never does not report a pull when using a local image", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t:                       t,
			containerCreateResponse: containerCreateFailure,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response {
					return mockImageInspectResponse(t, req, "linux", "amd64", "")
				},
			},
		}
		captured, result := execute(t, engine, PullPolicyNever)
		if result.Error == nil {
			t.Fatal("expected the scripted container-create failure")
		}
		captured.waitForEvents(t, 1)
		assertNoPullEvent(t, captured)
	})

	t.Run("inspection failure before a pull does not report a pull", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t: t,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response {
					return mockEngineErrorResponse(t, req, http.StatusInternalServerError, "engine unavailable")
				},
			},
		}
		captured, result := execute(t, engine, PullPolicyIfNotPresent)
		if result.Error == nil {
			t.Fatal("expected image inspection to fail")
		}
		assertNoPullEvent(t, captured)
	})

	t.Run("Never missing-image failure does not report a pull", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t: t,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response {
					return mockEngineErrorResponse(t, req, http.StatusNotFound, "no such image")
				},
			},
		}
		captured, result := execute(t, engine, PullPolicyNever)
		if result.Error == nil {
			t.Fatal("expected the missing local image to fail")
		}
		assertNoPullEvent(t, captured)
	})

	t.Run("local platform mismatch does not report a pull", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t: t,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response {
					return mockImageInspectResponse(t, req, "linux", "arm64", "")
				},
			},
		}
		captured, result := execute(t, engine, PullPolicyIfNotPresent)
		if result.Error == nil {
			t.Fatal("expected the local platform mismatch to fail")
		}
		assertNoPullEvent(t, captured)
	})

	t.Run("pull failure reports a failed pull", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t: t,
			pullResponse: func(req *http.Request) *http.Response {
				return mockEngineErrorResponse(t, req, http.StatusInternalServerError, "registry unavailable")
			},
		}
		captured, result := execute(t, engine, PullPolicyAlways)
		if result.Error == nil {
			t.Fatal("expected image pull to fail")
		}
		captured.waitForEvents(t, 1)
		event, ok := captured.get(SetupEventImagePull)
		if !ok {
			t.Fatal("missing failed image pull setup event")
		}
		if !event.Payload.IsError {
			t.Fatal("image pull setup event is_error = false, want true")
		}
	})
}
func TestPrepareSidecarsAppliesImagePullPolicy(t *testing.T) {
	const sidecarImage = "example.com/sidecar-image:v1"
	sidecars := []types.SidecarMount{{Image: sidecarImage, MountPath: "/mnt/sidecar"}}

	t.Run("IfNotPresent pulls the sidecar image only when it is missing locally", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t:           t,
			volumeFound: true, // Skip the (unrelated, unchanged) volume-copy machinery.
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response {
					return mockEngineErrorResponse(t, req, http.StatusNotFound, "no such image")
				},
				func(req *http.Request) *http.Response {
					return mockImageInspectResponse(t, req, "linux", "amd64", "sha256:deadbeefcafebabe0000")
				},
			},
			pullResponse: func(req *http.Request) *http.Response {
				return mockEngineResponse(req, http.StatusOK, "text/plain", "pulling image...\n")
			},
		}
		backend := newTestDockerBackend(t, engine, PullPolicyIfNotPresent)

		binds, err := backend.prepareSidecars(context.Background(), backend.dockerClient, sidecars)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(binds) != 1 {
			t.Fatalf("binds = %v, want 1 entry", binds)
		}
		// inspect (missing) -> pull -> inspect (post-pull platform check) -> inspect (digest
		// lookup for the volume name) -> volume_inspect (existing volume reused).
		wantCalls := []string{"inspect", "pull", "inspect", "inspect", "volume_inspect"}
		if !reflect.DeepEqual(engine.calls, wantCalls) {
			t.Fatalf("engine calls = %v, want %v", engine.calls, wantCalls)
		}
	})

	t.Run("Never fails clearly and never contacts the registry or the volume API when the sidecar image is missing", func(t *testing.T) {
		engine := &fakeDockerEngine{
			t: t,
			inspectResponses: []func(req *http.Request) *http.Response{
				func(req *http.Request) *http.Response {
					return mockEngineErrorResponse(t, req, http.StatusNotFound, "no such image")
				},
			},
		}
		backend := newTestDockerBackend(t, engine, PullPolicyNever)

		_, err := backend.prepareSidecars(context.Background(), backend.dockerClient, sidecars)
		if err == nil || !strings.Contains(err.Error(), "pull policy is Never") {
			t.Fatalf("error = %v, want a message naming pull policy Never", err)
		}
		wantCalls := []string{"inspect"}
		if !reflect.DeepEqual(engine.calls, wantCalls) {
			t.Fatalf("engine calls = %v, want %v (must fail before any pull or volume lookup)", engine.calls, wantCalls)
		}
	})
}
