package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestKubernetesFailureDetailsUseCurrentTerminationState(t *testing.T) {
	tests := []struct {
		name               string
		terminated         corev1.ContainerStateTerminated
		lastTerminated     corev1.ContainerStateTerminated
		wantRawExitCode    int32
		wantNormalizedCode int
		wantSignal         int32
	}{
		{
			name:               "current state wins over previous termination",
			terminated:         corev1.ContainerStateTerminated{ExitCode: 143, Reason: "Error"},
			lastTerminated:     corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"},
			wantRawExitCode:    143,
			wantNormalizedCode: 143,
		},
		{
			name:               "signal is normalized separately from raw exit code",
			terminated:         corev1.ContainerStateTerminated{ExitCode: 0, Signal: 15, Reason: "Error"},
			wantRawExitCode:    0,
			wantNormalizedCode: 143,
			wantSignal:         15,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &KubernetesBackend{
				config:    KubernetesBackendConfig{Namespace: "agents"},
				clientset: fake.NewSimpleClientset(),
			}
			err := backend.inspectPodFailureAt(context.Background(), &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "task-pod", UID: "pod-uid"},
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
					ContainerStatuses: []corev1.ContainerStatus{{
						Name: kubernetesTaskContainerName,
						State: corev1.ContainerState{
							Terminated: &tt.terminated,
						},
						LastTerminationState: corev1.ContainerState{
							Terminated: &tt.lastTerminated,
						},
					}},
				},
			}, nil, kubernetesFailureSourcePodWatch)
			if err == nil {
				t.Fatal("expected container failure")
			}

			details := taskFailureDetails(err)
			if details == nil || details.Kubernetes == nil || details.Kubernetes.Container == nil {
				t.Fatal("expected Kubernetes container details")
			}
			container := details.Kubernetes.Container
			if container.RawExitCode == nil || *container.RawExitCode != tt.wantRawExitCode {
				t.Fatalf("raw exit code = %v, want %d", container.RawExitCode, tt.wantRawExitCode)
			}
			if container.NormalizedExitCode == nil || *container.NormalizedExitCode != tt.wantNormalizedCode {
				t.Fatalf("normalized exit code = %v, want %d", container.NormalizedExitCode, tt.wantNormalizedCode)
			}
			if tt.wantSignal == 0 {
				if container.Signal != nil {
					t.Fatalf("signal = %v, want nil", container.Signal)
				}
			} else if container.Signal == nil || *container.Signal != tt.wantSignal {
				t.Fatalf("signal = %v, want %d", container.Signal, tt.wantSignal)
			}
			if container.TerminationReason != tt.terminated.Reason {
				t.Fatalf("termination reason = %q, want current reason %q", container.TerminationReason, tt.terminated.Reason)
			}
		})
	}
}

func TestKubernetesFailureDetailsStayWithinServerLimit(t *testing.T) {
	longReason := strings.Repeat("r", maxKubernetesFailureReasonBytes)
	jobConditions := make([]batchv1.JobCondition, maxKubernetesFailureConditions)
	podConditions := make([]corev1.PodCondition, maxKubernetesFailureConditions)
	events := make([]corev1.Event, maxKubernetesFailureEvents)
	for i := range maxKubernetesFailureConditions {
		jobConditions[i] = batchv1.JobCondition{
			Type:   batchv1.JobFailed,
			Status: corev1.ConditionTrue,
			Reason: longReason,
		}
		podConditions[i] = corev1.PodCondition{
			Type:   corev1.PodReady,
			Status: corev1.ConditionFalse,
			Reason: longReason,
		}
	}
	for i := range maxKubernetesFailureEvents {
		events[i] = corev1.Event{
			Type:   corev1.EventTypeWarning,
			Reason: longReason,
			Count:  1,
		}
	}
	rawExitCode := int32(143)
	normalizedExitCode := 143

	details := buildKubernetesFailureDetails(
		kubernetesFailureSourceSafetyPoll,
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("j", 253), UID: "job-uid"},
			Status:     batchv1.JobStatus{Conditions: jobConditions},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("p", 253), UID: "pod-uid"},
			Status:     corev1.PodStatus{Phase: corev1.PodFailed, Reason: longReason, Conditions: podConditions},
		},
		&types.KubernetesContainerFailureDetails{
			Kind:               "regular",
			Name:               strings.Repeat("c", 63),
			State:              "terminated",
			TerminationReason:  longReason,
			RawExitCode:        &rawExitCode,
			NormalizedExitCode: &normalizedExitCode,
		},
		events,
	)
	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatalf("failed to marshal maximum failure details: %v", err)
	}
	if len(encoded) > maxFailureDetailsBytes {
		t.Fatalf("failure details size = %d, exceeds server limit %d", len(encoded), maxFailureDetailsBytes)
	}
}

