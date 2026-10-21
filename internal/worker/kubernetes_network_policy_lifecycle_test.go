package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/warpdotdev/oz-agent-worker/internal/config"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type networkPolicyAPIFixture struct {
	mu                 sync.Mutex
	objects            map[string]*unstructured.Unstructured
	version            int
	jobPosts           int
	policyCreates      int
	beforeJobPost      func()
	loseJobResponse    bool
	losePolicyResponse bool
	losePatchResponse  bool
	denyPolicy         bool
	cilium             bool
	server             *httptest.Server
}

func TestKubernetesNetworkPolicyRegistrationRecovery(t *testing.T) {
	fixture, backend := newNetworkPolicyAPIFixture(t)
	provider := networkPolicyProvider{backend: backend}
	intended := compileTestNetworkPolicy(t, &types.NetworkPolicy{}, false, nil)
	fixture.losePolicyResponse = true
	first, err := provider.register(context.Background(), intended.DeepCopy(), testNetworkConsumer("one", "execution"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.losePatchResponse = true
	second, err := provider.register(context.Background(), intended.DeepCopy(), testNetworkConsumer("two", "execution"))
	if err != nil || second.GetUID() != first.GetUID() {
		t.Fatalf("recover registration: %v %v", second, err)
	}
	_, ledger := requireNetworkLedger(t, provider, first.GetName())
	if len(ledger.Consumers) != 2 || fixture.jobPosts != 0 {
		t.Fatalf("registrations=%+v", ledger)
	}
	other := testNetworkConsumer("one", "execution")
	other.Launcher = "replacement-process"
	if _, err := provider.register(context.Background(), intended.DeepCopy(), other); err == nil {
		t.Fatal("replacement launcher reused an ambiguous reservation")
	}
	fixture.mu.Lock()
	fixture.objects["networkpolicies/"+first.GetName()].SetUID("replacement-policy")
	fixture.mu.Unlock()
	if err := provider.bind(context.Background(), first, testNetworkConsumer("one", "execution"), "job-uid"); err == nil {
		t.Fatal("bound against replacement policy UID")
	}
}

func TestKubernetesNetworkPolicyUnsafeLedger(t *testing.T) {
	for _, kind := range []string{"unknown schema", "annotation exhaustion"} {
		t.Run(kind, func(t *testing.T) {
			fixture, backend := newNetworkPolicyAPIFixture(t)
			provider := networkPolicyProvider{backend: backend}
			intended := compileTestNetworkPolicy(t, &types.NetworkPolicy{}, false, nil)
			policy, err := provider.register(context.Background(), intended.DeepCopy(), testNetworkConsumer("one", "execution"))
			if err != nil {
				t.Fatal(err)
			}
			fixture.mu.Lock()
			stored := fixture.objects["networkpolicies/"+policy.GetName()]
			annotations := stored.GetAnnotations()
			if kind == "unknown schema" {
				annotations[networkConsumersAnnotation] = `{"version":99,"consumers":[]}`
			} else {
				size := 0
				for key, value := range annotations {
					size += len(key) + len(value)
				}
				annotations["filler"] = strings.Repeat("x", 256*1024-size-len("filler")-100)
			}
			stored.SetAnnotations(annotations)
			fixture.mu.Unlock()
			_, err = backend.createRestrictedJob(context.Background(), &TaskParams{TaskID: "two", ExecutionID: "execution", NetworkPolicy: &types.NetworkPolicy{}}, testRestrictedJob("two", "execution"))
			if err == nil || fixture.jobPosts != 0 {
				t.Fatalf("unsafe ledger launched: %v POSTs=%d", err, fixture.jobPosts)
			}
			if err := provider.reconcile(context.Background(), policy); kind == "unknown schema" && err == nil {
				t.Fatal("collector accepted unknown schema")
			}
			current, err := provider.get(context.Background(), policy.GetName())
			if err != nil || !slices.Contains(current.GetFinalizers(), networkPolicyFinalizer) {
				t.Fatalf("unsafe ledger lost protection: %v", err)
			}
		})
	}
}

func TestKubernetesNetworkPolicyUnexplainedObjects(t *testing.T) {
	for _, conflict := range []string{"live object without registration", "reused Job name"} {
		t.Run(conflict, func(t *testing.T) {
			fixture, backend := newNetworkPolicyAPIFixture(t)
			params := &TaskParams{TaskID: "run", ExecutionID: "execution", NetworkPolicy: &types.NetworkPolicy{}}
			job, err := backend.createRestrictedJob(context.Background(), params, testRestrictedJob("run", "execution"))
			if err != nil {
				t.Fatal(err)
			}
			provider := networkPolicyProvider{backend: backend}
			intended := compileTestNetworkPolicy(t, params.NetworkPolicy, false, nil)
			policy, ledger := requireNetworkLedger(t, provider, intended.GetName())
			if conflict == "live object without registration" {
				ledger.Consumers = nil
				if err := writeNetworkLedger(policy, ledger); err != nil {
					t.Fatal(err)
				}
				if err := provider.patchMetadata(context.Background(), policy); err != nil {
					t.Fatal(err)
				}
			} else {
				fixture.mu.Lock()
				fixture.objects["jobs/"+job.Name].SetUID("other-job")
				fixture.mu.Unlock()
			}
			if err := provider.reconcile(context.Background(), policy); err == nil {
				t.Fatal("unexplained consumer did not block cleanup")
			}
			requireNetworkLedger(t, provider, policy.GetName())
		})
	}
}

func newNetworkPolicyAPIFixture(t *testing.T) (*networkPolicyAPIFixture, *KubernetesBackend) {
	t.Helper()
	fixture := &networkPolicyAPIFixture{objects: make(map[string]*unstructured.Unstructured)}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serve))
	t.Cleanup(fixture.server.Close)
	cfg := &rest.Config{Host: fixture.server.URL, Timeout: time.Second * 5, ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents", NoCleanup: true, NetworkPolicy: config.NetworkPolicyConfig{Enabled: true}},
		clientset: client, networkDiscovery: client.Discovery()}
	backend.networkDynamic, err = dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, backend
}

