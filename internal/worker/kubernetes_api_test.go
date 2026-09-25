package worker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func TestRetryKubernetesAPI(t *testing.T) {
	t.Run("recovers from transient errors", func(t *testing.T) {
		calls := 0
		value, err := retryKubernetesAPI(context.Background(), wait.Backoff{Steps: 3}, func() (string, error) {
			calls++
			if calls < 3 {
				return "", apierrors.NewServiceUnavailable("temporarily unavailable")
			}
			return "recovered", nil
		})
		if err != nil || value != "recovered" || calls != 3 {
			t.Fatalf("value=%q err=%v calls=%d", value, err, calls)
		}
	})

	t.Run("exhausts retries preserving the last error", func(t *testing.T) {
		calls := 0
		lastErr := apierrors.NewTooManyRequests("throttled", 0)
		_, err := retryKubernetesAPI(context.Background(), wait.Backoff{Steps: 3}, func() (struct{}, error) {
			calls++
			return struct{}{}, lastErr
		})
		if calls != 3 || !errors.Is(err, lastErr) || !wait.Interrupted(err) {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})

	t.Run("does not retry forbidden errors", func(t *testing.T) {
		calls := 0
		forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "jobs"}, "task", errors.New("denied"))
		_, err := retryKubernetesAPI(context.Background(), wait.Backoff{Steps: 3}, func() (struct{}, error) {
			calls++
			return struct{}{}, forbidden
		})
		if calls != 1 || !apierrors.IsForbidden(err) {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})

	t.Run("cancellation interrupts backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		_, err := retryKubernetesAPI(ctx, wait.Backoff{Duration: time.Hour, Steps: 3}, func() (struct{}, error) {
			calls++
			cancel()
			return struct{}{}, io.EOF
		})
		if calls != 1 || !errors.Is(err, context.Canceled) {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})

	t.Run("cancelled context does not issue a request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := retryKubernetesAPI(ctx, wait.Backoff{Steps: 3}, func() (struct{}, error) {
			t.Fatal("request should not be attempted")
			return struct{}{}, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestIsTransientKubernetesAPIError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "timeout", err: apierrors.NewTimeoutError("timeout", 0), want: true},
		{name: "server timeout", err: apierrors.NewServerTimeout(schema.GroupResource{}, "get", 0), want: true},
		{name: "internal error", err: apierrors.NewInternalError(errors.New("temporary")), want: true},
		{name: "EOF", err: io.EOF, want: true},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF, want: true},
		{name: "connection reset", err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, want: true},
		{name: "connection refused", err: syscall.ECONNREFUSED, want: true},
		{name: "request deadline", err: context.DeadlineExceeded, want: true},
		{name: "cancelled", err: context.Canceled},
		{name: "missing job", err: apierrors.NewNotFound(schema.GroupResource{Resource: "jobs"}, "task")},
		{name: "unauthorized", err: apierrors.NewUnauthorized("denied")},
		{name: "invalid request", err: apierrors.NewBadRequest("invalid")},
		{name: "unknown error", err: errors.New("unexpected")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientKubernetesAPIError(tc.err); got != tc.want {
				t.Fatalf("transient=%t, want %t for %v", got, tc.want, tc.err)
			}
		})
	}
}

func TestCleanupFailedJob(t *testing.T) {
	for _, tc := range []struct {
		name             string
		condition        batchv1.JobConditionType
		observedTerminal bool
		noCleanup        bool
		missing          bool
		replaced         bool
		getForbidden     bool
		deleteForbidden  bool
		wantDeleted      bool
		wantError        bool
	}{
		{name: "deletes active job", wantDeleted: true},
		{name: "deletes active job even with cleanup disabled", noCleanup: true, wantDeleted: true},
		{name: "retains observed failed job", condition: batchv1.JobFailed, observedTerminal: true},
		{name: "retains newly failed job", condition: batchv1.JobFailed},
		{name: "retains newly completed job", condition: batchv1.JobComplete},
		{name: "retains terminal job with cleanup disabled", condition: batchv1.JobFailed, noCleanup: true},
		{name: "missing job is already stopped", missing: true, wantDeleted: true},
		{name: "does not delete replacement", replaced: true, wantError: true},
		{name: "deletes original UID when refresh fails", getForbidden: true, wantDeleted: true},
		{name: "reports deletion failure", deleteForbidden: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			observed := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "task-exec-1", Namespace: "agents", UID: "original"}}
			stored := observed.DeepCopy()
			if tc.condition != "" {
				stored.Status.Conditions = []batchv1.JobCondition{{Type: tc.condition, Status: corev1.ConditionTrue}}
				if tc.observedTerminal {
					observed = stored.DeepCopy()
				}
			}
			if tc.replaced {
				stored.UID = "replacement"
			}
			sibling := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "task-exec-2", Namespace: "agents", UID: "sibling"}}
			client := fake.NewSimpleClientset(sibling)
			if !tc.missing {
				if _, err := client.BatchV1().Jobs("agents").Create(ctx, stored, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.getForbidden {
				client.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "jobs"}, observed.Name, errors.New("denied"))
				})
			}
			client.PrependReactor("delete", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
				opts := action.(k8stesting.DeleteAction).GetDeleteOptions()
				if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != observed.UID {
					t.Error("deletion must be conditional on the original Job UID")
				}
				if opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationBackground {
					t.Error("deletion must cascade to task Pods")
				}
				if tc.deleteForbidden {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "jobs"}, observed.Name, errors.New("denied"))
				}
				return false, nil, nil
			})
			backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents", NoCleanup: tc.noCleanup}, clientset: client}
			err := backend.cleanupFailedJob(ctx, observed)
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v, wantError=%t", err, tc.wantError)
			}
			_, err = client.Tracker().Get(batchv1.SchemeGroupVersion.WithResource("jobs"), "agents", observed.Name)
			if apierrors.IsNotFound(err) != tc.wantDeleted {
				t.Fatalf("get after cleanup=%v, wantDeleted=%t", err, tc.wantDeleted)
			}
			if _, err := client.Tracker().Get(batchv1.SchemeGroupVersion.WithResource("jobs"), "agents", sibling.Name); err != nil {
				t.Fatalf("another execution's Job was affected: %v", err)
			}
		})
	}
}

