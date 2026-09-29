//go:build integration

package worker

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func TestJobExpirationWithController(t *testing.T) {
	kubeconfig := os.Getenv("OZ_K8S_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set OZ_K8S_TEST_KUBECONFIG to an explicit disposable-cluster kubeconfig")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	namespace, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "oz-expiration-test-"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.CoreV1().Namespaces().Delete(cleanupCtx, namespace.Name, metav1.DeleteOptions{}); err != nil {
			t.Errorf("remove test namespace %s: %v", namespace.Name, err)
		}
	})
	for _, noCleanup := range []bool{false, true} {
		name := "ttl"
		if noCleanup {
			name = "retained"
		}
		t.Run(name, func(t *testing.T) {
			ttl := int32(5)
			backend := &KubernetesBackend{
				config:    KubernetesBackendConfig{Namespace: namespace.Name, NoCleanup: noCleanup, TTLSecondsAfterFinish: &ttl},
				clientset: client,
			}
			zero := int32(0)
			suspended := true
			job, err := client.BatchV1().Jobs(namespace.Name).Create(ctx, &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "abandoned-"},
				Spec: batchv1.JobSpec{
					BackoffLimit: &zero, Suspend: &suspended,
					TTLSecondsAfterFinished: backend.taskJobTTLSecondsAfterFinished(),
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers:    []corev1.Container{{Name: "task", Image: "busybox:1.36", Command: []string{"sleep", "3600"}}},
					}},
				},
			}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.finalizeFailedJob(ctx, job, errors.New("execution abandoned")); err != nil {
				t.Fatal(err)
			}
			err = wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
				current, err := client.BatchV1().Jobs(namespace.Name).Get(ctx, job.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				for _, condition := range current.Status.Conditions {
					if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
						if condition.Reason != "DeadlineExceeded" {
							t.Errorf("failure reason=%s, want DeadlineExceeded", condition.Reason)
						}
						return true, nil
					}
				}
				return false, nil
			})
			if err != nil {
				t.Fatalf("Job did not reach Failed: %v", err)
			}
			pods, err := client.CoreV1().Pods(namespace.Name).List(ctx, metav1.ListOptions{
				LabelSelector: batchv1.ControllerUidLabel + "=" + string(job.UID),
			})
			if err != nil || len(pods.Items) != 0 {
				t.Fatalf("expiration should not have started Pods: pods=%v err=%v", pods, err)
			}
			if noCleanup {
				current, err := client.BatchV1().Jobs(namespace.Name).Get(ctx, job.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if current.Spec.TTLSecondsAfterFinished != nil {
					t.Fatal("NoCleanup must disable the retention TTL")
				}
			} else {
				err = wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
					_, err := client.BatchV1().Jobs(namespace.Name).Get(ctx, job.Name, metav1.GetOptions{})
					if apierrors.IsNotFound(err) {
						return true, nil
					}
					return false, err
				})
				if err != nil {
					t.Fatalf("TTL did not remove failed Job: %v", err)
				}
			}
		})
	}
}