func TestKubernetesFailureDetailsPrivacyProjection(t *testing.T) {
	now := time.Now().UTC()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-job",
			Namespace: "customer-namespace",
			UID:       "job-uid",
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type:               batchv1.JobFailed,
			Status:             corev1.ConditionTrue,
			Reason:             "BackoffLimitExceeded",
			Message:            "sensitive job message",
			LastTransitionTime: metav1.NewTime(now),
		}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-pod",
			Namespace: "customer-namespace",
			UID:       "pod-uid",
		},
		Spec: corev1.PodSpec{NodeName: "customer-node"},
		Status: corev1.PodStatus{
			Phase:   corev1.PodFailed,
			Reason:  "Evicted",
			Message: "sensitive pod message",
			Conditions: []corev1.PodCondition{{
				Type:               "DisruptionTarget",
				Status:             corev1.ConditionTrue,
				Reason:             "TerminationByKubelet",
				Message:            "sensitive condition message",
				LastTransitionTime: metav1.NewTime(now),
			}},
		},
	}
	terminated := &corev1.ContainerStateTerminated{
		ExitCode:    143,
		Reason:      "Error",
		Message:     "sensitive termination message",
		ContainerID: "containerd://sensitive-runtime-id",
	}
	events := []corev1.Event{
		{
			Type:                corev1.EventTypeWarning,
			Reason:              "FailedMount",
			Message:             "sensitive event message",
			Count:               2,
			FirstTimestamp:      metav1.NewTime(now.Add(-time.Minute)),
			LastTimestamp:       metav1.NewTime(now),
			ReportingController: "sensitive.controller",
			Source:              corev1.EventSource{Component: "sensitive-component", Host: "sensitive-host"},
		},
		{Type: corev1.EventTypeNormal, Reason: "Scheduled", Message: "routine event"},
		{Type: corev1.EventTypeNormal, Reason: "Killing", Message: "sensitive killing message"},
	}

	details := buildKubernetesFailureDetails(
		kubernetesFailureSourceJobWatch,
		job,
		pod,
		terminatedContainerFailureDetails("regular", kubernetesTaskContainerName, terminated),
		events,
	)
	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatalf("failed to marshal details: %v", err)
	}
	serialized := string(encoded)

	for _, want := range []string{
		`"name":"task-job"`,
		`"uid":"job-uid"`,
		`"name":"task-pod"`,
		`"uid":"pod-uid"`,
		`"name":"task"`,
		`"reason":"FailedMount"`,
		`"reason":"Killing"`,
	} {
		if !strings.Contains(serialized, want) {
			t.Fatalf("expected serialized details to contain %s: %s", want, serialized)
		}
	}
	for _, forbidden := range []string{
		"customer-namespace",
		"customer-node",
		"sensitive job message",
		"sensitive pod message",
		"sensitive condition message",
		"sensitive termination message",
		"sensitive-runtime-id",
		"sensitive event message",
		"sensitive killing message",
		"sensitive.controller",
		"sensitive-component",
		"sensitive-host",
		"routine event",
		"Scheduled",
	} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("serialized details contain forbidden value %q: %s", forbidden, serialized)
		}
	}
}

