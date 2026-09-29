package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/moby/moby/client"
)

type sidecarVolumeEngine struct {
	t *testing.T

	mu                  sync.Mutex
	volumes             map[string]bool
	inspectStatus       int
	createStatus        int
	createResponseError error
	removeStatus        int
	inspections         int
	creations           int
	removals            int
}

func (e *sidecarVolumeEngine) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case req.URL.Path == "/_ping":
		return mockEngineResponse(req, http.StatusOK, "text/plain", "OK"), nil
	case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/volumes/"):
		e.inspections++
		if e.inspectStatus != 0 {
			return mockEngineErrorResponse(e.t, req, e.inspectStatus, "inspection failed"), nil
		}
		if !e.volumes[path.Base(req.URL.Path)] {
			return mockEngineErrorResponse(e.t, req, http.StatusNotFound, "no such volume"), nil
		}
		return mockEngineResponse(req, http.StatusOK, "application/json", `{}`), nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/volumes/create"):
		e.creations++
		if e.createStatus != 0 {
			return mockEngineErrorResponse(e.t, req, e.createStatus, "creation failed"), nil
		}
		var body struct{ Name string }
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		e.volumes[body.Name] = true
		if e.createResponseError != nil {
			return nil, e.createResponseError
		}
		return mockEngineResponse(req, http.StatusCreated, "application/json", `{}`), nil
	case req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/volumes/"):
		e.removals++
		if e.removeStatus != 0 {
			return mockEngineErrorResponse(e.t, req, e.removeStatus, "removal failed"), nil
		}
		delete(e.volumes, path.Base(req.URL.Path))
		return mockEngineResponse(req, http.StatusNoContent, "application/json", ""), nil
	default:
		e.t.Errorf("unexpected Docker request: %s %s", req.Method, req.URL.Path)
		return mockEngineErrorResponse(e.t, req, http.StatusInternalServerError, "unexpected request"), nil
	}
}

func newSidecarVolumeTestBackend(t *testing.T) (*DockerBackend, *sidecarVolumeEngine) {
	t.Helper()
	engine := &sidecarVolumeEngine{t: t, volumes: make(map[string]bool)}
	dockerClient, err := client.New(client.WithHTTPClient(&http.Client{Transport: engine}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := dockerClient.Close(); err != nil {
			t.Error(err)
		}
	})
	return &DockerBackend{dockerClient: dockerClient}, engine
}

