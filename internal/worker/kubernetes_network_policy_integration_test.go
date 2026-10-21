//go:build integration

package worker

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/config"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type liveNetworkFixture struct {
	backend                                            *KubernetesBackend
	kubeconfig                                         string
	provider                                           networkPolicyProvider
	ctx                                                context.Context
	allowedIP, forbiddenIP, allowedHost, forbiddenHost string
}

func newLiveNetworkFixture(t *testing.T) *liveNetworkFixture {
	t.Helper()
	kubeconfig := os.Getenv("OZ_K8S_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set OZ_K8S_TEST_KUBECONFIG to an explicit disposable-cluster kubeconfig")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Fatal("kubectl is required for live container probes")
	}
	required := []string{"OZ_K8S_TEST_CNI_VERSION", "OZ_K8S_TEST_ALLOWED_IP", "OZ_K8S_TEST_FORBIDDEN_IP", "OZ_K8S_TEST_ALLOWED_HOST", "OZ_K8S_TEST_FORBIDDEN_HOST", "OZ_K8S_TEST_DNS_IPS"}
	for _, name := range required {
		if os.Getenv(name) == "" {
			t.Skipf("set %s for the controlled external endpoint/DNS fixture; API acceptance is not CNI proof", name)
		}
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Timeout = 30 * time.Second
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	namespace, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "oz-network-test-"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.CoreV1().Namespaces().Delete(cleanupCtx, namespace.Name, metav1.DeleteOptions{}); err != nil {
			t.Errorf("delete disposable namespace: %v", err)
		}
	})
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	backend := &KubernetesBackend{clientset: client, networkDiscovery: client.Discovery(), networkDynamic: dynamicClient,
		config: KubernetesBackendConfig{Namespace: namespace.Name, NoCleanup: true, NetworkPolicy: config.NetworkPolicyConfig{Enabled: true,
			DNS: config.NetworkPolicyDNSConfig{ServerIPs: strings.Split(os.Getenv("OZ_K8S_TEST_DNS_IPS"), ",")}}}}
	provider, err := backend.discoverNetworkPolicyProvider()
	if err != nil {
		t.Fatal(err)
	}
	wantProvider := os.Getenv("OZ_K8S_TEST_PROVIDER")
	if wantProvider != "native" && wantProvider != "cilium" {
		t.Fatal("set OZ_K8S_TEST_PROVIDER to native or cilium")
	}
	if provider.cilium != (wantProvider == "cilium") {
		t.Fatalf("selected provider=%v, expected %s", provider.cilium, wantProvider)
	}
	f := &liveNetworkFixture{backend: backend, kubeconfig: kubeconfig, provider: provider, ctx: ctx,
		allowedIP: os.Getenv("OZ_K8S_TEST_ALLOWED_IP"), forbiddenIP: os.Getenv("OZ_K8S_TEST_FORBIDDEN_IP"),
		allowedHost: os.Getenv("OZ_K8S_TEST_ALLOWED_HOST"), forbiddenHost: os.Getenv("OZ_K8S_TEST_FORBIDDEN_HOST")}
	for _, ip := range []string{f.allowedIP, f.forbiddenIP} {
		if _, err := netip.ParseAddr(ip); err != nil {
			t.Fatal(err)
		}
	}
	if f.allowedIP == f.forbiddenIP {
		t.Fatal("controlled endpoints must have distinct addresses")
	}
	if err := validateNetworkPolicy(allowNetworkPolicy(f.allowedHost, f.forbiddenHost)); err != nil {
		t.Fatal(err)
	}
	version, err := client.Discovery().ServerVersion()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Kubernetes=%s CNI=%s provider=%s allowed=%s forbidden=%s", version, os.Getenv("OZ_K8S_TEST_CNI_VERSION"), wantProvider, f.allowedIP, f.forbiddenIP)
	_, err = client.NetworkingV1().NetworkPolicies(namespace.Name).Create(ctx, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "restricted-baseline"},
		Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{networkRestrictedLabel: "true"}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, Egress: []networkingv1.NetworkPolicyEgressRule{}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *liveNetworkFixture) probeScript(allow bool) string {
	fetch := func(host string) string { return fmt.Sprintf("wget -q -T 3 -O /dev/null 'http://%s:8080/'", host) }
	var script []string
	if allow {
		script = append(script, fetch(f.allowedHost)+" || exit 12", fetch(f.allowedIP)+" || exit 11")
	} else {
		script = append(script, fetch(f.allowedIP)+" && exit 13")
	}
	script = append(script, fetch(f.forbiddenIP)+" && exit 14", fetch(f.forbiddenHost)+" && exit 15", "exit 0")
	return strings.Join(script, "\n")
}

