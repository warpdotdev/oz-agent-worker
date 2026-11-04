package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/warpdotdev/oz-agent-worker/internal/common"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"golang.org/x/sync/semaphore"
)

type standbyExecution struct {
	params *TaskParams
	// queued is the number of outbound messages waiting when ExecuteTask started.
	queued int
}

type standbyTestBackend struct {
	executed chan standbyExecution
	finish   chan ExecuteResult
	causes   chan error
	preserve bool
	pending  func() int
}

func newStandbyTestBackend(preserve bool) *standbyTestBackend {
	return &standbyTestBackend{
		executed: make(chan standbyExecution, 8),
		finish:   make(chan ExecuteResult, 8),
		causes:   make(chan error, 8),
		preserve: preserve,
	}
}

func (b *standbyTestBackend) ExecuteTask(ctx context.Context, params *TaskParams) ExecuteResult {
	queued := 0
	if b.pending != nil {
		queued = b.pending()
	}
	b.executed <- standbyExecution{params: params, queued: queued}
	select {
	case result := <-b.finish:
		return result
	case <-ctx.Done():
		b.causes <- context.Cause(ctx)
		return executeError(ctx.Err())
	}
}

func (b *standbyTestBackend) CancelTask(context.Context, *CancelParams) error { return nil }
func (b *standbyTestBackend) Shutdown(context.Context)                        {}
func (b *standbyTestBackend) PreservesTasksOnShutdown() bool                  { return b.preserve }

type destroyingStandbyBackend struct {
	*standbyTestBackend
	destroyed chan string
}

func (b *destroyingStandbyBackend) CancelTask(_ context.Context, params *CancelParams) error {
	if params.TaskID != "" || params.ExecutionID != "" {
		return fmt.Errorf("standby cancellation contains task identifiers")
	}
	b.destroyed <- params.StandbyID
	return nil
}

type standbyWatchReport struct {
	sandboxID string
	exitCode  int
	reason    string
}

type watchingStandbyBackend struct {
	*standbyTestBackend
	reports []standbyWatchReport
	watched chan struct{}
	stopped chan struct{}
}

func (b *watchingStandbyBackend) WatchStandbyExits(ctx context.Context, report func(string, int, string)) {
	for _, r := range b.reports {
		report(r.sandboxID, r.exitCode, r.reason)
	}
	close(b.watched)
	<-ctx.Done()
	if b.stopped != nil {
		close(b.stopped)
	}
}

func newStandbyTestWorker(t *testing.T, backend Backend, config Config) *Worker {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &Worker{
		config:       config,
		ctx:          ctx,
		outbound:     newOutboundQueue(64),
		activeTasks:  make(map[taskExecution]activeTask),
		standbys:     make(map[string]*standbyEntry),
		oneShot:      newOneShotState(),
		backend:      backend,
		capabilities: backendCapabilities{supportsStandbys: true},
	}
	if config.MaxConcurrentTasks > 0 {
		w.taskSemaphore = semaphore.NewWeighted(int64(config.MaxConcurrentTasks))
	}
	if config.MaxPrewarmedSandboxes > 0 {
		w.standbySemaphore = semaphore.NewWeighted(int64(config.MaxPrewarmedSandboxes))
	}
	t.Cleanup(func() {
		cancel()
		w.shutdownTasks()
		w.cancellationWG.Wait()
	})
	return w
}

func standbyCreate(sandboxID string) *types.PrewarmCreateMessage {
	return &types.PrewarmCreateMessage{
		SandboxID:          sandboxID,
		Args:               []string{"agent", "run-cloud-standby", "--standby-id", sandboxID},
		DeadlineCapSeconds: 3600,
	}
}

func awaitStandbyExecution(t *testing.T, backend *standbyTestBackend) standbyExecution {
	t.Helper()
	select {
	case execution := <-backend.executed:
		return execution
	case <-time.After(2 * time.Second):
		t.Fatal("backend did not start execution")
	}
	return standbyExecution{}
}

