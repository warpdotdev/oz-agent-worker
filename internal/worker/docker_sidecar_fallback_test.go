package worker

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func TestPrepareSidecarsFallsBackFromACPImage(t *testing.T) {
	const (
		acpImage = "docker.io/warpdotdev/warp-claude-acp-sidecar@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		cliImage = "docker.io/warpdotdev/warp-claude-cli-sidecar:latest"
		mount    = "/mnt/claude-cli-sidecar"
	)
	acpSidecar := []types.SidecarMount{{Image: acpImage, MountPath: mount, FallbackImage: cliImage}}
	inspectOK := func(req *http.Request) *http.Response {
		return mockImageInspectResponse(t, req, "linux", "amd64", "sha256:c11c11c11c11c11c11c11")
	}
	inspectMissing := func(req *http.Request) *http.Response {
		return mockEngineErrorResponse(t, req, http.StatusNotFound, "no such image")
	}
	// pullsBlockingACP answers pulls of the ACP repository with a registry error and
	// records every pulled repository.
	pullsBlockingACP := func(pulled *[]string, cliOK bool) func(*http.Request) *http.Response {
		return func(req *http.Request) *http.Response {
			from := req.URL.Query().Get("fromImage")
			*pulled = append(*pulled, from)
			if strings.Contains(from, "acp-sidecar") || !cliOK {
				return mockEngineErrorResponse(t, req, http.StatusInternalServerError, `Get "https://registry-1.docker.io/v2/": dial tcp: i/o timeout`)
			}
			return mockEngineResponse(req, http.StatusOK, "text/plain", "pulling image...\n")
		}
	}
	const acpRepo, cliRepo = "docker.io/warpdotdev/warp-claude-acp-sidecar", "docker.io/warpdotdev/warp-claude-cli-sidecar"

	t.Run("a failed ACP pull mounts the CLI image, and later tasks skip the ACP pull", func(t *testing.T) {
		var pulled []string
		engine := &fakeDockerEngine{t: t, volumeFound: true, inspectResponses: []func(*http.Request) *http.Response{inspectOK}, pullResponse: pullsBlockingACP(&pulled, true)}
		backend := newTestDockerBackend(t, engine, PullPolicyAlways)

		binds, err := backend.prepareSidecars(context.Background(), backend.dockerClient, acpSidecar, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(binds) != 1 || !strings.HasPrefix(binds[0], "warpdotdev-warp-claude-cli-sidecar-") || !strings.HasSuffix(binds[0], ":"+mount+":ro") {
			t.Fatalf("binds = %v, want the CLI sidecar volume mounted read-only at %s", binds, mount)
		}
		if want := []string{acpRepo, cliRepo}; !reflect.DeepEqual(pulled, want) {
			t.Fatalf("pulled = %v, want %v", pulled, want)
		}
		// ACP pull fails -> CLI pull -> post-pull platform check -> digest lookup -> volume reuse.
		if want := []string{"pull", "pull", "inspect", "inspect", "volume_inspect"}; !reflect.DeepEqual(engine.calls, want) {
			t.Fatalf("engine calls = %v, want %v", engine.calls, want)
		}

		pulled, engine.calls = nil, nil
		if _, err := backend.prepareSidecars(context.Background(), backend.dockerClient, acpSidecar, nil); err != nil {
			t.Fatalf("unexpected error on the second task: %v", err)
		}
		if want := []string{cliRepo}; !reflect.DeepEqual(pulled, want) {
			t.Fatalf("second task pulled %v, want only %v while the ACP failure is remembered", pulled, want)
		}
	})

	t.Run("both images failing names both", func(t *testing.T) {
		var pulled []string
		engine := &fakeDockerEngine{t: t, inspectResponses: []func(*http.Request) *http.Response{inspectOK}, pullResponse: pullsBlockingACP(&pulled, false)}
		backend := newTestDockerBackend(t, engine, PullPolicyAlways)

		_, err := backend.prepareSidecars(context.Background(), backend.dockerClient, acpSidecar, nil)
		if err == nil || !strings.Contains(err.Error(), acpImage) || !strings.Contains(err.Error(), "fallback sidecar image "+cliImage+" also failed") {
			t.Fatalf("error = %v, want one naming both images", err)
		}
		if want := []string{acpRepo, cliRepo}; !reflect.DeepEqual(pulled, want) {
			t.Fatalf("pulled = %v, want %v", pulled, want)
		}
	})

	t.Run("a failure after the image is prepared does not fall back", func(t *testing.T) {
		var pulled []string
		engine := &fakeDockerEngine{
			t: t,
			inspectResponses: []func(*http.Request) *http.Response{inspectOK, func(req *http.Request) *http.Response {
				return mockEngineErrorResponse(t, req, http.StatusInternalServerError, "inspect failed")
			}},
			pullResponse: func(req *http.Request) *http.Response {
				pulled = append(pulled, req.URL.Query().Get("fromImage"))
				return mockEngineResponse(req, http.StatusOK, "text/plain", "pulling image...\n")
			},
		}
		backend := newTestDockerBackend(t, engine, PullPolicyAlways)

		_, err := backend.prepareSidecars(context.Background(), backend.dockerClient, acpSidecar, nil)
		if err == nil || !strings.Contains(err.Error(), "failed to get digest") {
			t.Fatalf("error = %v, want the digest failure", err)
		}
		if want := []string{acpRepo}; !reflect.DeepEqual(pulled, want) {
			t.Fatalf("pulled = %v, want only %v", pulled, want)
		}
	})

	t.Run("a cancelled task does not fall back", func(t *testing.T) {
		var pulled []string
		engine := &fakeDockerEngine{t: t, inspectResponses: []func(*http.Request) *http.Response{inspectOK}, pullResponse: pullsBlockingACP(&pulled, true)}
		backend := newTestDockerBackend(t, engine, PullPolicyAlways)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := backend.prepareSidecars(ctx, backend.dockerClient, acpSidecar, nil); err == nil || strings.Contains(err.Error(), "fallback") {
			t.Fatalf("error = %v, want the ACP failure without a fallback attempt", err)
		}
		for _, repo := range pulled {
			if repo == cliRepo {
				t.Fatalf("pulled %v after cancellation", pulled)
			}
		}
		if backend.sidecarPullFailures.recent(acpImage) {
			t.Fatal("a cancelled pull must not be remembered as a failure")
		}
	})

	t.Run("pull policy Never falls back to a CLI image present locally", func(t *testing.T) {
		engine := &fakeDockerEngine{t: t, volumeFound: true, inspectResponses: []func(*http.Request) *http.Response{inspectMissing, inspectOK}}
		backend := newTestDockerBackend(t, engine, PullPolicyNever)

		binds, err := backend.prepareSidecars(context.Background(), backend.dockerClient, acpSidecar, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(binds) != 1 || !strings.HasPrefix(binds[0], "warpdotdev-warp-claude-cli-sidecar-") {
			t.Fatalf("binds = %v, want the CLI sidecar volume", binds)
		}
		if want := []string{"inspect", "inspect", "inspect", "volume_inspect"}; !reflect.DeepEqual(engine.calls, want) {
			t.Fatalf("engine calls = %v, want %v (no registry contact)", engine.calls, want)
		}
	})

	t.Run("a sidecar without a fallback fails as before", func(t *testing.T) {
		var pulled []string
		engine := &fakeDockerEngine{t: t, inspectResponses: []func(*http.Request) *http.Response{inspectOK}, pullResponse: pullsBlockingACP(&pulled, true)}
		backend := newTestDockerBackend(t, engine, PullPolicyAlways)

		_, err := backend.prepareSidecars(context.Background(), backend.dockerClient, []types.SidecarMount{{Image: acpImage, MountPath: mount}}, nil)
		if err == nil || !strings.Contains(err.Error(), "failed to prepare additional sidecar image "+acpImage) {
			t.Fatalf("error = %v, want the ACP pull failure", err)
		}
		if want := []string{acpRepo}; !reflect.DeepEqual(pulled, want) {
			t.Fatalf("pulled = %v, want %v", pulled, want)
		}
	})
}
