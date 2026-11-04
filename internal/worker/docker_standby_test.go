package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

const standbyTestContainerID = "standby-container"

type standbyCreateSpec struct {
	Labels map[string]string `json:"Labels"`
	Env    []string          `json:"Env"`
}

type standbyLifecycleEngine struct {
	t         *testing.T
	blockWait bool
	started   chan struct{}

	mu         sync.Mutex
	createName string
	createSpec standbyCreateSpec
	removed    []string
}

func newStandbyLifecycleEngine(t *testing.T, blockWait bool) *standbyLifecycleEngine {
	return &standbyLifecycleEngine{t: t, blockWait: blockWait, started: make(chan struct{}, 1)}
}

func (e *standbyLifecycleEngine) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case r.URL.Path == "/_ping":
		return mockEngineResponse(r, http.StatusOK, "text/plain", "OK"), nil
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/images/task-image/json"):
		return mockImageInspectResponse(e.t, r, "linux", "amd64", ""), nil
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create"):
		var spec standbyCreateSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			e.t.Errorf("failed to decode container create body: %v", err)
		}
		e.mu.Lock()
		e.createName = r.URL.Query().Get("name")
		e.createSpec = spec
		e.mu.Unlock()
		return mockEngineResponse(r, http.StatusCreated, "application/json", `{"Id":"`+standbyTestContainerID+`"}`), nil
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
		select {
		case e.started <- struct{}{}:
		default:
		}
		return mockEngineResponse(r, http.StatusNoContent, "application/json", ""), nil
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/wait"):
		if e.blockWait {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return mockEngineResponse(r, http.StatusOK, "application/json", `{"StatusCode":0}`), nil
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs"):
		return mockEngineResponse(r, http.StatusOK, "application/octet-stream", ""), nil
	case r.Method == http.MethodDelete:
		e.mu.Lock()
		e.removed = append(e.removed, path.Base(r.URL.Path))
		e.mu.Unlock()
		return mockEngineResponse(r, http.StatusNoContent, "application/json", ""), nil
	default:
		e.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		return mockEngineErrorResponse(e.t, r, http.StatusInternalServerError, "unexpected request"), nil
	}
}

func newStandbyDockerBackend(t *testing.T, engine http.RoundTripper) *DockerBackend {
	t.Helper()
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	dockerClient, err := client.New(client.WithHTTPClient(&http.Client{Transport: engine}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dockerClient.Close() })
	return &DockerBackend{
		config:       DockerBackendConfig{ImagePullPolicy: PullPolicyNever},
		dockerClient: dockerClient,
		platform:     "linux/amd64",
	}
}

func assertNoRunIdentityEnv(t *testing.T, envVars []string) {
	t.Helper()
	for _, entry := range envVars {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "OZ_RUN_ID", "WARP_RUN_ID", "OZ_EXECUTION_ID", "WARP_EXECUTION_ID":
			t.Errorf("standby container env contains %s", name)
		}
	}
}

func TestDockerStandbyExecuteTaskNamesAndLabelsContainer(t *testing.T) {
	engine := newStandbyLifecycleEngine(t, false)
	backend := newStandbyDockerBackend(t, engine)

	result := backend.ExecuteTask(context.Background(), &TaskParams{
		StandbyID:   "sb-1",
		DockerImage: "task-image",
		EnvVars:     []string{"STANDBY_SETTING=value"},
		BaseArgs:    []string{"agent", "run-cloud-standby", "--standby-id", "sb-1"},
	})
	if result.Outcome != ExecuteOutcomeCompleted || result.Error != nil {
		t.Fatalf("ExecuteTask() = %+v, want completed", result)
	}
	if engine.createName != StandbyInstanceRef("sb-1") {
		t.Errorf("container name = %q, want %q", engine.createName, StandbyInstanceRef("sb-1"))
	}
	if want := map[string]string{StandbyDockerLabel: "sb-1"}; !reflect.DeepEqual(engine.createSpec.Labels, want) {
		t.Errorf("container labels = %v, want %v", engine.createSpec.Labels, want)
	}
	assertNoRunIdentityEnv(t, engine.createSpec.Env)
	if want := []string{standbyTestContainerID}; !reflect.DeepEqual(engine.removed, want) {
		t.Errorf("removed containers = %v, want %v", engine.removed, want)
	}
}

