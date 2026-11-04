package worker

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const standbyTestNamespace = "agents"

func newKubernetesStandbyTestBackend(t *testing.T, config KubernetesBackendConfig, objects ...runtime.Object) (*KubernetesBackend, *fake.Clientset, <-chan *batchv1.Job, *watch.RaceFreeFakeWatcher) {
	t.Helper()
	config.Namespace = standbyTestNamespace
	client := fake.NewSimpleClientset(objects...)
	created := make(chan *batchv1.Job, 4)
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		job.UID = "job-uid"
		created <- job.DeepCopy()
		return false, nil, nil
	})
	jobWatch := watch.NewRaceFreeFake()
	t.Cleanup(jobWatch.Stop)
	client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, jobWatch, nil
	})
	return &KubernetesBackend{config: config, clientset: client}, client, created, jobWatch
}

func standbyTestParams(sandboxID string) *TaskParams {
	return &TaskParams{
		StandbyID:   sandboxID,
		DockerImage: "ubuntu:22.04",
		EnvVars:     []string{"FOO=bar"},
		BaseArgs:    []string{"agent", "run-cloud-standby", "--standby-id", sandboxID},
	}
}

func executeTaskAsync(ctx context.Context, backend *KubernetesBackend, params *TaskParams) <-chan ExecuteResult {
	done := make(chan ExecuteResult, 1)
	go func() { done <- backend.ExecuteTask(ctx, params) }()
	return done
}

func awaitStandbyTestJob(t *testing.T, created <-chan *batchv1.Job) *batchv1.Job {
	t.Helper()
	select {
	case job := <-created:
		return job
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Job creation")
	}
	return nil
}

func awaitExecuteTaskResult(t *testing.T, done <-chan ExecuteResult) ExecuteResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ExecuteTask to return")
	}
	return ExecuteResult{}
}

func taskContainerEnv(t *testing.T, job *batchv1.Job) map[string]string {
	t.Helper()
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.Name == kubernetesTaskContainerName {
			env := make(map[string]string, len(container.Env))
			for _, entry := range container.Env {
				env[entry.Name] = entry.Value
			}
			return env
		}
	}
	t.Fatal("task container not found")
	return nil
}

func requireJobPresence(t *testing.T, client *fake.Clientset, name string, wantPresent bool) {
	t.Helper()
	_, err := client.BatchV1().Jobs(standbyTestNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if present := err == nil; present != wantPresent {
		t.Fatalf("Job %s present=%t, want %t (err=%v)", name, present, wantPresent, err)
	}
}

func TestKubernetesStandbyJobNameChecksDNSLabelLimit(t *testing.T) {
	maxID := strings.Repeat("a", 63-len("warp-prewarm-"))
	if name, err := kubernetesStandbyJobName(maxID); err != nil || name != "warp-prewarm-"+maxID {
		t.Fatalf("name=%q err=%v, want a valid 63-character name", name, err)
	}
	if _, err := kubernetesStandbyJobName(maxID + "a"); err == nil {
		t.Fatal("expected error for a 64-character Job name")
	}
	if _, err := kubernetesStandbyJobName("Sandbox_1"); err == nil {
		t.Fatal("expected error for a sandbox ID that is not a DNS-1123 label")
	}
}

func TestKubernetesStandbyExecuteCreatesLabeledJob(t *testing.T) {
	backend, client, created, _ := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{WorkerID: "Worker A", ExtraLabels: map[string]string{"team": "infra"}})
	podSelectors := make(chan string, 1)
	client.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
		podSelectors <- action.(k8stesting.WatchAction).GetWatchRestrictions().Labels.String()
		return false, nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	done := executeTaskAsync(ctx, backend, standbyTestParams("sb-1"))
	job := awaitStandbyTestJob(t, created)

	if job.Name != "warp-prewarm-sb-1" {
		t.Fatalf("job name = %q, want %q", job.Name, "warp-prewarm-sb-1")
	}
	wantLabels := map[string]string{
		kubernetesWorkerIDLabel:     "worker-a",
		kubernetesWorkerHashLabel:   kubernetesLabelHash("Worker A"),
		"warp-prewarmed-sandbox-id": "sb-1",
		"team":                      "infra",
	}
	if !reflect.DeepEqual(job.Labels, wantLabels) {
		t.Fatalf("job labels = %v, want %v", job.Labels, wantLabels)
	}
	if !reflect.DeepEqual(job.Spec.Template.Labels, wantLabels) {
		t.Fatalf("pod template labels = %v, want %v", job.Spec.Template.Labels, wantLabels)
	}

	env := taskContainerEnv(t, job)
	for _, name := range []string{"OZ_RUN_ID", "WARP_RUN_ID", "OZ_EXECUTION_ID", "WARP_EXECUTION_ID"} {
		if _, ok := env[name]; ok {
			t.Fatalf("standby container must not set %s", name)
		}
	}
	if env["OZ_WORKER_BACKEND"] != "kubernetes" {
		t.Fatalf("OZ_WORKER_BACKEND = %q, want kubernetes", env["OZ_WORKER_BACKEND"])
	}

	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds < 80 || *job.Spec.ActiveDeadlineSeconds > 90 {
		t.Fatalf("activeDeadlineSeconds = %v, want about 90 from the context deadline", job.Spec.ActiveDeadlineSeconds)
	}

	select {
	case selector := <-podSelectors:
		if selector != "warp-prewarmed-sandbox-id=sb-1" {
			t.Fatalf("pod watch selector = %q, want the standby label", selector)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the pod watch")
	}

	cancel()
	awaitExecuteTaskResult(t, done)
}

func TestKubernetesStandbyExecuteRejectsLongJobName(t *testing.T) {
	backend, _, created, _ := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{})
	result := backend.ExecuteTask(context.Background(), standbyTestParams(strings.Repeat("a", 51)))
	if result.Outcome != ExecuteOutcomeError || result.Error == nil {
		t.Fatalf("outcome = %v, error = %v; want an error outcome", result.Outcome, result.Error)
	}
	select {
	case job := <-created:
		t.Fatalf("unexpected Job %s", job.Name)
	default:
	}
}

