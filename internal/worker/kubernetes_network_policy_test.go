package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/config"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"golang.org/x/sync/semaphore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestNetworkPolicyAssignmentJSON(t *testing.T) {
	for _, field := range []string{"", `,"network_policy":null`, `,"network_policy":{}`, `,"network_policy":{"rules":[]}`, `,"network_policy":{"rules":null}`} {
		var assignment types.TaskAssignmentMessage
		if err := json.Unmarshal([]byte(`{"task_id":"run"`+field+`}`), &assignment); err != nil {
			t.Fatal(err)
		}
		wantPresent := field != "" && field != `,"network_policy":null`
		if (assignment.NetworkPolicy != nil) != wantPresent {
			t.Fatalf("policy presence for %s = %v", field, assignment.NetworkPolicy)
		}
		encoded, err := json.Marshal(assignment)
		if err != nil || strings.Contains(string(encoded), `"network_policy"`) != wantPresent {
			t.Fatalf("wire presence changed: %s %v", encoded, err)
		}
	}
	policy := &types.NetworkPolicy{Rules: []types.NetworkPolicyRule{{Action: "ALLOW", Hosts: []string{"example.com", "192.0.2.1", "2001:db8::1"}, Description: "preserved"}}}
	raw, err := json.Marshal(types.TaskAssignmentMessage{NetworkPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	var assignment types.TaskAssignmentMessage
	if err := json.Unmarshal(raw, &assignment); err != nil || !reflect.DeepEqual(policy, assignment.NetworkPolicy) || !strings.Contains(string(raw), `"action":"ALLOW"`) {
		t.Fatalf("wire contract: %s %v", raw, err)
	}
}

func TestNetworkPolicyPreclaimRejection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend Backend
		policy  *types.NetworkPolicy
		code    string
	}{
		{"unsupported", &recordingBackend{}, &types.NetworkPolicy{}, "network_policy_unsupported"},
		{"disabled", &KubernetesBackend{}, &types.NetworkPolicy{}, "network_policy_unsupported"},
		{"unknown action", &recordingBackend{}, allowNetworkPolicy("example.com"), "network_policy_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "unknown action" {
				tc.policy.Rules[0].Action = "DENY"
			}
			w := &Worker{ctx: context.Background(), config: Config{OneShot: true}, backend: tc.backend,
				outbound: newOutboundQueue(4), oneShot: newOneShotState(), taskSemaphore: semaphore.NewWeighted(1)}
			w.handleTaskAssignment(&types.TaskAssignmentMessage{TaskID: "run", ExecutionID: "execution", NetworkPolicy: tc.policy}, time.Now())
			messages := drainMessages(t, w.outbound.messages)
			if len(messages) != 1 || messages[0].Type != types.MessageTypeTaskRejected {
				t.Fatalf("messages=%v", messages)
			}
			var rejected types.TaskRejectedMessage
			if err := json.Unmarshal(messages[0].Data, &rejected); err != nil || rejected.Code != tc.code || rejected.ExecutionID != "execution" {
				t.Fatalf("rejection=%+v err=%v", rejected, err)
			}
			if !w.oneShot.tryAccept() || !w.taskSemaphore.TryAcquire(1) {
				t.Fatal("rejected policy consumed one-shot acceptance or a concurrency slot")
			}
			w.taskSemaphore.Release(1)
		})
	}
}

func TestNetworkPolicyHostValidation(t *testing.T) {
	for _, host := range []string{"*", "*.example.com", "https://example.com", "192.0.2.0/24", "Example.com", "example.com.", "fe80::1%eth0", " bad", "-bad.example", "example..com"} {
		if err := validateNetworkPolicy(allowNetworkPolicy(host)); err == nil {
			t.Errorf("accepted invalid host %q", host)
		}
	}
	if err := validateNetworkPolicy(&types.NetworkPolicy{Rules: []types.NetworkPolicyRule{{Action: "ALLOW"}}}); err == nil {
		t.Fatal("accepted empty hosts")
	}
}