func awaitStandbyMessage(t *testing.T, w *Worker, messageType types.MessageType) types.WebSocketMessage {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case raw := <-w.outbound.messages:
			var msg types.WebSocketMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				t.Fatalf("failed to unmarshal outbound message: %v", err)
			}
			if msg.Type == messageType {
				return msg
			}
		case <-timeout:
			t.Fatalf("no %s message sent", messageType)
		}
	}
}

func decodeStandbyExited(t *testing.T, msg types.WebSocketMessage) types.PrewarmExitedMessage {
	t.Helper()
	var exited types.PrewarmExitedMessage
	if err := json.Unmarshal(msg.Data, &exited); err != nil {
		t.Fatalf("failed to decode prewarm exited message: %v", err)
	}
	return exited
}

func standbyFailure(reason metrics.TaskFailureReason, exitCode int) error {
	return newBackendFailureWithExitCode(metrics.TaskFailurePhaseBackend, reason, errors.New("standby failed"), exitCode)
}

func TestStandbyCapabilityHeader(t *testing.T) {
	const base = "execution-scoped-cancellation-v1"
	const withStandbys = base + ",prewarmed-sandboxes-v1"
	supported := backendCapabilities{supportsStandbys: true}

	tests := []struct {
		name         string
		capabilities backendCapabilities
		config       Config
		want         string
	}{
		{name: "backend supports standbys", capabilities: supported, want: withStandbys},
		{name: "backend without standbys", want: base},
		{name: "one-shot mode", capabilities: supported, config: Config{OneShot: true}, want: base},
		{
			name:         "direct backend with target dir",
			capabilities: supported,
			config:       Config{BackendType: "direct", Direct: &DirectBackendConfig{TargetDir: "/work"}},
			want:         base,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := make(chan string, 1)
			upgrader := websocket.Upgrader{}
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				headers <- r.Header.Get("X-Warp-Worker-Capabilities")
				conn, err := upgrader.Upgrade(rw, r, nil)
				if err != nil {
					return
				}
				_ = conn.Close()
			}))
			defer srv.Close()

			config := tt.config
			config.WorkerID = "header-worker"
			config.APIKey = "test-api-key"
			config.WebSocketURL = "ws" + strings.TrimPrefix(srv.URL, "http")
			w := &Worker{ctx: context.Background(), config: config, capabilities: tt.capabilities}
			conn, err := w.connect()
			if err != nil {
				t.Fatalf("connect() error = %v", err)
			}
			_ = conn.Close()

			if got := <-headers; got != tt.want {
				t.Fatalf("X-Warp-Worker-Capabilities = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPrewarmCreateSendsCreatedBeforeExecution(t *testing.T) {
	backend := newStandbyTestBackend(false)
	w := newStandbyTestWorker(t, backend, Config{})
	backend.pending = func() int { return len(w.outbound.messages) }

	w.handlePrewarmCreate(standbyCreate("standby-1"))

	execution := awaitStandbyExecution(t, backend)
	if execution.queued != 1 {
		t.Fatalf("queued messages at execution start = %d, want 1 (prewarm_created)", execution.queued)
	}
	var created types.PrewarmCreatedMessage
	msg := awaitStandbyMessage(t, w, types.MessageTypePrewarmCreated)
	if err := json.Unmarshal(msg.Data, &created); err != nil {
		t.Fatalf("failed to decode prewarm created message: %v", err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname() error = %v", err)
	}
	if created.SandboxID != "standby-1" || created.InstanceRef != StandbyInstanceRef("standby-1") || created.ReportedBy != hostname {
		t.Fatalf("prewarm created = %+v, want sandbox standby-1 with instance ref and hostname %q", created, hostname)
	}
}

func TestPrewarmCreateIgnoresDuplicateAndDispatchesFromHandleMessage(t *testing.T) {
	backend := newStandbyTestBackend(false)
	w := newStandbyTestWorker(t, backend, Config{})

	data, err := json.Marshal(standbyCreate("standby-dup"))
	if err != nil {
		t.Fatalf("failed to marshal create: %v", err)
	}
	raw, err := json.Marshal(types.WebSocketMessage{Type: types.MessageTypePrewarmCreate, Data: data})
	if err != nil {
		t.Fatalf("failed to marshal websocket message: %v", err)
	}

	w.handleMessage(raw, time.Now())
	awaitStandbyExecution(t, backend)
	w.handleMessage(raw, time.Now())

	select {
	case <-backend.executed:
		t.Fatal("duplicate create started a second execution")
	case <-time.After(100 * time.Millisecond):
	}
	awaitStandbyMessage(t, w, types.MessageTypePrewarmCreated)
	if len(w.outbound.messages) != 0 {
		t.Fatalf("duplicate create sent %d extra messages", len(w.outbound.messages))
	}
}

func TestPrewarmCreateRejectsWhenUnavailable(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		setup  func(w *Worker)
		reason string
	}{
		{
			name:   "standby capacity full",
			config: Config{MaxPrewarmedSandboxes: 1},
			setup:  func(w *Worker) { w.standbySemaphore.TryAcquire(1) },
			reason: "worker at maximum prewarmed sandboxes",
		},
		{
			name:   "shared task capacity full",
			config: Config{MaxConcurrentTasks: 1},
			setup:  func(w *Worker) { w.taskSemaphore.TryAcquire(1) },
			reason: "worker at maximum concurrency",
		},
		{
			name: "shutting down",
			setup: func(w *Worker) {
				w.tasksMutex.Lock()
				w.shuttingDown = true
				w.tasksMutex.Unlock()
			},
			reason: "worker is shutting down",
		},
		{
			name:   "one-shot mode",
			config: Config{OneShot: true},
			reason: "one-shot worker does not run standby sandboxes",
		},
		{
			name:   "backend without standbys",
			setup:  func(w *Worker) { w.capabilities = backendCapabilities{} },
			reason: "backend does not support standby sandboxes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newStandbyTestBackend(false)
			w := newStandbyTestWorker(t, backend, tt.config)
			if tt.setup != nil {
				tt.setup(w)
			}

			w.handlePrewarmCreate(standbyCreate("standby-rejected"))

			msg := awaitStandbyMessage(t, w, types.MessageTypePrewarmRejected)
			var rejected types.PrewarmRejectedMessage
			if err := json.Unmarshal(msg.Data, &rejected); err != nil {
				t.Fatalf("failed to decode prewarm rejected message: %v", err)
			}
			if rejected.SandboxID != "standby-rejected" || rejected.Reason != tt.reason {
				t.Fatalf("prewarm rejected = %+v, want reason %q", rejected, tt.reason)
			}
			if len(backend.executed) != 0 {
				t.Fatal("rejected standby was executed")
			}
		})
	}
}