func TestKubernetesStandbyActiveDeadlineSeconds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		timeout    time.Duration
		configured *int64
		wantNil    bool
		wantMin    int64
		wantMax    int64
	}{
		{name: "no deadline and no configured limit", wantNil: true},
		{name: "no deadline keeps configured limit", configured: int64Ptr(300), wantMin: 300, wantMax: 300},
		{name: "deadline applies without configured limit", timeout: 90 * time.Second, wantMin: 80, wantMax: 90},
		{name: "configured limit below deadline wins", timeout: 90 * time.Second, configured: int64Ptr(30), wantMin: 30, wantMax: 30},
		{name: "deadline below configured limit wins", timeout: 90 * time.Second, configured: int64Ptr(300), wantMin: 80, wantMax: 90},
		{name: "expired deadline clamps to one second", timeout: time.Millisecond, wantMin: 1, wantMax: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
				time.Sleep(2 * time.Millisecond)
			}
			got := kubernetesStandbyActiveDeadlineSeconds(ctx, tc.configured)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got %d, want nil", *got)
				}
				return
			}
			if got == nil || *got < tc.wantMin || *got > tc.wantMax {
				t.Fatalf("got %v, want between %d and %d", got, tc.wantMin, tc.wantMax)
			}
		})
	}
}

func int64Ptr(value int64) *int64 {
	return &value
}

func TestKubernetesStandbyCancellation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cause        error
		wantPreserve bool
	}{
		{name: "explicit cancellation deletes job"},
		{name: "worker shutdown preserves job", cause: errObservationInterrupted, wantPreserve: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, client, created, _ := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{})
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			done := executeTaskAsync(ctx, backend, standbyTestParams("sb-1"))
			job := awaitStandbyTestJob(t, created)

			cancel(tc.cause)
			awaitExecuteTaskResult(t, done)
			requireJobPresence(t, client, job.Name, tc.wantPreserve)
		})
	}
}

func TestKubernetesStandbyCompletionLeavesJobForExitWatcher(t *testing.T) {
	backend, client, created, jobWatch := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{WorkerID: "worker-a"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	done := executeTaskAsync(ctx, backend, standbyTestParams("sb-1"))
	job := awaitStandbyTestJob(t, created)

	completed := job.DeepCopy()
	completed.Status.Conditions = []batchv1.JobCondition{standbyCompleteCondition()}
	jobWatch.Modify(completed)

	result := awaitExecuteTaskResult(t, done)
	if result.Outcome != ExecuteOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", result.Outcome)
	}
	requireJobPresence(t, client, job.Name, true)
}