func TestNetworkPolicyDNSConfiguration(t *testing.T) {
	cfg, err := effectiveNetworkPolicyDNS(config.NetworkPolicyDNSConfig{}, true, []byte("search cluster.local\nnameserver 10.96.0.10\nnameserver 2001:db8::53\nnameserver 10.96.0.10\n"))
	if err != nil || !reflect.DeepEqual(cfg.ServerIPs, []string{"10.96.0.10", "2001:db8::53"}) ||
		cfg.Namespace != "kube-system" || cfg.PodLabels["k8s-app"] != "kube-dns" {
		t.Fatalf("discovery: %+v %v", cfg, err)
	}
	override := config.NetworkPolicyDNSConfig{ServerIPs: []string{"192.0.2.53"}, Namespace: "dns", PodLabels: map[string]string{"app": "resolver"}}
	got, err := effectiveNetworkPolicyDNS(override, false, []byte("nameserver 127.0.0.1"))
	if err != nil || !reflect.DeepEqual(got, override) {
		t.Fatalf("explicit override: %+v %v", got, err)
	}
	for _, tc := range []struct {
		inCluster bool
		resolv    string
		ips       []string
	}{
		{false, "nameserver 10.96.0.10", nil},
		{true, "", nil},
		{true, "nameserver invalid", nil},
		{true, "nameserver 127.0.0.1", nil},
		{true, "nameserver ::", nil},
		{false, "", []string{"fe80::1%eth0"}},
	} {
		if _, err := effectiveNetworkPolicyDNS(config.NetworkPolicyDNSConfig{ServerIPs: tc.ips}, tc.inCluster, []byte(tc.resolv)); err == nil {
			t.Errorf("accepted unusable DNS configuration: %+v", tc)
		}
	}
}

func allowNetworkPolicy(hosts ...string) *types.NetworkPolicy {
	return &types.NetworkPolicy{Rules: []types.NetworkPolicyRule{{Action: "ALLOW", Hosts: hosts}}}
}

type fixedNetworkResolver struct {
	ips   []netip.Addr
	err   error
	calls int
}

func (r *fixedNetworkResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.calls++
	return r.ips, r.err
}