func TestKubernetesFailureDetailsCaptureJobAndPodFailureStates(t *testing.T) {
	t.Run("waiting init container", func(t *testing.T) {
		backend := &KubernetesBackend{
			config:    KubernetesBackendConfig{Namespace: "agents"},
			clientset: fake.NewSimpleClientset(),
		}
		err := backend.inspectPodFailureAt(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "task-pod"},
			Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  "setup",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}}},
		}, nil, kubernetesFailureSourceSafetyPoll)
		details := taskFailureDetails(err)
		if details == nil || details.Kubernetes.Container == nil {
			t.Fatal("expected init-container failure details")
		}
		container := details.Kubernetes.Container
		if container.Kind != "init" || container.State != "waiting" || container.WaitingReason != "ImagePullBackOff" {
			t.Fatalf("unexpected container details: %+v", container)
		}
	})

	t.Run("Job deadline", func(t *testing.T) {
		backend := &KubernetesBackend{
			config:    KubernetesBackendConfig{Namespace: "agents"},
			clientset: fake.NewSimpleClientset(),
		}
		result := backend.handleJobStateAt(context.Background(), &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "task-job"},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobFailed,
				Status: corev1.ConditionTrue,
				Reason: "DeadlineExceeded",
			}}},
		}, "task-1", "execution-1", kubernetesFailureSourceJobWatch)
		if result == nil || result.err == nil {
			t.Fatal("expected failed Job result")
		}
		details := taskFailureDetails(result.err)
		if details == nil || details.Kubernetes.Job == nil || len(details.Kubernetes.Job.Conditions) != 1 {
			t.Fatalf("expected Job condition details, got %+v", details)
		}
		if details.Kubernetes.Job.Conditions[0].Reason != "DeadlineExceeded" {
			t.Fatalf("Job condition = %+v", details.Kubernetes.Job.Conditions[0])
		}
	})

	t.Run("Pod eviction condition", func(t *testing.T) {
		backend := &KubernetesBackend{
			config:    KubernetesBackendConfig{Namespace: "agents"},
			clientset: fake.NewSimpleClientset(),
		}
		err := backend.inspectPodFailureAt(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "task-pod"},
			Status: corev1.PodStatus{
				Phase:  corev1.PodFailed,
				Reason: "Evicted",
				Conditions: []corev1.PodCondition{{
					Type:   "DisruptionTarget",
					Status: corev1.ConditionTrue,
					Reason: "TerminationByKubelet",
				}},
			},
		}, nil, kubernetesFailureSourcePodWatch)
		details := taskFailureDetails(err)
		if details == nil || details.Kubernetes.Pod == nil || len(details.Kubernetes.Pod.Conditions) != 1 {
			t.Fatalf("expected Pod condition details, got %+v", details)
		}
		if details.Kubernetes.Pod.Reason != "Evicted" || details.Kubernetes.Pod.Conditions[0].Reason != "TerminationByKubelet" {
			t.Fatalf("unexpected Pod details: %+v", details.Kubernetes.Pod)
		}
	})
}

func TestKubernetesEventCollectionFailureDoesNotHidePodFailure(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	fakeClient.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("events unavailable")
	})
	backend := &KubernetesBackend{
		config:    KubernetesBackendConfig{Namespace: "agents"},
		clientset: fakeClient,
	}

	err := backend.inspectPodFailureAt(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "task-pod", UID: "pod-uid"},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Error"},
	}, nil, kubernetesFailureSourceSafetyPoll)
	if err == nil {
		t.Fatal("expected Pod failure despite Event-list error")
	}
	details := taskFailureDetails(err)
	if details == nil || details.Kubernetes == nil || details.Kubernetes.Pod == nil {
		t.Fatalf("expected partial failure details, got %+v", details)
	}
	if len(details.Kubernetes.Events) != 0 {
		t.Fatalf("events = %+v, want none", details.Kubernetes.Events)
	}
}