func fixtureStatus(w http.ResponseWriter, status int, reason metav1.StatusReason) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Code: int32(status), Reason: reason, Message: string(reason)})
}

func (f *networkPolicyAPIFixture) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/apis/cilium.io/v2" {
		if f.cilium {
			_ = json.NewEncoder(w).Encode(metav1.APIResourceList{GroupVersion: "cilium.io/v2", APIResources: []metav1.APIResource{{Name: "ciliumnetworkpolicies", Namespaced: true}}})
			return
		}
		fixtureStatus(w, 404, metav1.StatusReasonNotFound)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	resourceIndex := slices.Index(parts, "namespaces") + 2
	if resourceIndex < 2 || resourceIndex >= len(parts) {
		fixtureStatus(w, 404, metav1.StatusReasonNotFound)
		return
	}
	resource, name := parts[resourceIndex], ""
	if len(parts) > resourceIndex+1 {
		name = parts[resourceIndex+1]
	}
	body, _ := io.ReadAll(r.Body)
	if r.Method == http.MethodPost && resource == "jobs" && f.beforeJobPost != nil {
		f.beforeJobPost()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := resource + "/" + name
	current := f.objects[key]
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			selector, err := labels.Parse(r.URL.Query().Get("labelSelector"))
			if err != nil {
				fixtureStatus(w, 400, metav1.StatusReasonBadRequest)
				return
			}
			items := []any{}
			for objectKey, object := range f.objects {
				if strings.HasPrefix(objectKey, resource+"/") && selector.Matches(labels.Set(object.GetLabels())) {
					items = append(items, object.Object)
				}
			}
			kind := map[string]string{"pods": "PodList", "jobs": "JobList", "networkpolicies": "NetworkPolicyList", "ciliumnetworkpolicies": "CiliumNetworkPolicyList"}[resource]
			apiVersion := "v1"
			if resource == "jobs" {
				apiVersion = "batch/v1"
			}
			if resource == "networkpolicies" {
				apiVersion = "networking.k8s.io/v1"
			}
			if resource == "ciliumnetworkpolicies" {
				apiVersion = "cilium.io/v2"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": apiVersion, "kind": kind, "items": items})
			return
		}
		if current == nil {
			fixtureStatus(w, 404, metav1.StatusReasonNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(current)
	case http.MethodPost:
		if resource == "networkpolicies" || resource == "ciliumnetworkpolicies" {
			f.policyCreates++
			if f.denyPolicy {
				fixtureStatus(w, 403, metav1.StatusReasonForbidden)
				return
			}
		}
		if resource == "jobs" {
			f.jobPosts++
		}
		object := &unstructured.Unstructured{}
		if err := json.Unmarshal(body, &object.Object); err != nil {
			fixtureStatus(w, 400, metav1.StatusReasonBadRequest)
			return
		}
		key = resource + "/" + object.GetName()
		if f.objects[key] != nil {
			fixtureStatus(w, 409, metav1.StatusReasonAlreadyExists)
			return
		}
		f.version++
		object.SetUID(k8stypes.UID(uuid.NewString()))
		object.SetResourceVersion(strconv.Itoa(f.version))
		f.objects[key] = object
		if resource != "jobs" && f.losePolicyResponse {
			f.losePolicyResponse = false
			fixtureStatus(w, 504, metav1.StatusReasonTimeout)
			return
		}
		if resource == "jobs" && f.loseJobResponse {
			w.Header().Set("Retry-After", "0")
			fixtureStatus(w, 504, metav1.StatusReasonTimeout)
			return
		}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(object)
	case http.MethodPatch:
		var patch unstructured.Unstructured
		_ = json.Unmarshal(body, &patch.Object)
		if current == nil {
			fixtureStatus(w, 404, metav1.StatusReasonNotFound)
			return
		}
		if patch.GetUID() != current.GetUID() || patch.GetResourceVersion() != current.GetResourceVersion() {
			fixtureStatus(w, 409, metav1.StatusReasonConflict)
			return
		}
		raw, _ := json.Marshal(current)
		merged, err := jsonpatch.MergePatch(raw, body)
		if err != nil {
			fixtureStatus(w, 400, metav1.StatusReasonBadRequest)
			return
		}
		object := &unstructured.Unstructured{}
		_ = json.Unmarshal(merged, &object.Object)
		f.version++
		object.SetResourceVersion(strconv.Itoa(f.version))
		f.objects[key] = object
		if object.GetDeletionTimestamp() != nil && len(object.GetFinalizers()) == 0 {
			delete(f.objects, key)
		}
		if f.losePatchResponse {
			f.losePatchResponse = false
			fixtureStatus(w, 504, metav1.StatusReasonTimeout)
			return
		}
		_ = json.NewEncoder(w).Encode(object)
	case http.MethodDelete:
		if current == nil {
			fixtureStatus(w, 404, metav1.StatusReasonNotFound)
			return
		}
		var options metav1.DeleteOptions
		_ = json.Unmarshal(body, &options)
		if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != current.GetUID() ||
			(resource != "jobs" && (options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != current.GetResourceVersion())) {
			fixtureStatus(w, 409, metav1.StatusReasonConflict)
			return
		}
		f.version++
		if len(current.GetFinalizers()) == 0 {
			delete(f.objects, key)
		} else {
			now := metav1.Now()
			current.SetDeletionTimestamp(&now)
			current.SetResourceVersion(strconv.Itoa(f.version))
		}
		_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Success"})
	}
}

func testRestrictedJob(run, execution string) *batchv1.Job {
	zero := int32(0)
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: kubernetesTaskJobName(run, execution), Namespace: "agents"},
		Spec: batchv1.JobSpec{BackoffLimit: &zero, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "task", Image: "busybox:1.36"}},
		}}}}
}