func (f *liveNetworkFixture) job(run string, probe string, hold bool) *batchv1.Job {
	job := testRestrictedJob(run, "execution")
	job.Namespace = f.backend.config.Namespace
	command := probe
	if hold {
		command = strings.TrimSuffix(probe, "exit 0") + "while [ ! -f /tmp/finish ]; do sleep 1; done"
	}
	job.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "setup", Image: "busybox:1.36", Command: []string{"sh", "-c", probe}}}
	job.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", command}
	return job
}

func (f *liveNetworkFixture) awaitPod(t *testing.T, job *batchv1.Job) *corev1.Pod {
	t.Helper()
	var result *corev1.Pod
	err := wait.PollUntilContextTimeout(f.ctx, 250*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		pods, err := f.backend.clientset.CoreV1().Pods(f.backend.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: batchv1.ControllerUidLabel + "=" + string(job.UID)})
		if err != nil {
			return false, err
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Status.Phase == corev1.PodFailed {
				return false, fmt.Errorf("probe failed: Pod %s status %+v", pod.Name, pod.Status)
			}
			if podReady(pod) || pod.Status.Phase == corev1.PodSucceeded {
				result = pod
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *liveNetworkFixture) exec(t *testing.T, pod *corev1.Pod, script string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", f.kubeconfig, "--namespace", f.backend.config.Namespace, "exec", pod.Name, "-c", "task", "--", "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("probe exec: %v %s", err, output)
	}
}

func (f *liveNetworkFixture) awaitComplete(t *testing.T, job *batchv1.Job) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(f.ctx, 250*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		current, err := f.backend.getTaskJob(ctx, job.Name)
		if err != nil {
			return false, err
		}
		if jobFailed(current) {
			return false, fmt.Errorf("probe Job failed: %+v", current.Status)
		}
		return jobComplete(current), nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestKubernetesNetworkPolicyWithCNI(t *testing.T) {
	f := newLiveNetworkFixture(t)
	// Establish both external endpoints are reachable without restrictions.
	control := f.job("control", fmt.Sprintf("wget -q -T 3 -O /dev/null http://%s:8080/ && wget -q -T 3 -O /dev/null http://%s:8080/", f.allowedIP, f.forbiddenIP), false)
	control, err := f.backend.clientset.BatchV1().Jobs(f.backend.config.Namespace).Create(f.ctx, control, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.awaitComplete(t, control)
	if control.Labels[networkRestrictedLabel] != "" {
		t.Fatal("unrestricted control acquired reserved selector")
	}
	baseline := f.job("baseline", f.probeScript(false), false)
	baseline.Spec.Template.Labels = map[string]string{networkRestrictedLabel: "true"}
	baseline, err = f.backend.clientset.BatchV1().Jobs(f.backend.config.Namespace).Create(f.ctx, baseline, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.awaitComplete(t, baseline)
	emptyProbe := strings.TrimSuffix(f.probeScript(false), "exit 0") + fmt.Sprintf("timeout 4 nslookup '%s' '%s' && exit 16\nexit 0", f.allowedHost, strings.Split(os.Getenv("OZ_K8S_TEST_DNS_IPS"), ",")[0])
	empty, err := f.backend.createRestrictedJob(f.ctx, &TaskParams{TaskID: "empty", ExecutionID: "execution", NetworkPolicy: &types.NetworkPolicy{}}, f.job("empty", emptyProbe, false))
	if err != nil {
		t.Fatal(err)
	}
	f.awaitComplete(t, empty)
	emptyPolicies, err := f.provider.list(f.ctx)
	if err != nil || len(emptyPolicies) != 1 {
		t.Fatalf("empty policy: %v %v", emptyPolicies, err)
	}
	if err := f.provider.reconcile(f.ctx, &emptyPolicies[0]); err != nil {
		t.Fatal(err)
	}
	policy := allowNetworkPolicy(f.allowedHost)
	var jobs []*batchv1.Job
	var pods []*corev1.Pod
	for _, run := range []string{"one", "two"} {
		job, err := f.backend.createRestrictedJob(f.ctx, &TaskParams{TaskID: run, ExecutionID: "execution", NetworkPolicy: policy}, f.job(run, f.probeScript(true), true))
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, job)
		pods = append(pods, f.awaitPod(t, job))
	}
	objects, err := f.provider.list(f.ctx)
	if err != nil || len(objects) != 1 {
		t.Fatalf("expected shared policy: %v %v", objects, err)
	}
	original := objects[0].DeepCopy()
	_, ledger := requireNetworkLedger(t, f.provider, original.GetName())
	if len(ledger.Consumers) != 2 {
		t.Fatalf("consumers=%+v", ledger)
	}
	for i, job := range jobs {
		f.exec(t, pods[i], "touch /tmp/finish")
		f.awaitComplete(t, job)
		if err := f.provider.reconcile(f.ctx, original); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			f.exec(t, pods[1], f.probeScript(true))
			_, remaining := requireNetworkLedger(t, f.provider, original.GetName())
			if len(remaining.Consumers) != 1 {
				t.Fatal("released active consumer")
			}
		}
	}
	if _, err := f.provider.get(f.ctx, original.GetName()); !apierrors.IsNotFound(err) {
		t.Fatalf("policy retained after stopped executions: %v", err)
	}
	for _, job := range jobs {
		if _, err := f.backend.getTaskJob(f.ctx, job.Name); err != nil {
			t.Fatalf("NoCleanup lost terminal Job: %v", err)
		}
	}
	t.Log("shared enforcement, setup/main probes, retained Jobs and zero-consumer deletion passed")
}

func TestKubernetesNetworkPolicyTerminatingWithCNI(t *testing.T) {
	f := newLiveNetworkFixture(t)
	job, err := f.backend.createRestrictedJob(f.ctx, &TaskParams{TaskID: "terminating", ExecutionID: "execution", NetworkPolicy: allowNetworkPolicy(f.allowedHost)}, f.job("terminating", f.probeScript(true), true))
	if err != nil {
		t.Fatal(err)
	}
	pod := f.awaitPod(t, job)
	objects, err := f.provider.list(f.ctx)
	if err != nil || len(objects) != 1 {
		t.Fatalf("policy: %v %v", objects, err)
	}
	policy := objects[0].DeepCopy()
	pending := testNetworkConsumer("delayed", "execution")
	intended, err := compileNetworkPolicy(f.ctx, allowNetworkPolicy(f.allowedHost), f.provider.cilium, f.backend.config.NetworkPolicy.DNS, networkDNSResolver(f.backend.config.NetworkPolicy.DNS.ServerIPs))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider.register(f.ctx, intended, pending); err != nil {
		t.Fatal(err)
	}
	policy, err = f.provider.get(f.ctx, policy.GetName())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.provider.delete(f.ctx, policy); err != nil {
		t.Fatal(err)
	}
	f.exec(t, pod, f.probeScript(true))
	if err := f.provider.reconcile(f.ctx, policy); err != nil {
		t.Fatal(err)
	}
	delayed := f.job("delayed", f.probeScript(true), true)
	applyNetworkExecutionMetadata(delayed, pending, policy)
	applyNetworkExecutionMetadata(&delayed.Spec.Template, pending, policy)
	delayed, err = f.backend.clientset.BatchV1().Jobs(f.backend.config.Namespace).Create(f.ctx, delayed, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	delayedPod := f.awaitPod(t, delayed)
	if err := f.provider.bind(f.ctx, policy, pending, delayed.UID); err != nil {
		t.Fatal(err)
	}
	current, err := f.provider.get(f.ctx, policy.GetName())
	if err != nil || current.GetUID() != policy.GetUID() || current.GetDeletionTimestamp() == nil {
		t.Fatalf("terminating UID lost: %v %v", current, err)
	}
	want, _, _ := unstructured.NestedMap(policy.Object, "spec")
	got, _, _ := unstructured.NestedMap(current.Object, "spec")
	if !reflect.DeepEqual(want, got) {
		t.Fatal("terminating policy spec changed")
	}
	cniNS, selector := os.Getenv("OZ_K8S_TEST_CNI_NAMESPACE"), os.Getenv("OZ_K8S_TEST_CNI_POD_SELECTOR")
	if cniNS == "" || selector == "" {
		t.Fatal("CNI restart proof requires OZ_K8S_TEST_CNI_NAMESPACE and OZ_K8S_TEST_CNI_POD_SELECTOR")
	}
	agents, err := f.backend.clientset.CoreV1().Pods(cniNS).List(f.ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil || len(agents.Items) == 0 {
		t.Fatalf("CNI agents: %v %v", agents, err)
	}
	for _, agent := range agents.Items {
		uid := agent.UID
		if err := f.backend.clientset.CoreV1().Pods(cniNS).Delete(f.ctx, agent.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(t, pod, f.probeScript(true))
	if err := wait.PollUntilContextTimeout(f.ctx, time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		current, err := f.backend.clientset.CoreV1().Pods(cniNS).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, err
		}
		if len(current.Items) != len(agents.Items) {
			return false, nil
		}
		for _, agent := range current.Items {
			for _, old := range agents.Items {
				if old.UID == agent.UID {
					return false, nil
				}
			}
			if !podReady(&agent) {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, pod, f.probeScript(true))
	f.exec(t, pod, "touch /tmp/finish")
	f.awaitComplete(t, job)
	if err := f.provider.reconcile(f.ctx, policy); err != nil {
		t.Fatal(err)
	}
	f.exec(t, delayedPod, f.probeScript(true))
	f.exec(t, delayedPod, "touch /tmp/finish")
	f.awaitComplete(t, delayed)
	if err := f.provider.reconcile(f.ctx, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider.get(f.ctx, policy.GetName()); !apierrors.IsNotFound(err) {
		t.Fatalf("terminating policy not finalized: %v", err)
	}
	t.Log("terminating UID retained enforcement before/during/after CNI restart and fresh API reconciliation")
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