func TestStandbyExitReportsBackendResult(t *testing.T) {
	tests := []struct {
		name       string
		result     ExecuteResult
		wantExit   int
		wantReason string
	}{
		{
			name:     "completed",
			result:   executeCompleted(),
			wantExit: 0,
		},
		{
			name:       "agent failure keeps exit code",
			result:     executeError(standbyFailure(metrics.TaskFailureReasonAgentInvocation, 143)),
			wantExit:   143,
			wantReason: string(metrics.TaskFailureReasonAgentInvocation),
		},
		{
			name:       "failure without exit code reports 1",
			result:     executeError(standbyFailure(metrics.TaskFailureReasonImagePull, 0)),
			wantExit:   1,
			wantReason: string(metrics.TaskFailureReasonImagePull),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newStandbyTestBackend(false)
			w := newStandbyTestWorker(t, backend, Config{MaxPrewarmedSandboxes: 1})
			w.handlePrewarmCreate(standbyCreate("standby-exit"))
			awaitStandbyExecution(t, backend)

			backend.finish <- tt.result
			msg := awaitStandbyMessage(t, w, types.MessageTypePrewarmExited)
			exited := decodeStandbyExited(t, msg)
			if exited.SandboxID != "standby-exit" || exited.ExitCode != tt.wantExit || exited.Reason != tt.wantReason {
				t.Fatalf("prewarm exited = %+v, want exit %d reason %q", exited, tt.wantExit, tt.wantReason)
			}

			waitFor(t, 2*time.Second, func() bool {
				w.tasksMutex.Lock()
				defer w.tasksMutex.Unlock()
				return len(w.standbys) == 0
			})
			if !w.standbySemaphore.TryAcquire(1) {
				t.Fatal("standby slot was not released after exit")
			}
			w.standbySemaphore.Release(1)
		})
	}
}