func testNetworkConsumer(run, execution string) networkPolicyConsumer {
	return networkPolicyConsumer{RunID: run, ExecutionID: execution, JobName: kubernetesTaskJobName(run, execution), Launcher: networkLauncherIncarnation}
}

func requireNetworkLedger(t *testing.T, provider networkPolicyProvider, name string) (*unstructured.Unstructured, networkPolicyLedger) {
	t.Helper()
	object, err := provider.get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := validateManagedNetworkPolicy(object)
	if err != nil {
		t.Fatal(err)
	}
	return object, ledger
}

func TestKubernetesRestrictedLaunchOrdering(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprint("ambiguous=", ambiguous), func(t *testing.T) {
			fixture, backend := newNetworkPolicyAPIFixture(t)
			fixture.loseJobResponse = ambiguous
			provider := networkPolicyProvider{backend: backend}
			intended := compileTestNetworkPolicy(t, &types.NetworkPolicy{}, false, nil)
			fixture.beforeJobPost = func() {
				object, ledger := requireNetworkLedger(t, provider, intended.GetName())
				if len(ledger.Consumers) != 1 || ledger.Consumers[0].JobUID != "" || object.GetDeletionTimestamp() != nil {
					t.Error("Job POST preceded durable protected registration")
				}
			}
			params := &TaskParams{TaskID: "run", ExecutionID: "execution", NetworkPolicy: &types.NetworkPolicy{}}
			job, err := backend.createRestrictedJob(context.Background(), params, testRestrictedJob("run", "execution"))
			if err != nil || job.UID == "" || (job.Spec.Suspend != nil && *job.Spec.Suspend) {
				t.Fatalf("launch: %v %v", job, err)
			}
			_, ledger := requireNetworkLedger(t, provider, intended.GetName())
			if len(ledger.Consumers) != 1 || ledger.Consumers[0].JobUID != job.UID || fixture.jobPosts != 1 {
				t.Fatalf("binding=%+v POSTs=%d", ledger, fixture.jobPosts)
			}
			if _, err := backend.createRestrictedJob(context.Background(), params, testRestrictedJob("run", "execution")); err == nil || fixture.jobPosts != 1 {
				t.Fatal("duplicate registration authorized another POST")
			}
		})
	}
	for _, cilium := range []bool{false, true} {
		t.Run(fmt.Sprintf("policy denied cilium=%v", cilium), func(t *testing.T) {
			fixture, backend := newNetworkPolicyAPIFixture(t)
			fixture.denyPolicy = true
			fixture.cilium = cilium
			_, err := backend.createRestrictedJob(context.Background(), &TaskParams{TaskID: "run", ExecutionID: "execution", NetworkPolicy: &types.NetworkPolicy{}}, testRestrictedJob("run", "execution"))
			if err == nil || fixture.jobPosts != 0 || fixture.policyCreates != 1 {
				t.Fatal("policy failure launched a Job")
			}
		})
	}
}