func TestKubernetesAPIOperationsRetry(t *testing.T) {
	for _, operation := range []string{"get", "list", "watch jobs", "watch pods", "delete"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "agents", UID: "original"}}
			client := fake.NewSimpleClientset(job)
			backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}, clientset: client}
			calls := 0
			if strings.HasPrefix(operation, "watch ") {
				watcher := watch.NewRaceFreeFake()
				defer watcher.Stop()
				client.PrependWatchReactor(strings.TrimPrefix(operation, "watch "), func(k8stesting.Action) (bool, watch.Interface, error) {
					calls++
					if calls == 1 {
						return true, nil, apierrors.NewServiceUnavailable("temporary")
					}
					return true, watcher, nil
				})
				var got watch.Interface
				var err error
				if operation == "watch jobs" {
					got, err = backend.watchJob(ctx, job.Name)
				} else {
					got, err = backend.watchTaskPods(ctx, "exec")
				}
				if err != nil || got == nil {
					t.Fatalf("watch=%v err=%v", got, err)
				}
				defer got.Stop()
				watcher.Add(job)
				if _, open := <-got.ResultChan(); !open {
					t.Fatal("successful watch was closed by retry setup")
				}
			} else {
				resourceName := "jobs"
				if operation == "list" {
					resourceName = "pods"
				}
				client.PrependReactor(operation, resourceName, func(k8stesting.Action) (bool, runtime.Object, error) {
					calls++
					if calls == 1 {
						return true, nil, apierrors.NewServiceUnavailable("temporary")
					}
					return false, nil, nil
				})
				var err error
				switch operation {
				case "get":
					_, err = backend.getTaskJob(ctx, job.Name)
				case "list":
					_, err = backend.listTaskPods(ctx, "exec")
				case "delete":
					err = backend.deleteTaskJob(ctx, job.Name, &job.UID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if calls != 2 {
				t.Fatalf("calls=%d, want 2", calls)
			}
		})
	}
}

func TestExecuteTaskStopsActiveJobOnFailure(t *testing.T) {
	for _, noCleanup := range []bool{false, true} {
		for _, failure := range []string{"unschedulable", "init exit", "watch forbidden", "delete forbidden"} {
			t.Run(failure+map[bool]string{false: "/cleanup", true: "/no cleanup"}[noCleanup], func(t *testing.T) {
				client := fake.NewSimpleClientset()
				jobWatch := watch.NewRaceFreeFake()
				podWatch := watch.NewRaceFreeFake()
				defer jobWatch.Stop()
				defer podWatch.Stop()
				var created *batchv1.Job
				client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
					created = action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
					return false, nil, nil
				})
				client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
					if failure == "watch forbidden" {
						return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "jobs"}, created.Name, errors.New("denied"))
					}
					return true, jobWatch, nil
				})
				client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
					pod := &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{Name: "task-pod", Namespace: "agents", CreationTimestamp: metav1.NewTime(time.Now().Add(-11 * time.Minute))},
						Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
							Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
						}}},
					}
					if failure == "init exit" {
						pod.Status.Conditions = nil
						pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
							Name: "copy-sidecar-0", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
						}}
					}
					podWatch.Add(pod)
					return true, podWatch, nil
				})
				if failure == "delete forbidden" {
					client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "jobs"}, "task", errors.New("denied"))
					})
				}
				backend := &KubernetesBackend{
					config:    KubernetesBackendConfig{Namespace: "agents", NoCleanup: noCleanup, UnschedulableTimeout: durationPtr(defaultUnschedulableFailureDelay)},
					clientset: client,
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				result := backend.ExecuteTask(ctx, &TaskParams{TaskID: "task", ExecutionID: "exec", DockerImage: "ubuntu:22.04"})
				if result.Outcome != ExecuteOutcomeError || result.Error == nil {
					t.Fatalf("expected failure, got %+v", result)
				}
				if taskFailureDetails(result.Error) == nil {
					t.Fatal("failure details must survive cleanup")
				}
				_, err := client.BatchV1().Jobs("agents").Get(context.Background(), created.Name, metav1.GetOptions{})
				if failure == "delete forbidden" {
					if err != nil || !strings.Contains(result.Error.Error(), "failed to stop Kubernetes Job") {
						t.Fatalf("cleanup failure must be reported with Job retained: result=%+v get=%v", result, err)
					}
				} else if !apierrors.IsNotFound(err) {
					t.Fatalf("active failed Job was not deleted: %v", err)
				}
			})
		}
	}
}