func TestStandbyDeadlineCapEndsStandby(t *testing.T) {
	backend := newStandbyTestBackend(false)
	w := newStandbyTestWorker(t, backend, Config{})
	create := standbyCreate("standby-deadline")
	create.DeadlineCapSeconds = 1

	w.handlePrewarmCreate(create)

	msg := awaitStandbyMessage(t, w, types.MessageTypePrewarmExited)
	exited := decodeStandbyExited(t, msg)
	if exited.ExitCode != 1 || exited.Reason != string(metrics.TaskFailureReasonTaskTimeout) {
		t.Fatalf("prewarm exited = %+v, want exit 1 reason %q", exited, metrics.TaskFailureReasonTaskTimeout)
	}
}

func TestStandbyExitStatus(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		source     taskCancellationSource
		wantExit   int
		wantReason metrics.TaskFailureReason
	}{
		{name: "no error", wantExit: 0, wantReason: ""},
		{name: "deadline is task timeout", err: context.DeadlineExceeded, wantExit: 1, wantReason: metrics.TaskFailureReasonTaskTimeout},
		{name: "unknown error", err: errors.New("boom"), wantExit: 1, wantReason: metrics.TaskFailureReasonUnknown},
		{name: "destroy is task cancelled", err: context.Canceled, source: taskCancellationSourceUser, wantExit: 1, wantReason: metrics.TaskFailureReasonTaskCancelled},
		{
			name:       "destroy keeps failure exit code",
			err:        standbyFailure(metrics.TaskFailureReasonAgentInvocation, 143),
			source:     taskCancellationSourceUser,
			wantExit:   143,
			wantReason: metrics.TaskFailureReasonTaskCancelled,
		},
		{name: "shutdown is graceful", err: context.Canceled, source: taskCancellationSourceShutdown, wantExit: 1, wantReason: metrics.TaskFailureReasonGracefulShutdown},
		{
			name:       "shutdown SIGTERM is graceful",
			err:        standbyFailure(metrics.TaskFailureReasonAgentInvocation, 143),
			source:     taskCancellationSourceShutdown,
			wantExit:   143,
			wantReason: metrics.TaskFailureReasonGracefulShutdown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exitCode, reason := standbyExitStatus(tt.err, tt.source)
			if exitCode != tt.wantExit || reason != string(tt.wantReason) {
				t.Fatalf("standbyExitStatus() = (%d, %q), want (%d, %q)", exitCode, reason, tt.wantExit, tt.wantReason)
			}
		})
	}
}

func TestPrewarmDestroyCancelsLocalStandby(t *testing.T) {
	backend := newStandbyTestBackend(false)
	w := newStandbyTestWorker(t, backend, Config{})
	w.handlePrewarmCreate(standbyCreate("standby-destroy"))
	awaitStandbyExecution(t, backend)

	w.handlePrewarmDestroy(&types.PrewarmDestroyMessage{SandboxID: "standby-destroy"})

	msg := awaitStandbyMessage(t, w, types.MessageTypePrewarmExited)
	exited := decodeStandbyExited(t, msg)
	if exited.SandboxID != "standby-destroy" || exited.ExitCode != 1 || exited.Reason != string(metrics.TaskFailureReasonTaskCancelled) {
		t.Fatalf("prewarm exited = %+v, want task_cancelled exit 1", exited)
	}
}