func TestPrepareSidecarVolume(t *testing.T) {
	t.Run("shared volume is not reused until extraction completes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			backend, engine := newSidecarVolumeTestBackend(t)
			started := make(chan struct{})
			release := make(chan struct{})
			results := make(chan error, 4)
			go func() {
				results <- backend.prepareSidecarVolume(t.Context(), backend.dockerClient, "shared", func(context.Context) error {
					close(started)
					<-release
					return nil
				})
			}()
			<-started
			for range 3 {
				go func() {
					results <- backend.prepareSidecarVolume(t.Context(), backend.dockerClient, "shared", func(context.Context) error {
						t.Error("shared volume must be populated only once")
						return nil
					})
				}()
			}
			synctest.Wait()
			select {
			case err := <-results:
				t.Fatalf("task used a volume before extraction completed: %v", err)
			default:
			}
			if engine.inspections != 1 || engine.creations != 1 {
				t.Fatalf("waiters reached Docker: inspections=%d creations=%d", engine.inspections, engine.creations)
			}

			otherPopulated := false
			if err := backend.prepareSidecarVolume(t.Context(), backend.dockerClient, "independent", func(context.Context) error {
				otherPopulated = true
				return nil
			}); err != nil || !otherPopulated {
				t.Fatalf("independent volume must not wait for shared extraction: %v", err)
			}

			close(release)
			for range 4 {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			if engine.creations != 2 {
				t.Fatalf("created %d volumes, want one per image", engine.creations)
			}
		})
	})

	t.Run("canceled waiter does not cancel ongoing extraction", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			backend, engine := newSidecarVolumeTestBackend(t)
			started := make(chan struct{})
			release := make(chan struct{})
			ownerResult := make(chan error, 1)
			go func() {
				ownerResult <- backend.prepareSidecarVolume(t.Context(), backend.dockerClient, "shared", func(context.Context) error {
					close(started)
					<-release
					return nil
				})
			}()
			<-started
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waiterResult := make(chan error, 1)
			go func() {
				waiterResult <- backend.prepareSidecarVolume(ctx, backend.dockerClient, "shared", func(context.Context) error {
					t.Error("canceled waiter must not populate the volume")
					return nil
				})
			}()
			synctest.Wait()
			cancel()
			if err := <-waiterResult; !errors.Is(err, context.Canceled) {
				t.Fatalf("waiter error = %v, want context.Canceled", err)
			}
			if engine.inspections != 1 || engine.removals != 0 {
				t.Fatal("canceled waiter touched the initializing volume")
			}
			close(release)
			if err := <-ownerResult; err != nil {
				t.Fatal(err)
			}
		})
	})

	t.Run("canceled extraction is cleaned before a waiter retries", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			backend, engine := newSidecarVolumeTestBackend(t)
			started := make(chan struct{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ownerResult := make(chan error, 1)
			go func() {
				ownerResult <- backend.prepareSidecarVolume(ctx, backend.dockerClient, "shared", func(ctx context.Context) error {
					close(started)
					<-ctx.Done()
					return ctx.Err()
				})
			}()
			<-started
			retryResult := make(chan error, 1)
			repopulated := false
			go func() {
				retryResult <- backend.prepareSidecarVolume(t.Context(), backend.dockerClient, "shared", func(context.Context) error {
					repopulated = true
					return nil
				})
			}()
			synctest.Wait()
			cancel()
			if err := <-ownerResult; !errors.Is(err, context.Canceled) {
				t.Fatalf("owner error = %v, want context.Canceled", err)
			}
			if err := <-retryResult; err != nil {
				t.Fatal(err)
			}
			if !repopulated || engine.creations != 2 || engine.removals != 1 {
				t.Fatalf("retry reused partial contents: populated=%v creations=%d removals=%d",
					repopulated, engine.creations, engine.removals)
			}
		})
	})

	t.Run("lost create response does not leave a reusable empty volume", func(t *testing.T) {
		backend, engine := newSidecarVolumeTestBackend(t)
		engine.createResponseError = errors.New("connection lost after creation")
		err := backend.prepareSidecarVolume(t.Context(), backend.dockerClient, "shared", func(context.Context) error {
			t.Error("must not populate after a failed create response")
			return nil
		})
		if err == nil || !engine.volumes["shared"] {
			t.Fatalf("expected an ambiguous create failure with a volume left behind, got %v", err)
		}
		engine.createResponseError = nil
		repopulated := false
		err = backend.prepareSidecarVolume(t.Context(), backend.dockerClient, "shared", func(context.Context) error {
			repopulated = true
			return nil
		})
		if err != nil || !repopulated || engine.creations != 2 || engine.removals != 1 {
			t.Fatalf("retry reused an empty volume: err=%v populated=%v creations=%d removals=%d",
				err, repopulated, engine.creations, engine.removals)
		}
	})

	t.Run("failed cleanup never exposes incomplete contents to the next task", func(t *testing.T) {
		backend, engine := newSidecarVolumeTestBackend(t)
		engine.removeStatus = http.StatusConflict
		copyErr := errors.New("extraction failed")
		err := backend.prepareSidecarVolume(t.Context(), backend.dockerClient, "shared", func(context.Context) error {
			return copyErr
		})
		if !errors.Is(err, copyErr) {
			t.Fatalf("error = %v, want extraction failure", err)
		}
		err = backend.prepareSidecarVolume(t.Context(), backend.dockerClient, "shared", func(context.Context) error {
			t.Error("must not populate a volume that could not be cleaned")
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "incomplete sidecar volume") {
			t.Fatalf("error = %v, want incomplete-volume failure", err)
		}
		if engine.inspections != 1 {
			t.Fatal("must not inspect and reuse a known incomplete volume")
		}
		engine.removeStatus = 0
		repopulated := false
		if err := backend.prepareSidecarVolume(t.Context(), backend.dockerClient, "shared", func(context.Context) error {
			repopulated = true
			return nil
		}); err != nil || !repopulated {
			t.Fatalf("retry did not repopulate after successful cleanup: %v", err)
		}
	})

	for _, tc := range []struct {
		name          string
		existing      bool
		inspectStatus int
		createStatus  int
		wantError     string
	}{
		{name: "reuse existing cache", existing: true},
		{name: "inspection failure is not absence", inspectStatus: http.StatusInternalServerError, wantError: "failed to inspect"},
		{name: "creation failure does not populate", createStatus: http.StatusInternalServerError, wantError: "failed to create"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, engine := newSidecarVolumeTestBackend(t)
			engine.volumes["shared"] = tc.existing
			engine.inspectStatus = tc.inspectStatus
			engine.createStatus = tc.createStatus
			err := backend.prepareSidecarVolume(t.Context(), backend.dockerClient, "shared", func(context.Context) error {
				t.Error("unexpected extraction")
				return nil
			})
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want %q", err, tc.wantError)
			}
			if tc.inspectStatus != 0 && engine.creations != 0 {
				t.Fatal("inspection failure must not create a volume")
			}
		})
	}
}