func TestOpenKubernetesWatch(t *testing.T) {
	t.Run("bounds an HTTP connection stalled before response headers", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer server.Close()
		client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = openKubernetesWatch(ctx, 20*time.Millisecond, func(watchCtx context.Context) (watch.Interface, error) {
			return client.BatchV1().Jobs("agents").Watch(watchCtx, metav1.ListOptions{})
		})
		if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			t.Fatalf("expected establishment deadline, got err=%v parent=%v", err, ctx.Err())
		}
	})

	t.Run("successful stream outlives establishment budget until stopped", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var streamCtx context.Context
			fakeWatch := watch.NewRaceFreeFake()
			defer fakeWatch.Stop()
			watcher, err := openKubernetesWatch(context.Background(), time.Second, func(ctx context.Context) (watch.Interface, error) {
				streamCtx = ctx
				return fakeWatch, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Second)
			if streamCtx.Err() != nil {
				t.Fatalf("successful stream inherited establishment deadline: %v", streamCtx.Err())
			}
			watcher.Stop()
			if streamCtx.Err() != context.Canceled || !fakeWatch.IsStopped() {
				t.Fatal("Stop must cancel the stream context and stop the underlying watch")
			}
		})
	})

	t.Run("parent cancellation interrupts establishment", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := openKubernetesWatch(ctx, time.Minute, func(watchCtx context.Context) (watch.Interface, error) {
			cancel()
			<-watchCtx.Done()
			return nil, watchCtx.Err()
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestExecuteTaskRejectsReplacedJob(t *testing.T) {
	for _, source := range []string{"watch", "poll"} {
		t.Run(source, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := fake.NewSimpleClientset()
				jobWatch := watch.NewRaceFreeFake()
				podWatch := watch.NewRaceFreeFake()
				defer jobWatch.Stop()
				defer podWatch.Stop()
				var created *batchv1.Job
				client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
					job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
					job.UID = "original"
					created = job.DeepCopy()
					return false, nil, nil
				})
				client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
					replacement := created.DeepCopy()
					replacement.UID = "replacement"
					if err := client.Tracker().Update(batchv1.SchemeGroupVersion.WithResource("jobs"), replacement, "agents"); err != nil {
						t.Fatal(err)
					}
					if source == "watch" {
						jobWatch.Modify(replacement)
					}
					return true, jobWatch, nil
				})
				client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
					return true, podWatch, nil
				})
				client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
					t.Error("replacement Job must not be deleted")
					return false, nil, nil
				})
				backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}, clientset: client}
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				result := backend.ExecuteTask(ctx, &TaskParams{TaskID: "task", ExecutionID: "exec", DockerImage: "ubuntu:22.04"})
				if result.Error == nil || !strings.Contains(result.Error.Error(), "was replaced while") {
					t.Fatalf("expected replacement error, got %+v", result)
				}
				retained, err := client.BatchV1().Jobs("agents").Get(context.Background(), created.Name, metav1.GetOptions{})
				if err != nil || retained.UID != "replacement" {
					t.Fatalf("replacement was affected: job=%v err=%v", retained, err)
				}
				if !jobWatch.IsStopped() || !podWatch.IsStopped() {
					t.Fatal("task watches must stop on exit")
				}
			})
		})
	}
}

func TestDefaultKubernetesSchedulingBudget(t *testing.T) {
	if defaultUnschedulableFailureDelay != 10*time.Minute {
		t.Fatalf("default=%s, want 10m", defaultUnschedulableFailureDelay)
	}
	for _, tc := range []struct {
		name string
		age  time.Duration
		want bool
	}{
		{name: "normal Autopilot provisioning", age: 46 * time.Second},
		{name: "before deadline", age: 9 * time.Minute},
		{name: "after deadline", age: 11 * time.Minute, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &KubernetesBackend{config: KubernetesBackendConfig{UnschedulableTimeout: durationPtr(defaultUnschedulableFailureDelay)}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(time.Now().Add(-tc.age))}}
			if got := backend.shouldFailUnschedulablePod(pod); got != tc.want {
				t.Fatalf("should fail=%t, want %t", got, tc.want)
			}
		})
	}
}
