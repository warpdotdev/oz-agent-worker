package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

type expirationTestTransport func(*http.Request) (*http.Response, error)

func (f expirationTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestExpirationAfterObservationTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "agents", UID: "original", ResourceVersion: "1"}}
		body, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		patched := false
		transport := expirationTestTransport(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}
			if req.Method != http.MethodPatch || req.Context().Err() != nil {
				t.Fatalf("expiration did not get a fresh context: method=%s err=%v", req.Method, req.Context().Err())
			}
			patched = true
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		})
		client, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "http://kubernetes.test"}, &http.Client{Transport: transport})
		if err != nil {
			t.Fatal(err)
		}
		backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}, clientset: client}
		ctx, cancel := context.WithTimeout(context.Background(), kubernetesCleanupTimeout)
		defer cancel()
		if err := backend.finalizeFailedJob(ctx, job, errors.New("abandoned")); err != nil || !patched {
			t.Fatalf("patched=%t err=%v", patched, err)
		}
	})
}

func TestKubernetesFailureDisposition(t *testing.T) {
	for _, tc := range []struct {
		name    string
		phase   corev1.PodPhase
		waiting string
		exit    bool
		want    jobFailureDisposition
	}{
		{name: "image pull abandoned", phase: corev1.PodPending, waiting: "ImagePullBackOff", want: jobTerminationRequired},
		{name: "failed Pod retained despite Job status lag", phase: corev1.PodFailed, exit: true, want: jobExecutionStopped},
		{name: "failed container with other live containers", phase: corev1.PodRunning, exit: true, want: jobTerminationRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}, clientset: fake.NewSimpleClientset()}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "agents"},
				Status:     corev1.PodStatus{Phase: tc.phase},
			}
			state := corev1.ContainerState{}
			if tc.exit {
				state.Terminated = &corev1.ContainerStateTerminated{ExitCode: 1}
			} else {
				state.Waiting = &corev1.ContainerStateWaiting{Reason: tc.waiting}
			}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "task", State: state}}
			err := backend.inspectPodFailureAt(context.Background(), pod, nil, kubernetesFailureSourcePodWatch)
			if err == nil || jobDisposition(err) != tc.want {
				t.Fatalf("disposition=%v want=%v err=%v", jobDisposition(err), tc.want, err)
			}
			if jobDisposition(fmt.Errorf("wrapped: %w", err)) != tc.want {
				t.Fatal("wrapping lost the failure disposition")
			}
			if taskFailureDetails(err) == nil {
				t.Fatal("failure diagnostics were lost")
			}
		})
	}
	t.Run("unknown observation error requires termination", func(t *testing.T) {
		err := newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonJobWatch, errors.New("cannot observe job"))
		if jobDisposition(err) != jobTerminationRequired {
			t.Fatal("unknown execution state must not be treated as stopped")
		}
	})
}