func TestKubernetesTaskJobIsUnchangedByStandbySupport(t *testing.T) {
	deadline := int64(600)
	backend, client, created, _ := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{WorkerID: "worker-a", ActiveDeadlineSeconds: &deadline})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	done := executeTaskAsync(ctx, backend, &TaskParams{
		TaskID:      "task-1",
		ExecutionID: "exec-1",
		DockerImage: "ubuntu:22.04",
		EnvVars:     []string{"FOO=bar"},
		BaseArgs:    []string{"agent"},
	})
	job := awaitStandbyTestJob(t, created)

	if job.Name != kubernetesTaskJobName("task-1", "exec-1") {
		t.Fatalf("job name = %q, want the task Job name", job.Name)
	}
	if _, ok := job.Labels[StandbyKubernetesLabel]; ok {
		t.Fatal("task Job must not carry the standby label")
	}
	if job.Labels[kubernetesTaskIDLabel] != "task-1" {
		t.Fatalf("task label = %q, want task-1", job.Labels[kubernetesTaskIDLabel])
	}
	if env := taskContainerEnv(t, job); env["OZ_RUN_ID"] != "task-1" {
		t.Fatalf("OZ_RUN_ID = %q, want task-1", env["OZ_RUN_ID"])
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Fatalf("activeDeadlineSeconds = %v, want the configured 600", job.Spec.ActiveDeadlineSeconds)
	}

	cancel()
	awaitExecuteTaskResult(t, done)
	requireJobPresence(t, client, job.Name, true)
}

func standbyCompleteCondition() batchv1.JobCondition {
	return batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
}

func standbyFailedCondition(reason string) batchv1.JobCondition {
	return batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason}
}

func standbyExitTestJob(name, uid, sandboxID string, conditions ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: standbyTestNamespace,
			UID:       k8stypes.UID(uid),
			Labels: map[string]string{
				StandbyKubernetesLabel:  sandboxID,
				kubernetesWorkerIDLabel: "worker-a",
			},
		},
		Status: batchv1.JobStatus{Conditions: conditions},
	}
}

func standbyExitTestPod(jobName, jobUID, sandboxID string, exitCode int32) *corev1.Pod {
	isController := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-pod",
			Namespace: standbyTestNamespace,
			Labels:    map[string]string{StandbyKubernetesLabel: sandboxID},
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "Job",
				Name:       jobName,
				UID:        k8stypes.UID(jobUID),
				Controller: &isController,
			}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  kubernetesTaskContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode}},
			}},
		},
	}
}

type standbyExitReport struct {
	sandboxID string
	exitCode  int
	reason    string
}

func startStandbyExitWatcher(backend *KubernetesBackend) (<-chan standbyExitReport, context.CancelFunc, <-chan struct{}) {
	reports := make(chan standbyExitReport, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		backend.WatchStandbyExits(ctx, func(sandboxID string, exitCode int, reason string) {
			reports <- standbyExitReport{sandboxID: sandboxID, exitCode: exitCode, reason: reason}
		})
	}()
	return reports, cancel, done
}

func awaitStandbyExit(t *testing.T, reports <-chan standbyExitReport) standbyExitReport {
	t.Helper()
	select {
	case report := <-reports:
		return report
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a standby exit report")
	}
	return standbyExitReport{}
}

func TestKubernetesStandbyWatcherReportsEachJobOnce(t *testing.T) {
	first := standbyExitTestJob("warp-prewarm-sb-1", "job-1", "sb-1", standbyCompleteCondition())
	second := standbyExitTestJob("warp-prewarm-sb-2", "job-2", "sb-2", standbyFailedCondition("BackoffLimitExceeded"))
	backend, client, _, jobWatch := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{WorkerID: "worker-a"},
		first, second,
		standbyExitTestPod(first.Name, "job-1", "sb-1", 0),
		standbyExitTestPod(second.Name, "job-2", "sb-2", 2),
	)
	reports, cancel, done := startStandbyExitWatcher(backend)
	defer cancel()

	jobWatch.Add(first)
	jobWatch.Modify(first)
	jobWatch.Add(second)

	if got := awaitStandbyExit(t, reports); got != (standbyExitReport{"sb-1", 0, ""}) {
		t.Fatalf("first report = %+v, want success for sb-1", got)
	}
	if got := awaitStandbyExit(t, reports); got != (standbyExitReport{"sb-2", 2, "job_failed"}) {
		t.Fatalf("second report = %+v, want failure with exit 2 for sb-2", got)
	}
	waitFor(t, 2*time.Second, func() bool {
		_, firstErr := client.BatchV1().Jobs(standbyTestNamespace).Get(context.Background(), first.Name, metav1.GetOptions{})
		_, secondErr := client.BatchV1().Jobs(standbyTestNamespace).Get(context.Background(), second.Name, metav1.GetOptions{})
		return apierrors.IsNotFound(firstErr) && apierrors.IsNotFound(secondErr)
	})

	cancel()
	<-done
	if len(reports) != 0 {
		t.Fatalf("got %d unexpected duplicate reports", len(reports))
	}
}

