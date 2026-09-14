package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestExecuteTaskCopyReadiness(t *testing.T) {
	for _, count := range []int{2, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
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
				completed := created.DeepCopy()
				completed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
				jobWatch.Modify(completed)
				return true, jobWatch, nil
			})
			client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) { return true, podWatch, nil })
			backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents", SidecarCopyReadiness: true, SetupCommand: "true"}, clientset: client}
			params := &TaskParams{TaskID: "copy-test", DockerImage: "ubuntu:22.04"}
			for i := 0; i < count; i++ {
				params.Sidecars = append(params.Sidecars, types.SidecarMount{Image: "sidecar:test", MountPath: fmt.Sprintf("/helper-%d", i), ReadWrite: i == 0})
			}
			result := backend.ExecuteTask(context.Background(), params)
			if result.Error != nil {
				t.Fatal(result.Error)
			}
			spec := created.Spec.Template.Spec
			if len(spec.InitContainers) != count+1 || spec.InitContainers[count].Name != kubernetesSetupContainerName {
				t.Fatal("setup did not follow all copies")
			}
			for i := 0; i < count; i++ {
				c := spec.InitContainers[i]
				if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
					t.Fatal("copy is not restartable")
				}
				if c.StartupProbe == nil || c.StartupProbe.Exec == nil || c.StartupProbe.Exec.Command[2] != "test -f "+sidecarCopyCompletePath {
					t.Fatal("copy is not gated by successful completion")
				}
				if c.Command[2] != kubernetesSidecarReadinessScript() {
					t.Fatal("wrong copy script")
				}
				if len(c.VolumeMounts) != 2 || c.VolumeMounts[1].MountPath != sidecarCopyStateMountPath {
					t.Fatal("missing private completion state")
				}
				if c.SecurityContext == nil || *c.SecurityContext.RunAsUser != 0 {
					t.Fatal("copy must retain root permissions")
				}
			}
			if len(spec.Volumes) != 1+count*2 {
				t.Fatal("missing data/state volumes")
			}
			for _, v := range spec.Volumes {
				if v.EmptyDir == nil || v.EmptyDir.Medium != "" {
					t.Fatal("copy storage must remain disk-backed")
				}
			}
			task := spec.Containers[0]
			for _, c := range []corev1.Container{task, spec.InitContainers[count]} {
				for _, mount := range c.VolumeMounts {
					if mount.MountPath == sidecarCopyStateMountPath || strings.HasSuffix(mount.Name, "copy-state") {
						t.Fatal("completion state exposed to task/setup")
					}
				}
			}
			if task.VolumeMounts[1].ReadOnly || !task.VolumeMounts[2].ReadOnly {
				t.Fatal("sidecar access modes changed")
			}
		})
	}
}