func TestFailedPodRetentionBeforeJobStatusCatchesUp(t *testing.T) {
	for _, noCleanup := range []bool{false, true} {
		for _, tc := range []struct {
			name        string
			additional  corev1.PodPhase
			foreign     bool
			missing     bool
			listError   bool
			retries     int32
			wantExpired bool
		}{
			{name: "stopped Pod is retained"},
			{name: "other running Pod must stop", additional: corev1.PodRunning, wantExpired: true},
			{name: "other pending Pod must stop", additional: corev1.PodPending, wantExpired: true},
			{name: "unrelated running Pod ignored", additional: corev1.PodRunning, foreign: true},
			{name: "missing Pod is not evidence of termination", missing: true, wantExpired: true},
			{name: "unobservable remaining Pods require termination", listError: true, wantExpired: true},
			{name: "remaining retry could restart work", retries: 1, wantExpired: true},
		} {
			t.Run(fmt.Sprintf("%s/noCleanup=%t", tc.name, noCleanup), func(t *testing.T) {
				backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents", NoCleanup: noCleanup}}
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "agents", UID: "original", ResourceVersion: "1"},
					Spec:       batchv1.JobSpec{BackoffLimit: &tc.retries, TTLSecondsAfterFinished: backend.taskJobTTLSecondsAfterFinished()},
				}
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "failed-task", Namespace: "agents",
						Labels:          map[string]string{batchv1.ControllerUidLabel: string(job.UID)},
						OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))},
					},
					Status: corev1.PodStatus{Phase: corev1.PodFailed},
				}
				client := fake.NewSimpleClientset(job)
				if !tc.missing {
					if _, err := client.CoreV1().Pods("agents").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				if tc.additional != "" {
					other := pod.DeepCopy()
					other.Name = "other"
					other.Status.Phase = tc.additional
					if tc.foreign {
						other.OwnerReferences[0].UID = "another-job"
					}
					if _, err := client.CoreV1().Pods("agents").Create(context.Background(), other, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				if tc.listError {
					client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("denied"))
					})
				}
				backend.clientset = client
				failure := withJobDisposition(newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonContainerExit, errors.New("container failed")), jobExecutionStopped)
				if err := backend.finalizeFailedJob(context.Background(), job, failure); err != nil {
					t.Fatal(err)
				}
				retained, err := client.BatchV1().Jobs("agents").Get(context.Background(), job.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if (retained.Spec.ActiveDeadlineSeconds != nil) != tc.wantExpired {
					t.Fatalf("expired=%t, want %t", retained.Spec.ActiveDeadlineSeconds != nil, tc.wantExpired)
				}
				if !tc.missing {
					if _, err := client.CoreV1().Pods("agents").Get(context.Background(), pod.Name, metav1.GetOptions{}); err != nil {
						t.Fatalf("failed Pod artifacts lost: %v", err)
					}
				}
				if (retained.Spec.TTLSecondsAfterFinished == nil) != noCleanup {
					t.Fatal("retention policy changed")
				}
			})
		}
	}
}

func TestJobExpirationRetriesAndRechecksIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace bool
		finish  bool
	}{
		{name: "retry conflict with current resource version"},
		{name: "replacement during conflict is not modified", replace: true},
		{name: "completed job during conflict is retained", finish: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				original := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "agents", UID: "original", ResourceVersion: "1"}}
				client := fake.NewSimpleClientset(original)
				calls := 0
				client.PrependReactor("patch", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
					calls++
					if calls == 1 {
						updated := original.DeepCopy()
						updated.ResourceVersion = "2"
						if tc.replace {
							updated.UID = "replacement"
						}
						if tc.finish {
							updated.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
						}
						if err := client.Tracker().Update(batchv1.SchemeGroupVersion.WithResource("jobs"), updated, "agents"); err != nil {
							t.Fatal(err)
						}
						return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "jobs"}, original.Name, errors.New("stale version"))
					}
					return false, nil, nil
				})
				backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}, clientset: client}
				err := backend.finalizeFailedJob(ctx, original, errors.New("abandoned"))
				if (err != nil) != tc.replace {
					t.Fatalf("err=%v, wantError=%t", err, tc.replace)
				}
				wantCalls := 2
				if tc.replace || tc.finish {
					wantCalls = 1
				}
				if calls != wantCalls {
					t.Fatalf("patch calls=%d, want %d", calls, wantCalls)
				}
			})
		})
	}
	t.Run("suspended unstarted job can expire without starting Pods", func(t *testing.T) {
		suspended := true
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "agents", UID: "original", ResourceVersion: "1"},
			Spec:       batchv1.JobSpec{Suspend: &suspended},
		}
		client := fake.NewSimpleClientset(job)
		backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}, clientset: client}
		if err := backend.finalizeFailedJob(context.Background(), job, errors.New("abandoned")); err != nil {
			t.Fatal(err)
		}
		updated, err := client.BatchV1().Jobs("agents").Get(context.Background(), job.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if *updated.Spec.Suspend || *updated.Spec.Parallelism != 0 || *updated.Spec.ActiveDeadlineSeconds != 1 {
			t.Fatal("suspended Job cannot expire safely")
		}
	})
	t.Run("exhausted patch errors remain visible", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "agents", UID: "original", ResourceVersion: "1"}}
			client := fake.NewSimpleClientset(job)
			client.PrependReactor("patch", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewServiceUnavailable("unavailable")
			})
			backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}, clientset: client}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := backend.finalizeFailedJob(ctx, job, errors.New("abandoned")); err == nil {
				t.Fatal("expiration failure must not be reported as success")
			}
		})
	})
}