func compileTestNetworkPolicy(t *testing.T, policy *types.NetworkPolicy, cilium bool, resolver networkHostResolver) *unstructured.Unstructured {
	t.Helper()
	object, err := compileNetworkPolicy(context.Background(), policy, cilium, config.NetworkPolicyDNSConfig{ServerIPs: []string{"10.96.0.10"}}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func TestNativeNetworkPolicyTranslation(t *testing.T) {
	resolver := &fixedNetworkResolver{ips: []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("192.0.2.1")}}
	object := compileTestNetworkPolicy(t, allowNetworkPolicy("example.com", "example.com", "198.51.100.1"), false, resolver)
	spec, _, _ := unstructured.NestedMap(object.Object, "spec")
	raw, _ := json.Marshal(spec)
	for _, required := range []string{`192.0.2.1/32`, `2001:db8::1/128`, `198.51.100.1/32`, `10.96.0.10/32`, `"protocol":"TCP"`, `"protocol":"UDP"`, `"namespaceSelector"`, `"podSelector"`, `"policyTypes":["Egress"]`} {
		if !strings.Contains(string(raw), required) {
			t.Errorf("missing %s from %s", required, raw)
		}
	}
	if resolver.calls != 1 || strings.Contains(string(raw), `"ingress"`) {
		t.Fatalf("unexpected refresh/ingress: calls=%d spec=%s", resolver.calls, raw)
	}
	for _, r := range []*fixedNetworkResolver{{err: errors.New("DNS failed")}, {}} {
		if _, err := compileNetworkPolicy(context.Background(), allowNetworkPolicy("example.com"), false, config.NetworkPolicyDNSConfig{ServerIPs: []string{"10.96.0.10"}}, r); err == nil {
			t.Fatal("accepted incomplete DNS snapshot")
		}
	}
}

func TestCiliumNetworkPolicyTranslation(t *testing.T) {
	resolver := &fixedNetworkResolver{err: errors.New("must not resolve")}
	object := compileTestNetworkPolicy(t, allowNetworkPolicy("example.com", "192.0.2.1", "2001:db8::1"), true, resolver)
	spec, _, _ := unstructured.NestedMap(object.Object, "spec")
	raw, _ := json.Marshal(spec)
	for _, required := range []string{`"toFQDNs":[{"matchName":"example.com"}]`, `"toCIDR":["192.0.2.1/32","2001:db8::1/128"]`,
		`"rules":{"dns":[{"matchName":"example.com"}]}`, `"enableDefaultDeny":{"egress":true}`} {
		if !strings.Contains(string(raw), required) {
			t.Errorf("missing %s from %s", required, raw)
		}
	}
	if resolver.calls != 0 || strings.Contains(string(raw), "toEntities") || strings.Contains(string(raw), "matchPattern") || strings.Contains(string(raw), "ingress") {
		t.Fatalf("Cilium widened or resolved rules: %s", raw)
	}
	for _, cilium := range []bool{false, true} {
		empty, err := compileNetworkPolicy(context.Background(), &types.NetworkPolicy{}, cilium, config.NetworkPolicyDNSConfig{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		spec, _, _ := unstructured.NestedMap(empty.Object, "spec")
		if grants, present := spec["egress"]; present && len(grants.([]any)) > 0 {
			t.Fatalf("empty policy emitted egress grants: %v", spec)
		}
		if cilium && spec["egress"] == nil {
			t.Fatal("Cilium schema requires an explicit empty egress list")
		}
	}
}

func TestKubernetesNetworkPolicyFingerprint(t *testing.T) {
	a := compileTestNetworkPolicy(t, allowNetworkPolicy("198.51.100.1", "192.0.2.1", "192.0.2.1"), false, nil)
	b := compileTestNetworkPolicy(t, allowNetworkPolicy("192.0.2.1", "198.51.100.1"), false, nil)
	if a.GetName() != b.GetName() || len(a.GetName()) != 60 || len(a.GetLabels()[networkPolicyIDLabel]) != 52 {
		t.Fatalf("unstable fingerprints: %s / %s", a.GetName(), b.GetName())
	}
	for _, cilium := range []bool{false, true} {
		policy := allowNetworkPolicy("example.com")
		resolver := &fixedNetworkResolver{ips: []netip.Addr{netip.MustParseAddr("192.0.2.1")}}
		first := compileTestNetworkPolicy(t, policy, cilium, resolver)
		policy.Rules[0].Description = "non-enforcing"
		second := compileTestNetworkPolicy(t, policy, cilium, resolver)
		if first.GetName() != second.GetName() {
			t.Fatal("description changed digest")
		}
		second.SetNamespace("another")
		second.SetFinalizers([]string{"other/finalizer"})
		second.SetAnnotations(map[string]string{"other": "metadata"})
		expected, _ := canonicalNetworkSpec(first, first.GetLabels()[networkPolicyIDLabel])
		actual, _ := canonicalNetworkSpec(second, second.GetLabels()[networkPolicyIDLabel])
		if string(expected) != string(actual) {
			t.Fatal("metadata changed content")
		}
		resolver.ips = []netip.Addr{netip.MustParseAddr("198.51.100.1")}
		changed := compileTestNetworkPolicy(t, policy, cilium, resolver)
		if (first.GetName() == changed.GetName()) != cilium {
			t.Fatal("native snapshot/Cilium identity mismatch")
		}
		if err := unstructured.SetNestedField(second.Object, "altered", "spec", map[bool]string{false: "podSelector", true: "endpointSelector"}[cilium], "matchLabels", networkPolicyIDLabel); err != nil {
			t.Fatal(err)
		}
		if _, err := canonicalNetworkSpec(second, first.GetLabels()[networkPolicyIDLabel]); err == nil {
			t.Fatal("altered actual selector accepted")
		}
	}
}

func TestKubernetesNetworkPolicyProviderSelection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resources []metav1.APIResource
		err       error
		cilium    bool
		fail      bool
	}{
		{name: "present", resources: []metav1.APIResource{{Name: "ciliumnetworkpolicies", Namespaced: true}}, cilium: true},
		{name: "absent", err: apierrors.NewNotFound(schema.GroupResource{Group: "cilium.io", Resource: "v2"}, "")},
		{name: "resource absent"},
		{name: "denied", err: apierrors.NewForbidden(schema.GroupResource{Resource: "discovery"}, "", errors.New("denied")), fail: true},
		{name: "timeout", err: context.DeadlineExceeded, fail: true},
		{name: "partial", err: errors.New("partial group discovery"), fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset()
			discovery := client.Discovery().(*discoveryfake.FakeDiscovery)
			discovery.Resources = []*metav1.APIResourceList{{GroupVersion: "cilium.io/v2", APIResources: tc.resources}}
			if tc.err != nil {
				discovery.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, tc.err })
			}
			backend := &KubernetesBackend{clientset: client, networkDiscovery: discovery, networkDynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())}
			provider, err := backend.discoverNetworkPolicyProvider()
			if (err != nil) != tc.fail || provider.cilium != tc.cilium {
				t.Fatalf("provider=%+v err=%v", provider, err)
			}
		})
	}
}
