package worker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

func sharedPodTestBackend(t *testing.T) (*KubernetesBackend, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		job.UID = k8stypes.UID(job.Name)
		return false, nil, nil
	})
	client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, watch.NewRaceFreeFake(), nil
	})
	return &KubernetesBackend{
		config:    KubernetesBackendConfig{Namespace: standbyTestNamespace, WorkerID: "worker-a"},
		clientset: client,
	}, client
}

func TestKubernetesPodNotificationsStayWithinControllingUID(t *testing.T) {
	pods := &kubernetesPodObservation{subscribers: make(map[k8stypes.UID]map[chan struct{}]struct{})}
	first, detachFirst := pods.subscribe("first")
	defer detachFirst()
	second, detachSecond := pods.subscribe("second")
	defer detachSecond()
	<-first
	<-second
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
		Kind: "Job", Name: "same-name", UID: "first", Controller: workerBoolPtr(true),
	}}}}
	for range 1000 {
		pods.notify(pod)
	}
	select {
	case <-first:
	default:
		t.Fatal("controlling UID did not receive notification")
	}
	select {
	case <-second:
		t.Fatal("notification was broadcast to another UID")
	default:
	}
	pods.notify(cache.DeletedFinalStateUnknown{Obj: pod})
	select {
	case <-first:
	default:
		t.Fatal("tombstone did not notify controlling UID")
	}
}

func executionTestPod(backend *KubernetesBackend, id string, standby bool) *corev1.Pod {
	name := kubernetesTaskJobName(id, id)
	labels := backend.baseLabels(id, id)
	if standby {
		name = StandbyInstanceRef(id)
		labels = backend.kubernetesStandbyLabels(id)
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, UID: k8stypes.UID(name)}}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-pod", Namespace: standbyTestNamespace, UID: k8stypes.UID(name + "-pod"),
			Labels: labels, CreationTimestamp: metav1.NewTime(time.Now()),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestKubernetesConcurrentExecutionsSharePodWatchAndCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backend, client := sharedPodTestBackend(t)
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(errObservationInterrupted)
		exitCtx, cancelExits := context.WithCancel(ctx)
		defer cancelExits()
		exitDone := make(chan struct{})
		go func() {
			defer close(exitDone)
			backend.WatchStandbyExits(exitCtx, func(string, int, string) { t.Error("healthy standby reported an exit") })
		}()
		var results []<-chan ExecuteResult
		var taskPods []*corev1.Pod
		for i := range 12 {
			id := fmt.Sprintf("execution-%d", i)
			standby := i%2 == 1
			pod := executionTestPod(backend, id, standby)
			if _, err := client.CoreV1().Pods(standbyTestNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			taskPods = append(taskPods, pod)
			params := &TaskParams{TaskID: id, DockerImage: "ubuntu:22.04"}
			if standby {
				params = standbyTestParams(id)
			}
			results = append(results, executeTaskAsync(ctx, backend, params))
		}
		time.Sleep(time.Second)
		synctest.Wait()
		before := len(client.Actions())
		time.Sleep(2 * watchSafetyInterval)
		synctest.Wait()
		lists, watches, gets := 0, 0, 0
		for i, action := range client.Actions() {
			if action.Matches("list", "pods") {
				lists++
			}
			if action.Matches("watch", "pods") {
				watches++
			}
			if i >= before && action.Matches("get", "jobs") {
				gets++
			}
		}
		if lists != 1 || watches != 1 || gets < 24 {
			t.Fatalf("Pod LISTs=%d WATCHes=%d, safety Job GETs=%d; want 1, 1, >=24", lists, watches, gets)
		}
		cancelExits()
		<-exitDone
		// A same-name replacement owner must not poison an active execution.
		foreign := taskPods[0].DeepCopy()
		foreign.Name += "-replacement"
		foreign.OwnerReferences[0].UID = "replacement"
		foreign.Status.Phase = corev1.PodFailed
		if _, err := client.CoreV1().Pods(standbyTestNamespace).Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		for _, result := range results {
			select {
			case got := <-result:
				t.Fatalf("unrelated Pod ended an execution: %+v", got)
			default:
			}
		}
		failed := taskPods[1].DeepCopy()
		failed.Status.Phase = corev1.PodFailed
		failed.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  kubernetesTaskContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 42}},
		}}
		if _, err := client.CoreV1().Pods(standbyTestNamespace).UpdateStatus(ctx, failed, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		got := awaitExecuteTaskResult(t, results[1])
		if failureExitCode(got.Error) != 42 {
			t.Fatalf("standby failure lost exit 42: %+v", got)
		}
		for i, result := range results {
			if i == 1 {
				continue
			}
			select {
			case got := <-result:
				t.Fatalf("Pod update for one UID ended execution %d: %+v", i, got)
			default:
			}
		}
		cancel(errObservationInterrupted)
		for i, result := range results {
			if i != 1 {
				awaitExecuteTaskResult(t, result)
			}
		}
		if backend.podObservation != nil {
			t.Fatal("last consumer did not stop shared observation")
		}
	})
}

func TestKubernetesCachedPodFailuresWithoutUpdates(t *testing.T) {
	for _, mode := range []string{"cached exit", "unschedulable timeout", "new mount event"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				backend, client := sharedPodTestBackend(t)
				timeout := 20 * time.Second
				backend.config.UnschedulableTimeout = &timeout
				pod := executionTestPod(backend, "task", false)
				pod.Status.Phase = corev1.PodPending
				want := "unschedulable"
				switch mode {
				case "cached exit":
					pod.Status.Phase = corev1.PodFailed
					pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
						Name:  kubernetesTaskContainerName,
						State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 42}},
					}}
					want = "exited with code 42"
				case "unschedulable timeout":
					pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
				case "new mount event":
					want = "failed to mount"
				}
				if _, err := client.CoreV1().Pods(standbyTestNamespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				// Keep the informer warm so the execution subscribes after this
				// Pod is already cached, with no subsequent Pod event.
				pods, release := backend.acquirePodObservation(t.Context())
				defer release()
				time.Sleep(time.Second)
				synctest.Wait()
				if !pods.HasSynced() || len(pods.podsForJob(k8stypes.UID(kubernetesTaskJobName("task", "task")))) != 1 {
					t.Fatal("Pod was not cached before execution")
				}
				done := executeTaskAsync(t.Context(), backend, &TaskParams{TaskID: "task", DockerImage: "ubuntu:22.04"})
				synctest.Wait()
				if mode != "cached exit" {
					select {
					case got := <-done:
						t.Fatalf("failed before timeout/event: %+v", got)
					default:
					}
				}
				if mode == "new mount event" {
					event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "mount", Namespace: standbyTestNamespace},
						InvolvedObject: corev1.ObjectReference{UID: pod.UID}, Reason: "FailedMount", Type: corev1.EventTypeWarning}
					if _, err := client.CoreV1().Events(standbyTestNamespace).Create(t.Context(), event, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				if mode != "cached exit" {
					time.Sleep(watchSafetyInterval)
					synctest.Wait()
				}
				got := awaitExecuteTaskResult(t, done)
				if got.Error == nil || !strings.Contains(got.Error.Error(), want) {
					t.Fatalf("result=%+v, want %q", got, want)
				}
			})
		})
	}
}