func TestCopyReadinessPreflight(t *testing.T) {
	backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents", SidecarCopyReadiness: true, PreflightImage: "busybox:1.36"}}
	job := backend.startupPreflightJob()
	init := job.Spec.Template.Spec.InitContainers[0]
	if init.RestartPolicy == nil || *init.RestartPolicy != corev1.ContainerRestartPolicyAlways || init.StartupProbe == nil {
		t.Fatal("preflight does not exercise native sidecars")
	}
	client := fake.NewSimpleClientset(job, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "preflight", Namespace: "agents", Labels: map[string]string{"job-name": job.Name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}})
	backend.clientset = client
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := backend.waitForStartupPreflight(context.Background(), ctx, job); err == nil {
		t.Fatal("a running preflight must not count as successful")
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if _, err := client.BatchV1().Jobs("agents").UpdateStatus(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := backend.waitForStartupPreflight(context.Background(), context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := NewKubernetesBackend(context.Background(), KubernetesBackendConfig{SidecarCopyReadiness: true, UseImageVolumes: true}); err == nil {
		t.Fatal("incompatible copy modes accepted")
	}
}

// Exercise the actual shell program with tar, replacing only its absolute
// filesystem roots so tests need neither root privileges nor a container.
func TestCopyReadinessScript(t *testing.T) {
	tar, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("tar is unavailable")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	for _, failure := range []string{"", "producer", "consumer"} {
		t.Run("failure="+failure, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source")
			target := filepath.Join(root, "target")
			state := filepath.Join(root, "state")
			bin := filepath.Join(root, "bin")
			for _, dir := range []string{source, target, state, bin} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(source, "payload"), "complete data\n")
			write(filepath.Join(target, ".partial"), "old partial copy")
			// Fail the producer after emitting a valid archive: consumer success alone
			// must never release startup. Also exercise extraction failure separately.
			wrapper := `#!/bin/sh
stage=consumer
for arg do [ "$arg" != -cf ] || stage=producer; done
if [ "$OZ_TEST_FAILURE" = "$stage" ]; then
  if [ "$stage" = producer ]; then "$OZ_TEST_TAR" "$@"; else cat >/dev/null; fi
  exit 42
fi
exec "$OZ_TEST_TAR" "$@"
`
			write(filepath.Join(bin, "tar"), wrapper)
			script := strings.Replace(kubernetesSidecarReadinessScript(), "-C / -cf", "-C '"+source+"' -cf", 1)
			script = strings.ReplaceAll(script, "/target", target)
			script = strings.ReplaceAll(script, sidecarCopyStateMountPath, state)
			script = strings.ReplaceAll(script, "/dev/termination-log", filepath.Join(root, "termination-log"))
			held := filepath.Join(root, "held")
			script = strings.Replace(script, "while :; do", "touch '"+held+"'\nwhile :; do", 1)
			complete := filepath.Join(state, "complete")
			start := func(failure string) (*exec.Cmd, <-chan error) {
				t.Helper()
				cmd := exec.Command("sh", "-ec", script)
				cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "OZ_TEST_TAR="+tar, "OZ_TEST_FAILURE="+failure)
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				t.Cleanup(func() { _ = cmd.Process.Kill() })
				return cmd, done
			}
			waitReady := func(done <-chan error) {
				t.Helper()
				deadline := time.After(5 * time.Second)
				tick := time.NewTicker(5 * time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case err := <-done:
						t.Fatalf("copy exited before readiness: %v", err)
					case <-deadline:
						t.Fatal("copy did not become ready")
					case <-tick.C:
						if _, err := os.Stat(complete); err == nil {
							if _, err := os.Stat(held); err == nil {
								return
							}
						}
					}
				}
			}
			stop := func(cmd *exec.Cmd, done <-chan error) {
				t.Helper()
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("helper did not stop")
				}
			}
			cmd, done := start(failure)
			if failure != "" {
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("failed copy reported success")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("failed copy hung")
				}
				if _, err := os.Stat(complete); !os.IsNotExist(err) {
					t.Fatal("failed copy published completion")
				}
				message, err := os.ReadFile(filepath.Join(root, "termination-log"))
				if err != nil || strings.TrimSpace(string(message)) != sidecarCopyFailureMessage {
					t.Fatal("missing copy failure diagnostic")
				}
				// A failed copy remains failed on restart. A fresh task Pod
				// gets new volumes; recreate only the control volume here.
				_, retry := start("")
				select {
				case err := <-retry:
					if err == nil {
						t.Fatal("failed copy was retried")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("failed copy restart hung")
				}
				if err := os.Remove(filepath.Join(state, "failed")); err != nil {
					t.Fatal(err)
				}
				cmd, done = start("")
			}
			waitReady(done)
			if _, err := os.Stat(filepath.Join(target, ".partial")); !os.IsNotExist(err) {
				t.Fatal("partial copy was not cleared")
			}
			if data, err := os.ReadFile(filepath.Join(target, "payload")); err != nil || string(data) != "complete data\n" {
				t.Fatalf("wrong copied payload: %q %v", data, err)
			}
			stop(cmd, done)
			// The consumer may change a writable payload after startup. A helper
			// restart must preserve it, even if the source is no longer available.
			write(filepath.Join(target, "payload"), "changed by task\n")
			if err := os.RemoveAll(source); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(held); err != nil {
				t.Fatal(err)
			}
			cmd, done = start("producer")
			waitReady(done)
			stop(cmd, done)
			if data, err := os.ReadFile(filepath.Join(target, "payload")); err != nil || string(data) != "changed by task\n" {
				t.Fatal("restart overwrote task data")
			}
		})
	}
}

func TestCopyReadinessFailureIsReportedAndCleanedUp(t *testing.T) {
	for _, noCleanup := range []bool{false, true} {
		t.Run(fmt.Sprint(noCleanup), func(t *testing.T) {
			client := fake.NewSimpleClientset()
			jobs := watch.NewRaceFreeFake()
			pods := watch.NewRaceFreeFake()
			defer jobs.Stop()
			defer pods.Stop()
			var job *batchv1.Job
			client.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
				job = a.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
				return false, nil, nil
			})
			client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) { return true, jobs, nil })
			client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "copy-test", Namespace: "agents"}, Spec: job.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodPending}}
				// LastTerminationState catches failures even after the kubelet restarts
				// the helper and the failed process is no longer in State.Terminated.
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: kubernetesSidecarInitPrefix + "0", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 42, Message: sidecarCopyFailureMessage}}}}
				pods.Add(pod)
				return true, pods, nil
			})
			backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents", SidecarCopyReadiness: true, NoCleanup: noCleanup}, clientset: client}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result := backend.ExecuteTask(ctx, &TaskParams{TaskID: "copy-test", DockerImage: "ubuntu:22.04", Sidecars: []types.SidecarMount{{Image: "sidecar:test", MountPath: "/agent"}}})
			assertBackendFailureReason(t, result.Error, metrics.TaskFailureReasonSidecarPrep)
			remaining, err := client.BatchV1().Jobs("agents").List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if (len(remaining.Items) == 1) != noCleanup {
				t.Fatal("failed copy Job cleanup did not honor no_cleanup")
			}
		})
	}
}