func TestKubernetesStandbyWatcherReportsDeadlineWithoutPod(t *testing.T) {
	job := standbyExitTestJob("warp-prewarm-sb-1", "job-1", "sb-1", standbyFailedCondition("DeadlineExceeded"))
	backend, _, _, jobWatch := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{WorkerID: "worker-a"}, job)
	reports, cancel, done := startStandbyExitWatcher(backend)
	defer cancel()

	jobWatch.Add(job)

	if got := awaitStandbyExit(t, reports); got != (standbyExitReport{"sb-1", 1, "active_deadline"}) {
		t.Fatalf("report = %+v, want active_deadline with fallback exit 1", got)
	}
	cancel()
	<-done
}

func TestKubernetesStandbyFailedJobDoesNotReportSuccess(t *testing.T) {
	job := standbyExitTestJob("warp-prewarm-sb-1", "job-1", "sb-1", standbyFailedCondition("DeadlineExceeded"))
	backend, _, _, jobWatch := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{WorkerID: "worker-a"}, job,
		standbyExitTestPod(job.Name, "job-1", "sb-1", 0))

	reports, cancel, done := startStandbyExitWatcher(backend)
	defer cancel()
	jobWatch.Add(job)
	if got := awaitStandbyExit(t, reports); got != (standbyExitReport{"sb-1", 1, "active_deadline"}) {
		t.Fatalf("report = %+v, want failure despite task container exit 0", got)
	}
	cancel()
	<-done
}

func TestKubernetesStandbyWatcherCancellationStopsCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := standbyExitTestJob("warp-prewarm-sb-1", "job-1", "sb-1", standbyCompleteCondition())
		backend, client, _, jobWatch := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{WorkerID: "worker-a"}, job)
		var cancel context.CancelFunc
		deletes := 0
		client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
			deletes++
			cancel()
			return true, nil, apierrors.NewServiceUnavailable("temporarily unavailable")
		})
		_, cancel, done := startStandbyExitWatcher(backend)
		defer cancel()
		jobWatch.Add(job)
		<-done
		if deletes != 1 {
			t.Fatalf("cleanup made %d delete attempts after cancellation, want 1", deletes)
		}
	})
}

func TestKubernetesStandbyWatcherReopensClosedWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := standbyExitTestJob("warp-prewarm-sb-1", "job-1", "sb-1", standbyCompleteCondition())
		backend, client, _, _ := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{WorkerID: "worker-a"}, job,
			standbyExitTestPod(job.Name, "job-1", "sb-1", 0))

		closed := watch.NewRaceFreeFake()
		closed.Stop()
		live := watch.NewRaceFreeFake()
		live.Add(job)
		watches := []watch.Interface{closed, live}
		client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
			next := watches[0]
			if len(watches) > 1 {
				watches = watches[1:]
			}
			return true, next, nil
		})

		reports, cancel, done := startStandbyExitWatcher(backend)
		defer cancel()

		synctest.Wait()
		time.Sleep(kubernetesStandbyWatchRetryInterval)
		if got := awaitStandbyExit(t, reports); got != (standbyExitReport{"sb-1", 0, ""}) {
			t.Fatalf("report = %+v, want success for sb-1 after reopen", got)
		}
		cancel()
		<-done
	})
}

func TestKubernetesStandbyCancellationDeletesByLabel(t *testing.T) {
	target := standbyExitTestJob("warp-prewarm-sb-1", "job-1", "sb-1")
	other := standbyExitTestJob("warp-prewarm-sb-2", "job-2", "sb-2")
	backend, client, _, _ := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{WorkerID: "worker-a"}, target, other)
	ctx := context.Background()

	if err := backend.CancelTask(ctx, &CancelParams{StandbyID: "sb-1", TaskID: "run-1", ExecutionID: "exec-1"}); err == nil {
		t.Fatal("mixed task and standby identifiers were accepted")
	}
	requireJobPresence(t, client, target.Name, true)

	if err := backend.CancelTask(ctx, &CancelParams{StandbyID: "sb-1"}); err != nil {
		t.Fatalf("CancelTask(sb-1) error: %v", err)
	}
	requireJobPresence(t, client, target.Name, false)
	requireJobPresence(t, client, other.Name, true)

	if err := backend.CancelTask(ctx, &CancelParams{StandbyID: "sb-1"}); err != nil {
		t.Fatalf("second CancelTask(sb-1) error: %v", err)
	}
	if err := backend.CancelTask(ctx, &CancelParams{StandbyID: "missing"}); err != nil {
		t.Fatalf("CancelTask(missing) error: %v", err)
	}
}
