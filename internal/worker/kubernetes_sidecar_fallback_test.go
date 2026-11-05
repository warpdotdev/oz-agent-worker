package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	fallbackTestAgentImage = "docker.io/warpdotdev/warp-agent@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	fallbackTestACPImage   = "index.docker.io/warpdotdev/warp-claude-acp-sidecar@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fallbackTestCLIImage   = "mirror.example/claude-cli:latest"
	fallbackTestTaskImage  = "ubuntu:22.04"
	fallbackTestMountPath  = "/mnt/claude-cli-sidecar"
)

func TestPullFailedSidecarIndex(t *testing.T) {
	podSpec := corev1.PodSpec{
		Volumes: []corev1.Volume{
			imageVolume("sidecar-0-image", fallbackTestAgentImage, corev1.PullIfNotPresent),
			imageVolume("sidecar-1-image", "warpdotdev/warp-claude-acp-sidecar", corev1.PullIfNotPresent),
			imageVolume("sidecar-2-image", "warpdotdev/warp-claude-acp-sidecar", corev1.PullIfNotPresent),
		},
		InitContainers: []corev1.Container{{Name: kubernetesSetupContainerName, Image: fallbackTestTaskImage, VolumeMounts: []corev1.VolumeMount{{Name: "sidecar-1-image"}}}},
		Containers: []corev1.Container{{Name: kubernetesTaskContainerName, Image: fallbackTestTaskImage, VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace"}, {Name: "sidecar-0-image"}, {Name: "sidecar-1-image"},
		}}},
	}
	for _, tc := range []struct {
		name, container, message string
		want                     int
		wantOK                   bool
	}{
		{name: "copy init container", container: "copy-sidecar-2", want: 2, wantOK: true},
		{name: "kubelet back-off naming an untagged volume image as latest", container: kubernetesTaskContainerName,
			message: `Back-off pulling image "warpdotdev/warp-claude-acp-sidecar:latest"`, want: 1, wantOK: true},
		{name: "containerd error naming a digest-pinned volume image", container: kubernetesTaskContainerName,
			message: `rpc error: code = Unknown desc = failed to pull and unpack image "` + fallbackTestAgentImage + `": dial tcp: i/o timeout`, want: 0, wantOK: true},
		{name: "setup init container", container: kubernetesSetupContainerName,
			message: `Back-off pulling image "docker.io/warpdotdev/warp-claude-acp-sidecar:latest"`, want: 1, wantOK: true},
		{name: "the task image", container: kubernetesTaskContainerName, message: `Back-off pulling image "ubuntu:22.04"`},
		{name: "a message naming both the task and a sidecar image", container: kubernetesTaskContainerName,
			message: `"ubuntu:22.04" and "warpdotdev/warp-claude-acp-sidecar:latest"`},
		{name: "no image named", container: kubernetesTaskContainerName, message: "Back-off pulling image"},
		{name: "an image the container does not mount", container: kubernetesSetupContainerName,
			message: `Back-off pulling image "` + fallbackTestAgentImage + `"`},
		{name: "unknown container", container: "other", message: `Back-off pulling image "warpdotdev/warp-claude-acp-sidecar"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Spec: podSpec}
			got, ok := pullFailedSidecarIndex(pod, tc.container, &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: tc.message})
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Fatalf("pullFailedSidecarIndex = %d, %v; want %d, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestInspectPodFailureRecordsPulledSidecar(t *testing.T) {
	backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}, clientset: fake.NewSimpleClientset()}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "task-pod", Namespace: "agents"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
			{Name: "copy-sidecar-0", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: "copy-sidecar-1", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}},
		}},
	}
	err := backend.inspectPodFailureAt(context.Background(), pod, nil, kubernetesFailureSourcePodWatch)
	if i, ok := taskFailurePullFailedSidecar(err); !ok || i != 1 {
		t.Fatalf("pulled sidecar = %d, %v; want 1 (err %v)", i, ok, err)
	}
	if pull, ok := taskFailureSidecarPull(err); !ok || pull.container != "copy-sidecar-1" || pull.reason != "ErrImagePull" || pull.confirmed {
		t.Fatalf("sidecar pull = %+v, %v; want copy-sidecar-1 waiting on ErrImagePull, unconfirmed", pull, ok)
	}

	pod.Status.InitContainerStatuses[1].State.Waiting.Reason = "ImagePullBackOff"
	err = backend.inspectPodFailureAt(context.Background(), pod, nil, kubernetesFailureSourcePodWatch)
	if pull, ok := taskFailureSidecarPull(err); !ok || pull.index != 1 || pull.reason != "ImagePullBackOff" {
		t.Fatalf("sidecar pull = %+v, %v; want sidecar 1 waiting on ImagePullBackOff", pull, ok)
	}
	confirmSidecarPullFailure(err)
	if pull, _ := taskFailureSidecarPull(err); !pull.confirmed {
		t.Fatalf("confirmation must be carried on the failure: %+v", pull)
	}

	pod.Status.InitContainerStatuses[1].State.Waiting.Reason = "CreateContainerConfigError"
	err = backend.inspectPodFailureAt(context.Background(), pod, nil, kubernetesFailureSourcePodWatch)
	if _, ok := taskFailurePullFailedSidecar(err); ok || err == nil {
		t.Fatalf("a non-pull failure must not name a sidecar: %v", err)
	}
}

// fallbackTestCluster fakes a cluster in which pods fail to pull every image in
// blocked and every other Job completes. Its watches deliver events on virtual
// time, so tests run it under synctest.
type fallbackTestCluster struct {
	t            *testing.T
	client       *fake.Clientset
	imageVolumes bool
	blocked      map[string]bool
	// pullStates are the kubelet's successive reports for a blocked image, one pod
	// event every fallbackTestPullStateInterval: a waiting reason, or
	// fallbackTestPullRecovered once the pull succeeds and the pod moves on, after
	// which the Job completes. nil means a pull that keeps failing: ErrImagePull,
	// then ImagePullBackOff, then ErrImagePull again.
	pullStates []string

	mu      sync.Mutex
	created []*batchv1.Job
	deleted bool
}

func newFallbackTestCluster(t *testing.T, imageVolumes bool, blocked ...string) *fallbackTestCluster {
	c := &fallbackTestCluster{t: t, client: fake.NewSimpleClientset(), imageVolumes: imageVolumes, blocked: make(map[string]bool)}
	for _, image := range blocked {
		c.blocked[image] = true
	}
	c.client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		c.mu.Lock()
		defer c.mu.Unlock()
		job.UID = k8stypes.UID(fmt.Sprintf("attempt-%d", len(c.created)+1))
		job.ResourceVersion = "1"
		c.created = append(c.created, job.DeepCopy())
		return false, nil, nil
	})
	c.client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		c.mu.Lock()
		c.deleted = true
		c.mu.Unlock()
		return false, nil, nil
	})
	c.client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		w := watch.NewRaceFreeFake()
		job := c.latest()
		done := job.DeepCopy()
		done.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		states := c.states()
		switch {
		case c.failingPod(job, states[0]) == nil:
			w.Modify(done)
		case states[len(states)-1] == fallbackTestPullRecovered:
			// Timers rather than sleeping goroutines: a bubble may end before they
			// fire, and a pending timer does not count as a blocked goroutine.
			time.AfterFunc(fallbackTestPullStateInterval*time.Duration(len(states)), func() { w.Modify(done) })
		}
		return true, w, nil
	})
	c.client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		w := watch.NewRaceFreeFake()
		job := c.latest()
		states := c.states()
		if c.failingPod(job, states[0]) == nil {
			return true, w, nil
		}
		w.Add(c.failingPod(job, states[0]))
		for i, state := range states[1:] {
			// Sends after the watch is stopped are dropped by the fake.
			time.AfterFunc(fallbackTestPullStateInterval*time.Duration(i+1), func() { w.Modify(c.failingPod(job, state)) })
		}
		return true, w, nil
	})
	return c
}

const (
	fallbackTestPullStateInterval = 2 * time.Second
	fallbackTestPullRecovered     = "recovered"
)

func (c *fallbackTestCluster) states() []string {
	if c.pullStates == nil {
		return []string{"ErrImagePull", "ImagePullBackOff", "ErrImagePull"}
	}
	return c.pullStates
}

func (c *fallbackTestCluster) latest() *batchv1.Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.created[len(c.created)-1]
}

func (c *fallbackTestCluster) jobs() []*batchv1.Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*batchv1.Job(nil), c.created...)
}

// failingPod returns job's pod as the kubelet reports it when one of its images is
// blocked and the pull is in state (a waiting reason, or fallbackTestPullRecovered),
// or nil when the pod would run.
func (c *fallbackTestCluster) failingPod(job *batchv1.Job, state string) *corev1.Pod {
	spec := job.Spec.Template.Spec.DeepCopy()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: job.Namespace, UID: job.UID + "-pod", Labels: job.Spec.Template.Labels},
		Spec:       *spec,
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	pulling := func(image string) corev1.ContainerState {
		switch state {
		case fallbackTestPullRecovered:
			return corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}
		case "ErrImagePull":
			return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: state, Message: fmt.Sprintf("rpc error: code = Unknown desc = failed to pull and unpack image %q: toomanyrequests", image)}}
		default:
			return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: state, Message: fmt.Sprintf("Back-off pulling image %q", image)}}
		}
	}
	for _, container := range spec.InitContainers {
		if c.blocked[container.Image] {
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: container.Name, State: pulling(container.Image)}}
			return pod
		}
	}
	for _, volume := range spec.Volumes {
		if volume.Image != nil && c.blocked[volume.Image.Reference] {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: kubernetesTaskContainerName, State: pulling(volume.Image.Reference)}}
			return pod
		}
	}
	if c.blocked[fallbackTestTaskImage] {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: kubernetesTaskContainerName, State: pulling(fallbackTestTaskImage)}}
		return pod
	}
	return nil
}

func (c *fallbackTestCluster) backend() *KubernetesBackend {
	return &KubernetesBackend{
		config:          KubernetesBackendConfig{Namespace: "agents", UnschedulableTimeout: durationPtr(defaultUnschedulableFailureDelay)},
		clientset:       c.client,
		useImageVolumes: c.imageVolumes,
	}
}

// sidecarImage returns the image job mounts for the sidecar at index i.
func (c *fallbackTestCluster) sidecarImage(job *batchv1.Job, i int) string {
	spec := job.Spec.Template.Spec
	if c.imageVolumes {
		for _, volume := range spec.Volumes {
			if volume.Name == kubernetesSidecarVolumeName(i, kubernetesSidecarImageVolumeSuffix) {
				return volume.Image.Reference
			}
		}
	}
	for _, container := range spec.InitContainers {
		if container.Name == fmt.Sprintf("%s%d", kubernetesSidecarInitPrefix, i) {
			return container.Image
		}
	}
	c.t.Fatalf("job %s mounts no sidecar %d", job.Name, i)
	return ""
}

func (c *fallbackTestCluster) actions(verb string) []k8stesting.Action {
	var matched []k8stesting.Action
	for _, action := range c.client.Actions() {
		if action.GetVerb() == verb && action.GetResource().Resource == "jobs" {
			matched = append(matched, action)
		}
	}
	return matched
}

// expired reports whether the named Job was stopped through its active deadline.
func (c *fallbackTestCluster) expired(name string) bool {
	job, err := c.client.BatchV1().Jobs("agents").Get(context.Background(), name, metav1.GetOptions{})
	return err == nil && job.Spec.ActiveDeadlineSeconds != nil && *job.Spec.ActiveDeadlineSeconds == 1
}

func fallbackTestParams(taskID string) *TaskParams {
	return &TaskParams{
		TaskID:      taskID,
		ExecutionID: "exec-" + taskID,
		DockerImage: fallbackTestTaskImage,
		Sidecars: []types.SidecarMount{
			{Image: fallbackTestAgentImage, MountPath: "/agent"},
			{Image: fallbackTestACPImage, MountPath: fallbackTestMountPath, FallbackImage: fallbackTestCLIImage},
		},
	}
}

func TestKubernetesSidecarFallbackReplacesJob(t *testing.T) {
	for _, imageVolumes := range []bool{false, true} {
		for _, standby := range []bool{false, true} {
			t.Run(fmt.Sprintf("imageVolumes=%v/standby=%v", imageVolumes, standby), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					cluster := newFallbackTestCluster(t, imageVolumes, fallbackTestACPImage)
					backend := cluster.backend()
					params := fallbackTestParams("task-1")
					if standby {
						params = &TaskParams{StandbyID: "sb-1", DockerImage: params.DockerImage, Sidecars: params.Sidecars}
					}
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()

					result := backend.ExecuteTask(ctx, params)
					if result.Outcome != ExecuteOutcomeCompleted || result.Error != nil {
						t.Fatalf("result = %+v, want the retried Job to complete", result)
					}
					jobs := cluster.jobs()
					if len(jobs) != 2 || jobs[0].Name != jobs[1].Name {
						t.Fatalf("created %d Jobs, want two under one name", len(jobs))
					}
					if got := cluster.sidecarImage(jobs[0], 1); got != fallbackTestACPImage {
						t.Fatalf("first Job mounts %s, want the ACP image", got)
					}
					if got := cluster.sidecarImage(jobs[1], 1); got != fallbackTestCLIImage {
						t.Fatalf("retried Job mounts %s, want the fallback", got)
					}
					if got := cluster.sidecarImage(jobs[1], 0); got != fallbackTestAgentImage {
						t.Fatalf("retried Job mounts agent %s, want it unchanged", got)
					}
					deletes := cluster.actions("delete")
					if len(deletes) == 0 {
						t.Fatal("first Job was not deleted")
					}
					options := deletes[0].(k8stesting.DeleteAction).GetDeleteOptions()
					if options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground ||
						options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "attempt-1" {
						t.Fatalf("first delete options = %+v, want foreground deletion of attempt-1", options)
					}
					if patches := cluster.actions("patch"); len(patches) != 0 {
						t.Fatalf("got %d Job patches; the replaced Job must not be stopped first", len(patches))
					}
					if !backend.sidecarPullFailures.recent(fallbackTestACPImage) {
						t.Fatal("the failed ACP image must be remembered")
					}

					if standby {
						return
					}
					result = backend.ExecuteTask(ctx, fallbackTestParams("task-2"))
					if result.Outcome != ExecuteOutcomeCompleted {
						t.Fatalf("second task result = %+v", result)
					}
					jobs = cluster.jobs()
					if len(jobs) != 3 || cluster.sidecarImage(jobs[2], 1) != fallbackTestCLIImage {
						t.Fatalf("second task must create one Job that already mounts the fallback; created %d", len(jobs)-2)
					}
				})
			})
		}
	}
}

// TestKubernetesSidecarPullConfirmWindow covers what the attempt decides while it
// watches a failed sidecar pull for kubernetesSidecarPullConfirmWindow. A pull that
// fails again after the back-off is covered by TestKubernetesSidecarFallbackReplacesJob.
func TestKubernetesSidecarPullConfirmWindow(t *testing.T) {
	type expectation struct {
		jobs       int    // Jobs created by the first task
		lastImage  string // sidecar 1's image on the last of them
		remembered bool
		waited     bool // the window ran out before the attempt decided
	}
	run := func(name string, states []string, want expectation) {
		for _, imageVolumes := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/imageVolumes=%v", name, imageVolumes), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					cluster := newFallbackTestCluster(t, imageVolumes, fallbackTestACPImage)
					cluster.pullStates = states
					backend := cluster.backend()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					defer cancel()

					start := time.Now()
					result := backend.ExecuteTask(ctx, fallbackTestParams("task-1"))
					if result.Outcome != ExecuteOutcomeCompleted || result.Error != nil {
						t.Fatalf("result = %+v, want a completed task", result)
					}
					if waited := time.Since(start) >= kubernetesSidecarPullConfirmWindow; waited != want.waited {
						t.Fatalf("decided after %s; want waited for the %s window = %v", time.Since(start), kubernetesSidecarPullConfirmWindow, want.waited)
					}
					jobs := cluster.jobs()
					if len(jobs) != want.jobs || cluster.sidecarImage(jobs[len(jobs)-1], 1) != want.lastImage {
						t.Fatalf("created %d Jobs, the last mounting %s; want %d ending with %s", len(jobs), cluster.sidecarImage(jobs[len(jobs)-1], 1), want.jobs, want.lastImage)
					}
					// A replaced Job is deleted in the foreground with a UID precondition;
					// a completed Job's cleanup delete is not.
					replaced := 0
					for _, action := range cluster.actions("delete") {
						options := action.(k8stesting.DeleteAction).GetDeleteOptions()
						if options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationForeground && options.Preconditions != nil {
							replaced++
						}
					}
					if (replaced != 0) != (want.jobs > 1) {
						t.Fatalf("got %d Job replacements with %d Jobs", replaced, want.jobs)
					}
					if backend.sidecarPullFailures.recent(fallbackTestACPImage) != want.remembered {
						t.Fatalf("remembered = %v, want %v", !want.remembered, want.remembered)
					}

					// The next task mounts the fallback directly only when the failure
					// was remembered; otherwise it tries the ACP image again.
					result = backend.ExecuteTask(ctx, fallbackTestParams("task-2"))
					if result.Outcome != ExecuteOutcomeCompleted || result.Error != nil {
						t.Fatalf("second task result = %+v", result)
					}
					next := cluster.jobs()[len(jobs)]
					if got, wantImage := cluster.sidecarImage(next, 1), fallbackTestACPImage; want.remembered {
						wantImage = fallbackTestCLIImage
						if got != wantImage {
							t.Fatalf("second task's Job mounts %s, want %s", got, wantImage)
						}
					} else if got != wantImage {
						t.Fatalf("second task's Job mounts %s, want %s", got, wantImage)
					}
				})
			})
		}
	}

	run("a single failure falls back after the window and is not remembered",
		[]string{"ErrImagePull"}, expectation{jobs: 2, lastImage: fallbackTestCLIImage, waited: true})
	run("a failure followed by a back-off alone is not a second failure",
		[]string{"ErrImagePull", "ImagePullBackOff"}, expectation{jobs: 2, lastImage: fallbackTestCLIImage, waited: true})
	run("a failure after a back-off is confirmed and remembered",
		[]string{"ImagePullBackOff", "ErrImagePull"}, expectation{jobs: 2, lastImage: fallbackTestCLIImage, remembered: true})
	run("a pull that succeeds within the window continues without a fallback",
		[]string{"ErrImagePull", fallbackTestPullRecovered}, expectation{jobs: 1, lastImage: fallbackTestACPImage})

	t.Run("cancellation during the window fails without a fallback", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cluster := newFallbackTestCluster(t, false, fallbackTestACPImage)
			cluster.pullStates = []string{"ErrImagePull"}
			backend := cluster.backend()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				time.Sleep(fallbackTestPullStateInterval)
				cancel()
			}()

			result := backend.ExecuteTask(ctx, fallbackTestParams("task-1"))
			if result.Outcome != ExecuteOutcomeError || !errors.Is(result.Error, context.Canceled) {
				t.Fatalf("result = %+v, want cancellation", result)
			}
			if jobs := cluster.jobs(); len(jobs) != 1 || len(cluster.actions("delete")) != 0 {
				t.Fatalf("created %d Jobs with %d deletes; want the one Job left alone", len(jobs), len(cluster.actions("delete")))
			}
			if backend.sidecarPullFailures.recent(fallbackTestACPImage) {
				t.Fatal("a cancelled attempt must not remember the image")
			}
		})
	})
}

func TestKubernetesSidecarFallbackLimits(t *testing.T) {
	ctxWithTimeout := func(t *testing.T) context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		return ctx
	}
	assertImagePullFailure := func(t *testing.T, result ExecuteResult) {
		t.Helper()
		if result.Outcome != ExecuteOutcomeError {
			t.Fatalf("result = %+v, want a failure", result)
		}
		if _, reason := taskFailureLabels(result.Error); reason != metrics.TaskFailureReasonImagePull {
			t.Fatalf("failure reason = %s (%v), want image_pull", reason, result.Error)
		}
	}
	// run runs a case under synctest, since the fake cluster delivers its pod
	// events and the attempt's confirmation window runs on virtual time.
	run := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) { synctest.Test(t, f) })
	}

	run("a task image pull failure is not retried", func(t *testing.T) {
		cluster := newFallbackTestCluster(t, true, fallbackTestTaskImage)
		result := cluster.backend().ExecuteTask(ctxWithTimeout(t), fallbackTestParams("task-1"))
		assertImagePullFailure(t, result)
		jobs := cluster.jobs()
		if len(jobs) != 1 || !cluster.expired(jobs[0].Name) {
			t.Fatalf("created %d Jobs; want one, stopped as before", len(jobs))
		}
	})

	run("a sidecar without a fallback is not retried", func(t *testing.T) {
		cluster := newFallbackTestCluster(t, false, fallbackTestACPImage)
		params := fallbackTestParams("task-1")
		params.Sidecars[1].FallbackImage = ""
		assertImagePullFailure(t, cluster.backend().ExecuteTask(ctxWithTimeout(t), params))
		if jobs := cluster.jobs(); len(jobs) != 1 || !cluster.expired(jobs[0].Name) {
			t.Fatalf("created %d Jobs; want one, stopped as before", len(jobs))
		}
	})

	run("a failing fallback is retried only once", func(t *testing.T) {
		cluster := newFallbackTestCluster(t, false, fallbackTestACPImage, fallbackTestCLIImage)
		result := cluster.backend().ExecuteTask(ctxWithTimeout(t), fallbackTestParams("task-1"))
		assertImagePullFailure(t, result)
		if !strings.Contains(result.Error.Error(), "neither could its fallback "+fallbackTestCLIImage) {
			t.Fatalf("error = %v, want one naming both images", result.Error)
		}
		jobs := cluster.jobs()
		if len(jobs) != 2 || !cluster.expired(jobs[1].Name) {
			t.Fatalf("created %d Jobs; want two, with the retry stopped", len(jobs))
		}
	})

	run("a Job that cannot be deleted is stopped instead", func(t *testing.T) {
		cluster := newFallbackTestCluster(t, false, fallbackTestACPImage)
		cluster.client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "task", errors.New("denied"))
		})
		assertImagePullFailure(t, cluster.backend().ExecuteTask(ctxWithTimeout(t), fallbackTestParams("task-1")))
		if jobs := cluster.jobs(); len(jobs) != 1 || !cluster.expired(jobs[0].Name) {
			t.Fatalf("created %d Jobs; want one, stopped after the failed delete", len(jobs))
		}
	})

	run("a Job that is not removed in time is not replaced", func(t *testing.T) {
		{
			cluster := newFallbackTestCluster(t, false, fallbackTestACPImage)
			cluster.client.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
				cluster.mu.Lock()
				defer cluster.mu.Unlock()
				if !cluster.deleted {
					return false, nil, nil
				}
				return true, cluster.created[0].DeepCopy(), nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			start := time.Now()
			assertImagePullFailure(t, cluster.backend().ExecuteTask(ctx, fallbackTestParams("task-1")))
			if elapsed := time.Since(start); elapsed < kubernetesJobReplaceTimeout {
				t.Fatalf("gave up after %s, want the full %s wait", elapsed, kubernetesJobReplaceTimeout)
			}
			if jobs := cluster.jobs(); len(jobs) != 1 {
				t.Fatalf("created %d Jobs; want no retry", len(jobs))
			}
		}
	})

	run("cancellation while waiting for the deletion stops the retry", func(t *testing.T) {
		cluster := newFallbackTestCluster(t, false, fallbackTestACPImage)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cluster.client.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
			cluster.mu.Lock()
			defer cluster.mu.Unlock()
			if !cluster.deleted {
				return false, nil, nil
			}
			cancel()
			return true, cluster.created[0].DeepCopy(), nil
		})
		if result := cluster.backend().ExecuteTask(ctx, fallbackTestParams("task-1")); result.Outcome != ExecuteOutcomeError {
			t.Fatalf("result = %+v, want a failure", result)
		}
		if jobs := cluster.jobs(); len(jobs) != 1 {
			t.Fatalf("created %d Jobs; want no retry after cancellation", len(jobs))
		}
	})
}