func TestPrewarmDestroyCallsBackendCancellation(t *testing.T) {
	t.Run("empty sandbox does not cancel a task", func(t *testing.T) {
		backend := &destroyingStandbyBackend{
			standbyTestBackend: newStandbyTestBackend(false),
			destroyed:          make(chan string, 1),
		}
		w := newStandbyTestWorker(t, backend, Config{})

		w.handlePrewarmDestroy(&types.PrewarmDestroyMessage{})
		w.cancellationWG.Wait()

		if len(backend.destroyed) != 0 {
			t.Fatal("empty sandbox ID reached backend cancellation")
		}
	})

	t.Run("unknown sandbox is not an error", func(t *testing.T) {
		backend := &destroyingStandbyBackend{
			standbyTestBackend: newStandbyTestBackend(false),
			destroyed:          make(chan string, 4),
		}
		w := newStandbyTestWorker(t, backend, Config{})

		w.handlePrewarmDestroy(&types.PrewarmDestroyMessage{SandboxID: "remote-standby"})

		select {
		case id := <-backend.destroyed:
			if id != "remote-standby" {
				t.Fatalf("CancelTask standby ID = %q, want remote-standby", id)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("CancelTask was not called")
		}
		if len(w.outbound.messages) != 0 {
			t.Fatal("destroy of an unknown sandbox sent a message")
		}
	})

	t.Run("local standby is cancelled and destroyed on the backend", func(t *testing.T) {
		backend := &destroyingStandbyBackend{
			standbyTestBackend: newStandbyTestBackend(false),
			destroyed:          make(chan string, 4),
		}
		w := newStandbyTestWorker(t, backend, Config{})
		w.handlePrewarmCreate(standbyCreate("standby-both"))
		awaitStandbyExecution(t, backend.standbyTestBackend)

		w.handlePrewarmDestroy(&types.PrewarmDestroyMessage{SandboxID: "standby-both"})

		select {
		case id := <-backend.destroyed:
			if id != "standby-both" {
				t.Fatalf("CancelTask standby ID = %q, want standby-both", id)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("CancelTask was not called")
		}
		awaitStandbyMessage(t, w, types.MessageTypePrewarmExited)
	})
}

func TestStandbyExitWatcherSkipsLocalSandboxes(t *testing.T) {
	backend := &watchingStandbyBackend{
		standbyTestBackend: newStandbyTestBackend(false),
		reports: []standbyWatchReport{
			{sandboxID: "local-1", exitCode: 0},
			{sandboxID: "remote-1", exitCode: 2, reason: string(metrics.TaskFailureReasonAgentInvocation)},
		},
		watched: make(chan struct{}),
	}
	w := newStandbyTestWorker(t, backend, Config{})
	w.handlePrewarmCreate(standbyCreate("local-1"))
	awaitStandbyExecution(t, backend.standbyTestBackend)

	w.startStandbyExitWatcher()
	select {
	case <-backend.watched:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not start")
	}

	msg := awaitStandbyMessage(t, w, types.MessageTypePrewarmExited)
	exited := decodeStandbyExited(t, msg)
	if exited.SandboxID != "remote-1" || exited.ExitCode != 2 || exited.Reason != string(metrics.TaskFailureReasonAgentInvocation) {
		t.Fatalf("prewarm exited = %+v, want remote-1 exit 2", exited)
	}
	for _, queued := range drainMessages(t, w.outbound.messages) {
		if queued.Type == types.MessageTypePrewarmExited {
			t.Fatalf("watcher reported a second exit: %s", queued.Data)
		}
	}
}

func TestStandbyExitWatcherSkipsCompletedLocalSandbox(t *testing.T) {
	backend := newStandbyTestBackend(false)
	w := newStandbyTestWorker(t, backend, Config{})
	w.handlePrewarmCreate(standbyCreate("local-1"))
	awaitStandbyExecution(t, backend)
	backend.finish <- executeCompleted()
	w.taskWG.Wait()

	w.reportWatchedStandbyExit("local-1", 0, "")
	count := 0
	for _, msg := range drainMessages(t, w.outbound.messages) {
		if msg.Type == types.MessageTypePrewarmExited {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("sent %d exit reports for a completed local standby, want 1", count)
	}
}

func TestStandbyExitWatcherStopsDuringShutdown(t *testing.T) {
	backend := &watchingStandbyBackend{
		standbyTestBackend: newStandbyTestBackend(true),
		watched:            make(chan struct{}),
		stopped:            make(chan struct{}),
	}
	w := newStandbyTestWorker(t, backend, Config{})
	w.startStandbyExitWatcher()
	<-backend.watched

	w.shutdownTasks()

	select {
	case <-backend.stopped:
	default:
		t.Fatal("shutdown returned before the standby exit watcher stopped")
	}
}

func TestStandbyExitDeduplication(t *testing.T) {
	w := newStandbyTestWorker(t, newStandbyTestBackend(false), Config{})
	var reporters sync.WaitGroup
	for range 8 {
		reporters.Go(func() { w.sendStandbyExit("sb-0", 0, "") })
	}
	reporters.Wait()
	if messages := drainMessages(t, w.outbound.messages); len(messages) != 1 {
		t.Fatalf("concurrent reporters sent %d exits, want 1", len(messages))
	}

	for i := 1; i <= standbyExitCacheSize; i++ {
		w.sendStandbyExit(fmt.Sprintf("sb-%d", i), 0, "")
		<-w.outbound.messages
	}
	if len(w.standbyExits) != standbyExitCacheSize || len(w.standbyExitIDs) != standbyExitCacheSize {
		t.Fatalf("cache retained %d IDs in map and %d in order, want %d", len(w.standbyExits), len(w.standbyExitIDs), standbyExitCacheSize)
	}
	if _, retained := w.standbyExits["sb-0"]; retained {
		t.Fatal("oldest standby exit was not evicted")
	}
	w.reportWatchedStandbyExit(fmt.Sprintf("sb-%d", standbyExitCacheSize), 0, "")
	if len(w.outbound.messages) != 0 {
		t.Fatal("latest standby exit was not deduplicated after cache eviction")
	}
}

func TestStandbyRejectsInvalidIDBeforeCreation(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../escape", `a\b`, "a/b", "UPPER", "under_score", strings.Repeat("a", 51)} {
		t.Run(id, func(t *testing.T) {
			backend := newStandbyTestBackend(false)
			w := newStandbyTestWorker(t, backend, Config{MaxPrewarmedSandboxes: 1})
			data, err := json.Marshal(standbyCreate(id))
			if err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(types.WebSocketMessage{Type: types.MessageTypePrewarmCreate, Data: data})
			if err != nil {
				t.Fatal(err)
			}
			w.handleMessage(wire, time.Now())
			messages := drainMessages(t, w.outbound.messages)
			if len(messages) != 1 || messages[0].Type != types.MessageTypePrewarmRejected {
				t.Fatalf("messages = %+v, want only prewarm_rejected", messages)
			}
			if !w.standbySemaphore.TryAcquire(1) {
				t.Fatal("invalid ID consumed a standby slot")
			}
			w.standbySemaphore.Release(1)
		})
	}
}

func TestStandbyAndTaskCapacity(t *testing.T) {
	t.Run("separate semaphores let a task run beside a standby", func(t *testing.T) {
		backend := newStandbyTestBackend(false)
		w := newStandbyTestWorker(t, backend, Config{MaxConcurrentTasks: 1, MaxPrewarmedSandboxes: 1})
		w.handlePrewarmCreate(standbyCreate("standby-sep"))
		awaitStandbyExecution(t, backend)

		w.handleTaskAssignment(executionAssignment("exec-sep"), time.Now())

		execution := awaitStandbyExecution(t, backend)
		if execution.params.TaskID != "run" {
			t.Fatalf("second execution TaskID = %q, want run", execution.params.TaskID)
		}
	})

	t.Run("shared semaphore makes a standby block a task", func(t *testing.T) {
		backend := newStandbyTestBackend(false)
		w := newStandbyTestWorker(t, backend, Config{MaxConcurrentTasks: 1})
		w.handlePrewarmCreate(standbyCreate("standby-shared"))
		awaitStandbyExecution(t, backend)

		w.handleTaskAssignment(executionAssignment("exec-shared"), time.Now())
		rejected := awaitStandbyMessage(t, w, types.MessageTypeTaskRejected)
		var msg types.TaskRejectedMessage
		if err := json.Unmarshal(rejected.Data, &msg); err != nil {
			t.Fatalf("failed to decode task rejected message: %v", err)
		}
		if msg.Reason != "worker at maximum concurrency" {
			t.Fatalf("task rejected reason = %q, want worker at maximum concurrency", msg.Reason)
		}

		backend.finish <- executeCompleted()
		awaitStandbyMessage(t, w, types.MessageTypePrewarmExited)
		w.taskWG.Wait()
		if !w.taskSemaphore.TryAcquire(1) {
			t.Fatal("shared slot was not released after the standby exited")
		}
		w.taskSemaphore.Release(1)
	})
}

func TestStandbyTaskParams(t *testing.T) {
	remapper, err := common.NewImageRemapper(map[string]string{
		"docker.io/warpdotdev/warp-agent": "mirror.example/agent:approved",
	})
	if err != nil {
		t.Fatalf("NewImageRemapper() error = %v", err)
	}
	backend := newStandbyTestBackend(false)
	w := newStandbyTestWorker(t, backend, Config{
		ServerRootURL:        "https://app.warp.dev",
		SidecarImageMappings: remapper,
		Kubernetes:           &KubernetesBackendConfig{DefaultImage: "registry.example/default:1"},
	})

	create := standbyCreate("standby-params")
	create.SidecarImage = "docker.io/warpdotdev/warp-agent:v1"
	create.AdditionalSidecars = []types.SidecarMount{{Image: "docker.io/other/tool:v2", MountPath: "/tool"}}
	create.EnvVars = map[string]string{"WARP_API_KEY": "secret-key", "FOO": "bar"}
	create.InstanceShape = &types.InstanceShape{Vcpus: 2, MemoryGb: 4}

	params := w.prepareStandbyTaskParams(create)

	if params.StandbyID != "standby-params" || params.TaskID != "" || params.ExecutionID != "" || params.Task != nil {
		t.Fatalf("identity = standby %q task %q execution %q task %v, want standby only", params.StandbyID, params.TaskID, params.ExecutionID, params.Task)
	}
	wantArgs := append(slices.Clone(create.Args), "--server-root-url", "https://app.warp.dev")
	if !reflect.DeepEqual(params.BaseArgs, wantArgs) {
		t.Fatalf("BaseArgs = %v, want %v", params.BaseArgs, wantArgs)
	}
	for _, want := range []string{
		"GIT_TERMINAL_PROMPT=0",
		"GH_PROMPT_DISABLED=1",
		"WARP_SERVER_ROOT_URL=https://app.warp.dev",
		"WARP_API_KEY=secret-key",
		"FOO=bar",
	} {
		if !slices.Contains(params.EnvVars, want) {
			t.Errorf("EnvVars missing %q: %v", want, params.EnvVars)
		}
	}
	for _, entry := range params.EnvVars {
		for _, forbidden := range []string{"TASK_ID=", "OZ_RUN_ID=", "WARP_RUN_ID=", "OZ_EXECUTION_ID=", "WARP_EXECUTION_ID="} {
			if strings.HasPrefix(entry, forbidden) {
				t.Errorf("EnvVars contains run identity variable %q", entry)
			}
		}
	}
	if params.DockerImage != "registry.example/default:1" {
		t.Errorf("DockerImage = %q, want the Kubernetes default image", params.DockerImage)
	}
	wantSidecars := []types.SidecarMount{
		{Image: "mirror.example/agent:approved", MountPath: "/agent"},
		{Image: "docker.io/other/tool:v2", MountPath: "/tool"},
	}
	if !reflect.DeepEqual(params.Sidecars, wantSidecars) {
		t.Errorf("Sidecars = %+v, want %+v", params.Sidecars, wantSidecars)
	}
	if params.InstanceShape == nil || *params.InstanceShape != *create.InstanceShape {
		t.Errorf("InstanceShape = %+v, want %+v", params.InstanceShape, create.InstanceShape)
	}
	if params.SetupEvents != nil || params.Logs != nil {
		t.Errorf("SetupEvents and Logs = %v, %v, want nil", params.SetupEvents, params.Logs)
	}

	create.DockerImage = "registry.example/requested:2"
	if got := w.prepareStandbyTaskParams(create).DockerImage; got != "registry.example/requested:2" {
		t.Errorf("DockerImage = %q, want the server-provided image", got)
	}
}

func TestStandbyBaseArgs(t *testing.T) {
	const (
		serverRootURL     = "https://app.warp.dev"
		sessionSharingURL = "wss://sessions.warp.dev"
	)
	base := []string{"agent", "run-cloud-standby", "--standby-id", "standby-args"}

	tests := []struct {
		name   string
		config Config
		args   []string
		want   []string
	}{
		{
			name:   "appends server URLs",
			config: Config{ServerRootURL: serverRootURL, SessionSharingServerURL: sessionSharingURL},
			args:   base,
			want:   []string{"agent", "run-cloud-standby", "--standby-id", "standby-args", "--server-root-url", serverRootURL, "--session-sharing-server-url", sessionSharingURL},
		},
		{
			name:   "appends nothing when config URLs are empty",
			config: Config{},
			args:   base,
			want:   []string{"agent", "run-cloud-standby", "--standby-id", "standby-args"},
		},
		{
			name:   "does not duplicate flags already in args",
			config: Config{ServerRootURL: serverRootURL, SessionSharingServerURL: sessionSharingURL},
			args:   []string{"agent", "--server-root-url", "https://server.example", "--session-sharing-server-url", "wss://server.example/sessions"},
			want:   []string{"agent", "--server-root-url", "https://server.example", "--session-sharing-server-url", "wss://server.example/sessions"},
		},
		{
			name:   "adds only the missing flag",
			config: Config{ServerRootURL: serverRootURL, SessionSharingServerURL: sessionSharingURL},
			args:   []string{"agent", "--server-root-url", "https://server.example"},
			want:   []string{"agent", "--server-root-url", "https://server.example", "--session-sharing-server-url", sessionSharingURL},
		},
		{
			name:   "keeps server-supplied args in order",
			config: Config{ServerRootURL: serverRootURL},
			args:   []string{"c", "b", "a"},
			want:   []string{"c", "b", "a", "--server-root-url", serverRootURL},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &Worker{config: tt.config}
			if got := w.standbyBaseArgs(tt.args); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("standbyBaseArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShutdownStopsStandbysUnlessPreserved(t *testing.T) {
	t.Run("standby is cancelled", func(t *testing.T) {
		backend := newStandbyTestBackend(false)
		w := newStandbyTestWorker(t, backend, Config{})
		w.handlePrewarmCreate(standbyCreate("standby-shutdown"))
		awaitStandbyExecution(t, backend)

		w.shutdownTasks()

		msg := awaitStandbyMessage(t, w, types.MessageTypePrewarmExited)
		exited := decodeStandbyExited(t, msg)
		if exited.Reason != string(metrics.TaskFailureReasonGracefulShutdown) {
			t.Fatalf("prewarm exited reason = %q, want graceful_shutdown", exited.Reason)
		}
	})

	t.Run("preserving backend keeps the standby running", func(t *testing.T) {
		backend := newStandbyTestBackend(true)
		w := newStandbyTestWorker(t, backend, Config{})
		w.handlePrewarmCreate(standbyCreate("standby-preserved"))
		awaitStandbyExecution(t, backend)

		w.shutdownTasks()

		select {
		case cause := <-backend.causes:
			if !errors.Is(cause, errObservationInterrupted) {
				t.Fatalf("execution context cause = %v, want errObservationInterrupted", cause)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("observation was not interrupted")
		}
		for _, queued := range drainMessages(t, w.outbound.messages) {
			if queued.Type == types.MessageTypePrewarmExited {
				t.Fatal("preserved standby sent prewarm_exited during shutdown")
			}
		}
	})
}