func TestDockerTaskExecuteTaskKeepsTaskLabels(t *testing.T) {
	engine := newStandbyLifecycleEngine(t, false)
	backend := newStandbyDockerBackend(t, engine)

	result := backend.ExecuteTask(context.Background(), &TaskParams{
		TaskID:      "run-1",
		ExecutionID: "exec-1",
		DockerImage: "task-image",
	})
	if result.Outcome != ExecuteOutcomeCompleted || result.Error != nil {
		t.Fatalf("ExecuteTask() = %+v, want completed", result)
	}
	if engine.createName != "" {
		t.Errorf("task container name = %q, want an engine-generated name", engine.createName)
	}
	want := map[string]string{"oz-task-id": "run-1", "oz-execution-id": "exec-1"}
	if !reflect.DeepEqual(engine.createSpec.Labels, want) {
		t.Errorf("task labels = %v, want %v", engine.createSpec.Labels, want)
	}
}

func TestDockerStandbyCancellationRemovesContainer(t *testing.T) {
	engine := newStandbyLifecycleEngine(t, true)
	backend := newStandbyDockerBackend(t, engine)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make(chan ExecuteResult, 1)
	go func() {
		results <- backend.ExecuteTask(ctx, &TaskParams{StandbyID: "sb-1", DockerImage: "task-image"})
	}()
	select {
	case <-engine.started:
	case <-time.After(10 * time.Second):
		t.Fatal("standby container was not started")
	}

	cancel()
	select {
	case result := <-results:
		if result.Outcome != ExecuteOutcomeError {
			t.Fatalf("ExecuteTask() outcome = %v, want error", result.Outcome)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ExecuteTask did not return after cancellation")
	}
	if want := []string{standbyTestContainerID}; !reflect.DeepEqual(engine.removed, want) {
		t.Errorf("removed containers = %v, want %v", engine.removed, want)
	}
}

type standbySweepEngine struct {
	t          *testing.T
	failRemove string
	listQuery  url.Values
	removed    []string
}

func (e *standbySweepEngine) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case r.URL.Path == "/_ping":
		return mockEngineResponse(r, http.StatusOK, "text/plain", "OK"), nil
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json"):
		e.listQuery = r.URL.Query()
		body := `[{"Id":"c1","Labels":{"oz.prewarmed-sandbox.id":"sb-1"}},{"Id":"c2","Labels":{"oz.prewarmed-sandbox.id":"sb-2"}}]`
		return mockEngineResponse(r, http.StatusOK, "application/json", body), nil
	case r.Method == http.MethodDelete:
		id := path.Base(r.URL.Path)
		if id == e.failRemove {
			return mockEngineErrorResponse(e.t, r, http.StatusInternalServerError, "unavailable"), nil
		}
		e.removed = append(e.removed, id)
		return mockEngineResponse(r, http.StatusNoContent, "application/json", ""), nil
	default:
		e.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		return mockEngineErrorResponse(e.t, r, http.StatusInternalServerError, "unexpected request"), nil
	}
}

func TestDockerSweepStandbysRemovesLabeledContainers(t *testing.T) {
	t.Run("removes every labeled container and returns sandbox IDs", func(t *testing.T) {
		engine := &standbySweepEngine{t: t}
		backend := newStandbyDockerBackend(t, engine)

		ids, err := backend.SweepStandbys(context.Background())
		if err != nil {
			t.Fatalf("SweepStandbys() error = %v", err)
		}
		if want := []string{"sb-1", "sb-2"}; !reflect.DeepEqual(ids, want) {
			t.Errorf("SweepStandbys() ids = %v, want %v", ids, want)
		}
		if want := []string{"c1", "c2"}; !reflect.DeepEqual(engine.removed, want) {
			t.Errorf("removed containers = %v, want %v", engine.removed, want)
		}
		if engine.listQuery.Get("all") != "1" {
			t.Errorf("list query all = %q, want 1 (stopped containers must be listed)", engine.listQuery.Get("all"))
		}
		var filters map[string]map[string]bool
		if err := json.Unmarshal([]byte(engine.listQuery.Get("filters")), &filters); err != nil {
			t.Fatalf("failed to decode list filters: %v", err)
		}
		if !filters["label"][StandbyDockerLabel] {
			t.Errorf("list filters = %v, want label %s", filters, StandbyDockerLabel)
		}
	})

	t.Run("omits sandbox IDs whose containers failed removal", func(t *testing.T) {
		engine := &standbySweepEngine{t: t, failRemove: "c1"}
		backend := newStandbyDockerBackend(t, engine)

		ids, err := backend.SweepStandbys(context.Background())
		if err == nil {
			t.Fatal("SweepStandbys() error = nil, want removal failure")
		}
		if want := []string{"sb-2"}; !reflect.DeepEqual(ids, want) {
			t.Errorf("SweepStandbys() ids = %v, want %v", ids, want)
		}
		if want := []string{"c2"}; !reflect.DeepEqual(engine.removed, want) {
			t.Errorf("removed containers = %v, want %v", engine.removed, want)
		}
	})
}
