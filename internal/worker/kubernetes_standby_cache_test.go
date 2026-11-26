package worker

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"
)

func TestKubernetesStandbyWatcherUsesCachedPodsFor100Jobs(t *testing.T) {
	var objects []runtime.Object
	var jobs []*batchv1.Job
	for i := range 100 {
		id := fmt.Sprintf("sb-%d", i)
		job := standbyExitTestJob("warp-prewarm-"+id, fmt.Sprintf("job-%d", i), id, standbyFailedCondition("BackoffLimitExceeded"))
		pod := standbyExitTestPod(job.Name, string(job.UID), id, 42)
		pod.Status.ContainerStatuses[0].State.Terminated.Signal = 9
		unowned := pod.DeepCopy()
		unowned.Name += "-unowned"
		unowned.OwnerReferences[0].UID = "another-job"
		unowned.Status.ContainerStatuses[0].State.Terminated.Signal = 15
		objects = append(objects, job, pod, unowned)
		jobs = append(jobs, job)
	}
	taskPod := standbyExitTestPod("task-job", "task-job-uid", "", 7)
	delete(taskPod.Labels, StandbyKubernetesLabel)
	objects = append(objects, taskPod)
	backend, client, _, jobWatch := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{WorkerID: "worker-a"}, objects...)
	podWatch := watch.NewRaceFreeFake()
	client.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
		want := kubernetesWorkerHashLabel + "=" + kubernetesLabelHash("worker-a") + "," + StandbyKubernetesLabel
		if action.GetNamespace() != standbyTestNamespace || action.(k8stesting.WatchAction).GetWatchRestrictions().Labels.String() != want {
			t.Errorf("Pod watch not scoped to namespace, worker, and standby Pods: %+v", action)
		}
		return true, podWatch, nil
	})
	var reported []string
	var deleted []string
	client.PrependReactor("delete", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deletion := action.(k8stesting.DeleteAction)
		index := len(deleted)
		if index >= len(reported) || reported[index] != deletion.GetName() {
			t.Errorf("Job %s deleted before its report", deletion.GetName())
		}
		if index >= len(jobs) {
			t.Errorf("unexpected duplicate Job deletion: %s", deletion.GetName())
		} else if uid := deletion.GetDeleteOptions().Preconditions; uid == nil || uid.UID == nil || *uid.UID != jobs[index].UID {
			t.Errorf("deletion missing expected Job UID: %+v", deletion)
		}
		deleted = append(deleted, deletion.GetName())
		return false, nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	received := make(chan standbyExitReport)
	go func() {
		defer close(done)
		backend.WatchStandbyExits(ctx, func(id string, code int, reason string) {
			reported = append(reported, "warp-prewarm-"+id)
			select {
			case received <- standbyExitReport{id, code, reason}:
			case <-ctx.Done():
			}
		})
	}()
	for _, job := range jobs {
		jobWatch.Add(job)
		jobWatch.Modify(job)
		want := standbyExitReport{job.Labels[StandbyKubernetesLabel], 137, "job_failed"}
		if got := awaitStandbyExit(t, received); got != want {
			t.Fatalf("report = %+v, want %+v", got, want)
		}
	}
	waitFor(t, 2*time.Second, func() bool {
		_, err := client.BatchV1().Jobs(standbyTestNamespace).Get(ctx, jobs[99].Name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
	cancel()
	<-done
	if len(reported) != 100 || len(deleted) != 100 {
		t.Fatalf("reports=%d deletions=%d, want 100 each", len(reported), len(deleted))
	}
	lists := 0
	for _, action := range client.Actions() {
		if action.Matches("list", "pods") {
			lists++
			selector := action.(k8stesting.ListAction).GetListRestrictions().Labels
			foreign := labels.Set{
				StandbyKubernetesLabel:    "sb-foreign",
				kubernetesWorkerHashLabel: kubernetesLabelHash("worker-b"),
			}
			if action.GetNamespace() != standbyTestNamespace || selector.Matches(labels.Set(taskPod.Labels)) ||
				selector.Matches(foreign) || !selector.Matches(labels.Set(backend.kubernetesStandbyLabels("sb-0"))) {
				t.Errorf("Pod list not scoped to namespace, worker, and standby Pods: %+v", action)
			}
		}
	}
	if lists != 1 {
		t.Fatalf("Pod LISTs = %d, want one informer initialization and zero per-exit reads", lists)
	}
	if !podWatch.IsStopped() {
		t.Fatal("Pod informer watch still running after exit watcher shutdown")
	}
}

func TestKubernetesStandbyWatcherRefreshesStalePodStatus(t *testing.T) {
	job := standbyExitTestJob("warp-prewarm-sb-1", "job-1", "sb-1", standbyFailedCondition("BackoffLimitExceeded"))
	pod := standbyExitTestPod(job.Name, string(job.UID), "sb-1", 42)
	backend, client, _, jobWatch := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{WorkerID: "worker-a"}, job, pod)
	lists := 0
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists++
		if lists == 1 {
			stale := pod.DeepCopy()
			stale.Status.ContainerStatuses = nil
			return true, &corev1.PodList{Items: []corev1.Pod{*stale}}, nil
		}
		return false, nil, nil
	})
	podWatch := watch.NewRaceFreeFake()
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, podWatch, nil
	})
	reports, cancel, done := startStandbyExitWatcher(backend)
	defer cancel()
	jobWatch.Add(job)
	if got := awaitStandbyExit(t, reports); got != (standbyExitReport{"sb-1", 42, "job_failed"}) {
		t.Fatalf("report = %+v, want live task exit 42", got)
	}
	cancel()
	<-done
	if lists != 2 {
		t.Fatalf("Pod LISTs = %d, want initialization plus one live recovery", lists)
	}
}

func TestKubernetesStandbyWatcherBoundsUnsyncedCacheAndLiveLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := standbyExitTestJob("warp-prewarm-sb-1", "job-1", "sb-1", standbyFailedCondition("DeadlineExceeded"))
		backend, client, _, jobWatch := newKubernetesStandbyTestBackend(t, KubernetesBackendConfig{WorkerID: "worker-a"}, job)
		client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewServiceUnavailable("unavailable")
		})
		start := time.Now()
		reports, cancel, done := startStandbyExitWatcher(backend)
		defer cancel()
		jobWatch.Add(job)
		select {
		case got := <-reports:
			if got != (standbyExitReport{"sb-1", 1, "active_deadline"}) {
				t.Fatalf("report = %+v, want deadline fallback", got)
			}
		case <-time.After(2*kubernetesAPIRequestTimeout + time.Second):
			t.Fatal("cache sync or live lookup exceeded bounded wait")
		}
		if elapsed := time.Since(start); elapsed > 2*kubernetesAPIRequestTimeout {
			t.Fatalf("report took %s, want at most two request timeouts", elapsed)
		}
		cancel()
		<-done
	})
}