func TestKubernetesNetworkPolicySharedReuse(t *testing.T) {
	fixture, backend := newNetworkPolicyAPIFixture(t)
	provider := networkPolicyProvider{backend: backend}
	intended := compileTestNetworkPolicy(t, allowNetworkPolicy("192.0.2.1"), false, nil)
	var wg sync.WaitGroup
	for _, run := range []string{"one", "two"} {
		wg.Go(func() {
			if _, err := provider.register(context.Background(), intended.DeepCopy(), testNetworkConsumer(run, "execution")); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	object, ledger := requireNetworkLedger(t, provider, intended.GetName())
	if len(ledger.Consumers) != 2 || len(object.GetOwnerReferences()) != 0 {
		t.Fatalf("shared ledger=%+v", ledger)
	}
	fixture.mu.Lock()
	count := len(fixture.objects)
	fixture.mu.Unlock()
	if count != 1 {
		t.Fatalf("created %d policies", count)
	}
	for _, mutation := range []string{"spec", "selector", "finalizer", "ledger"} {
		t.Run(mutation, func(t *testing.T) {
			modified := object.DeepCopy()
			switch mutation {
			case "spec":
				_ = unstructured.SetNestedSlice(modified.Object, []any{map[string]any{}}, "spec", "egress")
			case "selector":
				_ = unstructured.SetNestedField(modified.Object, "wrong", "spec", "podSelector", "matchLabels", networkPolicyIDLabel)
			case "finalizer":
				modified.SetFinalizers(nil)
			case "ledger":
				annotations := modified.GetAnnotations()
				annotations[networkConsumersAnnotation] = `{"version":99}`
				modified.SetAnnotations(annotations)
			}
			if _, err := validateMatchingNetworkPolicy(modified, intended); err == nil {
				t.Fatal("unsafe existing policy accepted")
			}
		})
	}
}

func TestKubernetesNetworkPolicyRegistrationFinalizerRace(t *testing.T) {
	_, backend := newNetworkPolicyAPIFixture(t)
	provider := networkPolicyProvider{backend: backend}
	intended := compileTestNetworkPolicy(t, &types.NetworkPolicy{}, false, nil)
	first := testNetworkConsumer("one", "execution")
	object, err := provider.register(context.Background(), intended.DeepCopy(), first)
	if err != nil {
		t.Fatal(err)
	}
	ledger, _ := readNetworkLedger(object)
	ledger.Consumers = nil
	if err := writeNetworkLedger(object, ledger); err != nil {
		t.Fatal(err)
	}
	if err := provider.patchMetadata(context.Background(), object); err != nil {
		t.Fatal(err)
	}
	empty, err := provider.get(context.Background(), object.GetName())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := provider.register(context.Background(), intended.DeepCopy(), testNetworkConsumer("two", "execution"))
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.delete(context.Background(), empty); !apierrors.IsConflict(err) {
		t.Fatalf("stale collector DELETE=%v", err)
	}
	latest, err := provider.get(context.Background(), pending.GetName())
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.delete(context.Background(), latest); err != nil {
		t.Fatal(err)
	}
	if err := provider.reconcile(context.Background(), latest); err != nil {
		t.Fatal(err)
	}
	terminating, ledger := requireNetworkLedger(t, provider, latest.GetName())
	if terminating.GetDeletionTimestamp() == nil || len(ledger.Consumers) != 1 {
		t.Fatal("pending launch did not retain terminating policy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := provider.register(ctx, intended.DeepCopy(), testNetworkConsumer("three", "execution")); err == nil {
		t.Fatal("joined terminating UID")
	}
	// The earlier registration still authorizes its one delayed Job POST.
	job := testRestrictedJob("two", "execution")
	applyNetworkExecutionMetadata(job, ledger.Consumers[0], terminating)
	applyNetworkExecutionMetadata(&job.Spec.Template, ledger.Consumers[0], terminating)
	created, err := backend.clientset.BatchV1().Jobs("agents").Create(context.Background(), job, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.bind(context.Background(), terminating, ledger.Consumers[0], created.UID); err != nil {
		t.Fatal(err)
	}
	if err := provider.reconcile(context.Background(), terminating); err != nil {
		t.Fatal(err)
	}
	requireNetworkLedger(t, provider, terminating.GetName())
}

func TestKubernetesNetworkPolicyCleanupAndRecovery(t *testing.T) {
	fixture, backend := newNetworkPolicyAPIFixture(t)
	provider := networkPolicyProvider{backend: backend}
	intended := compileTestNetworkPolicy(t, &types.NetworkPolicy{}, false, nil)
	entry := testNetworkConsumer("run", "execution")
	policy, err := provider.register(context.Background(), intended.DeepCopy(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.reconcile(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	_, ledger := requireNetworkLedger(t, provider, policy.GetName())
	if len(ledger.Consumers) != 1 || ledger.Consumers[0].JobUID != "" {
		t.Fatal("missing Job expired reservation")
	}
	job := testRestrictedJob("run", "execution")
	applyNetworkExecutionMetadata(job, entry, policy)
	applyNetworkExecutionMetadata(&job.Spec.Template, entry, policy)
	created, err := backend.clientset.BatchV1().Jobs("agents").Create(context.Background(), job, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.reconcile(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	_, ledger = requireNetworkLedger(t, provider, policy.GetName())
	if ledger.Consumers[0].JobUID != created.UID {
		t.Fatal("replacement did not recover matching Job UID")
	}
	fixture.mu.Lock()
	stored := fixture.objects["jobs/"+job.Name]
	_ = unstructured.SetNestedSlice(stored.Object, []any{map[string]any{"type": "Complete", "status": "True"}}, "status", "conditions")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "agents",
		Labels:          map[string]string{batchv1.ControllerUidLabel: string(created.UID)},
		OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: job.Name, UID: created.UID, Controller: ptrTrue()}}},
		Spec: job.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	applyNetworkExecutionMetadata(pod, entry, policy)
	raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
	fixture.objects["pods/pod"] = &unstructured.Unstructured{Object: raw}
	fixture.mu.Unlock()
	if err := provider.reconcile(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	requireNetworkLedger(t, provider, policy.GetName())
	fixture.mu.Lock()
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "task", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
	raw, _ = runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
	fixture.objects["pods/pod"] = &unstructured.Unstructured{Object: raw}
	fixture.mu.Unlock()
	if err := provider.reconcile(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.get(context.Background(), policy.GetName()); !apierrors.IsNotFound(err) {
		t.Fatalf("terminal retained Job/Pod kept policy: %v", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.objects["pods/pod"] == nil || fixture.objects["jobs/"+job.Name] == nil {
		t.Fatal("NoCleanup removed inspection objects")
	}
}

func ptrTrue() *bool { value := true; return &value }

func TestKubernetesNetworkPolicyPodTermination(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "task"}}, InitContainers: []corev1.Container{{Name: "sidecar"}},
		EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug"}}}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	terminated := func(name string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{terminated("task")}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{terminated("sidecar")}
	if networkPodStopped(pod) {
		t.Fatal("missing ephemeral status counted stopped")
	}
	pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{terminated("debug")}
	if !networkPodStopped(pod) {
		t.Fatal("all containers stopped")
	}
	pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	if networkPodStopped(pod) {
		t.Fatal("running native sidecar counted stopped")
	}
	pod.Status.InitContainerStatuses[0] = terminated("sidecar")
	pod.Status.Phase = corev1.PodUnknown
	if networkPodStopped(pod) {
		t.Fatal("Unknown phase counted stopped")
	}
}

func TestNetworkPolicyTemplateAndMetadata(t *testing.T) {
	_, backend := newNetworkPolicyAPIFixture(t)
	privileged := true
	security := &corev1.SecurityContext{Privileged: &privileged, Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN", "SYS_ADMIN"}}}
	template := &corev1.PodSpec{HostNetwork: true, DNSPolicy: corev1.DNSNone, DNSConfig: &corev1.PodDNSConfig{Nameservers: []string{"192.0.2.53"}},
		Containers:     []corev1.Container{{Name: "task", SecurityContext: security}},
		InitContainers: []corev1.Container{{Name: "operator-init", Image: "operator-image", SecurityContext: security}}}
	backend.config.PodTemplate = template.DeepCopy()
	spec := backend.buildTaskPodSpec(nil, nil, corev1.Container{Name: "task", Image: "task-image"})
	if !spec.HostNetwork || spec.DNSPolicy != template.DNSPolicy || !reflect.DeepEqual(spec.DNSConfig, template.DNSConfig) ||
		!reflect.DeepEqual(spec.Containers[0].SecurityContext, security) || !reflect.DeepEqual(spec.InitContainers, template.InitContainers) {
		t.Fatalf("operator Pod settings lost: %+v", spec)
	}
	job := testRestrictedJob("run", "execution")
	job.Spec.Template.Spec = spec
	job.Annotations = map[string]string{networkRunAnnotation: "operator", networkPolicyUIDAnnotation: "operator", "other": "preserved", "warp-custom": "preserved"}
	job.Spec.Template.Annotations = copyStringMap(job.Annotations)
	created, err := backend.createRestrictedJob(context.Background(), &TaskParams{TaskID: "run", ExecutionID: "execution", NetworkPolicy: &types.NetworkPolicy{}}, job)
	if err != nil {
		t.Fatal(err)
	}
	for _, meta := range []metav1.Object{created, &created.Spec.Template} {
		if meta.GetAnnotations()[networkRunAnnotation] != "run" || meta.GetAnnotations()[networkExecutionAnnotation] != "execution" ||
			meta.GetAnnotations()[networkPolicyUIDAnnotation] == "" || meta.GetAnnotations()[networkPolicyUIDAnnotation] == "operator" ||
			meta.GetAnnotations()["other"] != "preserved" || meta.GetAnnotations()["warp-custom"] != "preserved" || meta.GetLabels()[networkRestrictedLabel] != "true" {
			t.Fatalf("metadata=%+v %+v", meta.GetLabels(), meta.GetAnnotations())
		}
	}
	if _, err := NewKubernetesBackend(context.Background(), KubernetesBackendConfig{ExtraLabels: map[string]string{"warp-network-restricted": "true"}}); err == nil {
		t.Fatal("reserved operator labels accepted")
	}
}

func TestKubernetesRestrictedLaunchDuplicateCancellation(t *testing.T) {
	fixture, backend := newNetworkPolicyAPIFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	fixture.beforeJobPost = func() { close(entered); <-release }
	params := &TaskParams{TaskID: "run", ExecutionID: "execution", NetworkPolicy: &types.NetworkPolicy{}}
	first := make(chan error, 1)
	go func() {
		_, err := backend.createRestrictedJob(context.Background(), params, testRestrictedJob("run", "execution"))
		first <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := backend.createRestrictedJob(ctx, params, testRestrictedJob("run", "execution"))
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("duplicate wait ignored cancellation: %v", err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if fixture.jobPosts != 1 {
		t.Fatalf("duplicate launched %d requests", fixture.jobPosts)
	}
}
